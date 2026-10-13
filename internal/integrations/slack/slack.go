package slackbot

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/filing"
	"reportbot/internal/publish"
	"reportbot/internal/report"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

var delegatedAuthorRegex = regexp.MustCompile(`^\{([^{}]+)\}\s*`)

const (
	listItemsPageSize     = 15
	listScopeMine         = "mine"
	listScopeAll          = "all"
	actionDeleteItem      = "list_items_delete"
	actionEditItemOpen    = "list_items_edit_open"
	actionPagePrev        = "list_items_page_prev"
	actionPageNext        = "list_items_page_next"
	actionRowMenu         = "list_items_row_menu"
	modalEditCallbackID   = "list_items_edit_modal"
	modalDeleteCallbackID = "list_items_delete_modal"
	modalMetaPrefix       = "item:"
	editBlockDescription  = "edit_description"
	editActionDescription = "description_input"
	editBlockStatus       = "edit_status"
	editActionStatus      = "status_input"
	editBlockStatusNote   = "edit_status_note"
	editActionStatusNote  = "status_note_input"
	editBlockSection      = "edit_section"
	editActionSection     = "section_input"

	actionNudgeMember         = "nudge_member"
	actionNudgeAll            = "nudge_all"
	modalNudgeConfirmCallback = "nudge_confirm_modal"
	nudgeMetaPrefix           = "nudge:"

	actionPlacePick      = "place_pick"
	actionPlaceOther     = "place_other"
	actionUpdateConfirm  = "update_confirm"
	actionUpdateReject   = "update_reject"
	actionLinkConfirm    = "link_confirm"
	actionLinkReject     = "link_reject"
	actionPlacementUndo  = "placement_undo"
	actionCheckDone      = "check_done"
	actionPublishAnyway  = "publish_anyway"
	undeterminedOptValue = "0"
)

// wakeFiling starts the filing workers on new jobs; set by StartSlackBot.
var wakeFiling = func() {}

// StartSlackBot runs the Socket Mode loop; wake tells the filing workers new jobs exist.
// p is nil when no LLM is configured.
func StartSlackBot(cfg Config, db *sql.DB, api *slack.Client, wake func(), p Proposer) error {
	if wake != nil {
		wakeFiling = wake
	}
	proposer = p
	client := socketmode.New(api)

	go func() {
		for evt := range client.Events {
			switch evt.Type {
			case socketmode.EventTypeSlashCommand:
				client.Ack(*evt.Request)
				cmd, ok := evt.Data.(slack.SlashCommand)
				if !ok {
					continue
				}
				log.Printf("Slash command received: %s from user=%s channel=%s", cmd.Command, cmd.UserID, cmd.ChannelID)
				go handleSlashCommand(client, api, db, cfg, cmd)
			case socketmode.EventTypeEventsAPI:
				client.Ack(*evt.Request)
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				go handleEventsAPI(api, cfg, eventsAPIEvent)
			case socketmode.EventTypeInteractive:
				client.Ack(*evt.Request)
				callback, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					continue
				}
				go handleInteraction(api, db, cfg, callback)
			}
		}
	}()

	log.Println("Slack bot connected via Socket Mode")
	return client.Run()
}

func handleSlashCommand(client *socketmode.Client, api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	switch cmd.Command {
	case "/report":
		handleReport(api, db, cfg, cmd)
	case "/rpt":
		handleReport(api, db, cfg, cmd)
	case "/fetch":
		handleFetchMRs(api, db, cfg, cmd)
	case "/generate-report":
		handleGenerateReport(api, db, cfg, cmd)
	case "/gen":
		handleGenerateReport(api, db, cfg, cmd)
	case "/list":
		handleListItems(api, db, cfg, cmd)
	case "/check":
		handleListMissing(api, db, cfg, cmd)
	case "/nudge":
		handleTestNudge(api, db, cfg, cmd)
	case "/retrospect":
		handleRetrospective(api, db, cfg, cmd)
	case "/stats":
		handleReportStats(api, db, cfg, cmd)
	case "/help":
		handleHelp(api, cfg, cmd)
	}
}

func handleEventsAPI(api *slack.Client, cfg Config, event slackevents.EventsAPIEvent) {
	if event.Type != slackevents.CallbackEvent {
		return
	}
	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.MemberJoinedChannelEvent:
		handleMemberJoined(api, cfg, ev)
	}
}

func handleMemberJoined(api *slack.Client, cfg Config, ev *slackevents.MemberJoinedChannelEvent) {
	log.Printf("member-joined user=%s channel=%s", ev.User, ev.Channel)

	teamName := cfg.TeamName
	if teamName == "" {
		teamName = "the team"
	}

	intro := fmt.Sprintf("Welcome to %s! I'm ReportAgent — I help track Work Items and file them into the weekly report.\n\n"+
		"Here's how to get started:\n"+
		"• `/report <description> (status)` — Report a Work Item (e.g. `/report Fix login bug (done)`)\n"+
		"• `/list` — View your Work Items on this week's report (`/list all` for the team)\n"+
		"• `/help` — See all available commands\n\n"+
		"You can report several Work Items at once with newlines, and set a shared status on the last line.",
		teamName,
	)

	_, _, err := api.PostMessage(ev.Channel,
		slack.MsgOptionText(intro, false),
		slack.MsgOptionPostEphemeral(ev.User),
	)
	if err != nil {
		log.Printf("member-joined intro error user=%s channel=%s: %v", ev.User, ev.Channel, err)
	}
}

func handleReport(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	text := strings.TrimSpace(cmd.Text)
	if text == "" {
		postEphemeral(api, cmd, "Usage: /report <description> (status)\nExample: /report [ticket_id] Add pagination to user list API (done)\nMultiline (separate items with newlines, e.g. Shift+Enter): /report Item A (in progress)\\nItem B (done)")
		return
	}

	author := cmd.UserName
	if user, err := api.GetUserInfo(cmd.UserID); err == nil {
		if user.Profile.DisplayName != "" {
			author = user.Profile.DisplayName
		} else if user.RealName != "" {
			author = user.RealName
		}
	}

	// Manager-only delegated reporting syntax:
	// /report {Member Name} Description (status)
	reportText := text
	authorID := cmd.UserID
	if match := delegatedAuthorRegex.FindStringSubmatch(text); len(match) > 1 {
		if cfg.IsManagerID(cmd.UserID) {
			delegated := strings.TrimSpace(match[1])
			remaining := strings.TrimSpace(text[len(match[0]):])
			if delegated != "" && remaining != "" {
				resolvedAuthor, ok := resolveDelegatedAuthorName(delegated, cfg.TeamMembers)
				if !ok {
					postEphemeral(api, cmd, fmt.Sprintf("Could not resolve delegated member %q to exactly one team member.", delegated))
					log.Printf("report delegated author unresolved manager=%s delegated=%q", cmd.UserID, delegated)
					return
				}
				// Only author_ids binds an owner; otherwise the item stays manager-editable.
				author, authorID = resolvedAuthor, cfg.AuthorID(resolvedAuthor)
				if isLikelySlackID(resolvedAuthor) {
					authorID = resolvedAuthor
				}
				reportText = remaining
			}
		}
	}

	items, parseErr := parseReportItems(reportText, author, cfg.Location)
	if parseErr != nil {
		postEphemeral(api, cmd, parseErr.Error())
		log.Printf("report parse error user=%s: %v", cmd.UserID, parseErr)
		return
	}
	for i := range items {
		items[i].AuthorID = authorID
	}

	payload := filing.Payload{Kind: filing.KindArrival, ResponseURL: cmd.ResponseURL, ChannelID: cmd.ChannelID, UserID: cmd.UserID, Replace: len(items) == 1}
	if _, err := insertWorkItemsForFiling(db, items, payload.JSON()); err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error saving your Work Items: %v", err))
		log.Printf("report insert error user=%s: %v", cmd.UserID, err)
		return
	}
	saved := "Saved — filing it…"
	if len(items) > 1 {
		saved = fmt.Sprintf("Saved %d Work Items — filing them…", len(items))
	}
	// Reply before waking the workers so "Saved" comes before the filed reply.
	respond(api, cmd.ResponseURL, cmd.ChannelID, cmd.UserID, false, saved, nil)
	wakeFiling()

	notifyManagersOnMemberReport(api, cfg, cmd, author, items)
	log.Printf("report saved user=%s author=%s count=%d", cmd.UserID, author, len(items))
}

func notifyManagersOnMemberReport(api *slack.Client, cfg Config, cmd slack.SlashCommand, author string, items []WorkItem) {
	if len(items) == 0 || len(cfg.ManagerSlackIDs) == 0 {
		return
	}
	if cfg.IsManagerID(cmd.UserID) {
		return
	}

	msg := buildManagerReportNotificationMessage(cmd.ChannelID, author, items)
	seen := make(map[string]bool)
	for _, managerID := range cfg.ManagerSlackIDs {
		managerID = strings.TrimSpace(managerID)
		if managerID == "" || managerID == cmd.UserID || seen[managerID] {
			continue
		}
		seen[managerID] = true

		ch, _, _, err := api.OpenConversation(&slack.OpenConversationParameters{Users: []string{managerID}})
		if err != nil {
			log.Printf("report manager notify open conversation error manager=%s reporter=%s: %v", managerID, cmd.UserID, err)
			continue
		}
		if _, _, err := api.PostMessage(ch.ID, slack.MsgOptionText(msg, false)); err != nil {
			log.Printf("report manager notify post error manager=%s reporter=%s: %v", managerID, cmd.UserID, err)
			continue
		}
	}
}

func buildManagerReportNotificationMessage(channelID, author string, items []WorkItem) string {
	header := fmt.Sprintf("New /report from *%s*:", author)
	if strings.TrimSpace(channelID) != "" {
		header = fmt.Sprintf("New /report from *%s* in <#%s>:", author, channelID)
	}

	msg := header
	const limit = 5
	for i, item := range items {
		if i >= limit {
			msg += fmt.Sprintf("\n• ... and %d more", len(items)-limit)
			break
		}
		msg += fmt.Sprintf("\n• %s (%s)", item.Description, report.StatusText(item))
	}
	return msg
}

func parseReportItems(reportText, author string, loc *time.Location) ([]WorkItem, error) {
	lines := strings.Split(strings.ReplaceAll(reportText, "\r\n", "\n"), "\n")
	trimmed := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			trimmed = append(trimmed, line)
		}
	}
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("Usage: /report <description> (status)")
	}

	sharedStatus := ""
	if len(trimmed) > 1 {
		last := trimmed[len(trimmed)-1]
		if strings.HasPrefix(last, "(") && strings.HasSuffix(last, ")") {
			candidate := strings.TrimSpace(last[1 : len(last)-1])
			if candidate != "" {
				sharedStatus = candidate
				trimmed = trimmed[:len(trimmed)-1]
			}
		}
	}
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("Usage: /report <description> (status)")
	}

	now := time.Now().In(loc)
	items := make([]WorkItem, 0, len(trimmed))
	for _, line := range trimmed {
		description, statusText := filing.SplitStatus(line)
		if statusText == "" {
			statusText = sharedStatus
		}
		if description == "" {
			return nil, fmt.Errorf("Error: empty Work Item line.")
		}
		items = append(items, newReportedItem(description, statusText, author, now))
	}
	return items, nil
}

// newReportedItem maps what code can decide; an empty Status leaves it to the decision model,
// with the reporter's wording kept as the Status Note.
func newReportedItem(description, statusText, author string, at time.Time) WorkItem {
	item := WorkItem{Description: description, Author: author, Source: "slack", ReportedAt: at}
	if status, ok := domain.StatusFromWord(statusText); ok {
		item.Status = status
	} else {
		item.StatusNote = statusText
	}
	if certain, _ := filing.FindTickets(description); len(certain) > 0 {
		item.TicketIDs = strings.Join(certain, ",")
	}
	return item
}

func handleFetchMRs(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	isManager, err := isManagerUser(api, cfg, cmd.UserID)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error checking permissions: %v", err))
		log.Printf("fetch auth error user=%s: %v", cmd.UserID, err)
		return
	}
	if !isManager {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		log.Printf("fetch denied user=%s", cmd.UserID)
		return
	}

	if !cfg.GitLabConfigured() && !cfg.GitHubConfigured() {
		postEphemeral(api, cmd, "Neither GitLab nor GitHub is configured. Set gitlab_sources or github_* config fields.")
		return
	}

	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))

	var sources []string
	for _, src := range cfg.GitLabSources {
		sources = append(sources, "gitlab@"+src.Label())
	}
	if cfg.GitHubConfigured() {
		sources = append(sources, "GitHub")
	}
	postEphemeral(api, cmd, fmt.Sprintf("Fetching MRs/PRs/commits from %s for %s to %s...",
		strings.Join(sources, " + "),
		monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2")))

	result, err := FetchAndImportMRs(cfg, db, FetchHooks(api, cfg))
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error: %v", err))
		log.Printf("fetch error: %v", err)
		return
	}

	msg := FormatFetchSummary(result)
	postEphemeral(api, cmd, msg)
	log.Printf("fetch inserted=%d alreadyTracked=%d skippedNonTeam=%d", result.Inserted, result.AlreadyTracked, result.SkippedNonTeam)
}

func handleTestNudge(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	targetID := cmd.UserID
	targetLabel := "yourself"
	rawTarget := strings.TrimSpace(cmd.Text)

	if rawTarget != "" {
		isManager, err := isManagerUser(api, cfg, cmd.UserID)
		if err != nil {
			postEphemeral(api, cmd, fmt.Sprintf("Error checking permissions: %v", err))
			log.Printf("nudge auth error user=%s: %v", cmd.UserID, err)
			return
		}
		if !isManager {
			postEphemeral(api, cmd, "Usage: /nudge\nManagers can also use `/nudge <member>` to test another member's nudge DM.")
			return
		}

		resolvedID, resolvedLabel, err := resolveNudgeTarget(api, cfg, rawTarget)
		if err != nil {
			postEphemeral(api, cmd, fmt.Sprintf("Unable to resolve nudge target %q: %v", rawTarget, err))
			return
		}
		targetID = resolvedID
		targetLabel = resolvedLabel
	}

	sendNudges(api, db, cfg, []string{targetID}, cfg.ReportChannelID)
	if targetID == cmd.UserID {
		postEphemeral(api, cmd, "Sent a test nudge to your DM.")
		log.Printf("test nudge sent user=%s target=self", cmd.UserID)
		return
	}

	postEphemeral(api, cmd, fmt.Sprintf("Sent a test nudge to %s.", targetLabel))
	log.Printf("test nudge sent user=%s target=%s", cmd.UserID, targetID)
}

func parseGenerateReportArgs(text string) (mode string, sendPrivate bool, err error) {
	mode = "team"
	sendPrivate = false
	modeSet := false

	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	for _, f := range fields {
		switch f {
		case "team", "boss", "post":
			if modeSet && mode != f {
				return "", false, fmt.Errorf("Usage: /generate-report [team|boss|post] [private]\nExamples: /generate-report team, /generate-report boss private, /generate-report post, /gen post private")
			}
			mode = f
			modeSet = true
		case "private":
			sendPrivate = true
		case "channel":
			sendPrivate = false
		default:
			return "", false, fmt.Errorf("Usage: /generate-report [team|boss|post] [private]\nExamples: /generate-report team, /generate-report boss private, /generate-report post, /gen post private")
		}
	}
	return mode, sendPrivate, nil
}

func handleGenerateReport(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	if !cfg.IsManagerID(cmd.UserID) {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		log.Printf("generate-report denied user=%s", cmd.UserID)
		return
	}
	mode, sendPrivate, parseErr := parseGenerateReportArgs(cmd.Text)
	if parseErr != nil {
		postEphemeral(api, cmd, parseErr.Error())
		return
	}
	if mode == "post" {
		postLatestTeamReport(api, cfg, cmd, sendPrivate)
		return
	}
	publishReport(api, db, cfg, cmd.ChannelID, cmd.UserID, mode, sendPrivate, false)
}

// publishReport stores the week's Published Report and uploads the requested version.
// With Undetermined items and no confirmation, it warns instead.
func publishReport(api *slack.Client, db *sql.DB, cfg Config, channelID, userID, mode string, sendPrivate, confirmed bool) {
	report.GenerationMu.Lock()
	defer report.GenerationMu.Unlock()

	live, err := publish.LoadWeek(db, cfg, time.Now())
	if err != nil {
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("Error loading the report: %v", err))
		log.Printf("generate-report load error: %v", err)
		return
	}
	if live.View.ItemCount() == 0 {
		postEphemeralTo(api, channelID, userID, "No Work Items found for this week.")
		return
	}
	if n := len(live.View.Undetermined); n > 0 && !confirmed {
		text := fmt.Sprintf("%d Work Item(s) are still in Undetermined.", n)
		if m := fallbackCount(live.View.Undetermined); m > 0 {
			text += fmt.Sprintf(" Publishing files %d of them under the model's first choice and tells their reporters.", m)
		}
		if link := liveReportLink(cfg); link != "" {
			text += " " + link
		}
		btn := slack.NewButtonBlockElement(actionPublishAnyway, fmt.Sprintf("%s|%t", mode, sendPrivate),
			slack.NewTextBlockObject(slack.PlainTextType, "Publish anyway", false, false))
		blocks := []slack.Block{
			slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
			slack.NewActionBlock("publish_actions", btn),
		}
		if _, err := api.PostEphemeral(channelID, userID, slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(text, false)); err != nil {
			log.Printf("generate-report warning error: %v", err)
		}
		return
	}
	// Confirmed: unsure Work Items nobody answered go to the model's #1 Section; no model is called.
	if filing.FileByFallback(db, NewNotifier(api, db, cfg), live.View.Undetermined, true) > 0 {
		if live, err = publish.LoadWeek(db, cfg, time.Now()); err != nil {
			postEphemeralTo(api, channelID, userID, fmt.Sprintf("Error loading the report: %v", err))
			return
		}
	}

	res, err := publish.Publish(db, cfg, live, userID)
	if err != nil {
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("Error publishing the report: %v", err))
		log.Printf("generate-report publish error: %v", err)
		return
	}
	if proposer != nil {
		go proposer.AfterPublish()
	}
	filePath, title := res.TeamPath, fmt.Sprintf("%s team report", cfg.TeamName)
	if mode == "boss" {
		filePath, title = res.BossPath, fmt.Sprintf("%s report email draft", cfg.TeamName)
	}
	if err := uploadReportFile(api, channelID, userID, sendPrivate, filePath, title,
		fmt.Sprintf("Published report for the week of %s (mode: %s)", live.Monday.Format("2006-01-02"), mode)); err != nil {
		postEphemeralTo(api, channelID, userID, err.Error())
		return
	}
	postEphemeralTo(api, channelID, userID, fmt.Sprintf("Published %d Work Items (mode: %s). Saved to: %s", live.View.ItemCount(), mode, filePath))
	log.Printf("generate-report done items=%d mode=%s", live.View.ItemCount(), mode)
}

// fallbackCount is how many of items publishing would file by fallback.
func fallbackCount(items []WorkItem) int {
	n := 0
	for _, it := range items {
		if filing.EligibleForFallback(it) {
			n++
		}
	}
	return n
}

func uploadReportFile(api *slack.Client, channelID, userID string, sendPrivate bool, filePath, title, comment string) error {
	fi, err := os.Stat(filePath)
	if err != nil || fi.Size() <= 0 {
		return fmt.Errorf("error: generated report file is empty or unreadable")
	}
	uploadChannel := channelID
	if sendPrivate {
		ch, _, _, err := api.OpenConversation(&slack.OpenConversationParameters{Users: []string{userID}})
		if err != nil {
			log.Printf("Error opening DM for private report: %v", err)
			return fmt.Errorf("error opening DM to send private report. Check bot permissions")
		}
		uploadChannel = ch.ID
	}
	_, err = api.UploadFileV2(slack.UploadFileV2Parameters{
		File: filePath, FileSize: int(fi.Size()), Filename: filepath.Base(filePath),
		Channel: uploadChannel, Title: title, InitialComment: comment,
	})
	if err != nil {
		log.Printf("Error uploading report file: %v", err)
		return fmt.Errorf("error uploading report file to channel. Check bot permissions")
	}
	return nil
}

func postLatestTeamReport(api *slack.Client, cfg Config, cmd slack.SlashCommand, sendPrivate bool) {
	postEphemeral(api, cmd, "Posting latest team report...")
	filePath, reportDate, err := findLatestTeamReportFile(cfg.ReportOutputDir, cfg.TeamName)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error finding latest team report: %v", err))
		log.Printf("post-report find error: %v", err)
		return
	}

	fi, err := os.Stat(filePath)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error reading report file: %v", err))
		log.Printf("post-report stat error path=%s err=%v", filePath, err)
		return
	}
	if fi.Size() <= 0 {
		postEphemeral(api, cmd, "Latest team report file is empty.")
		log.Printf("post-report empty file path=%s", filePath)
		return
	}

	title := fmt.Sprintf("%s team report", cfg.TeamName)
	initialComment := fmt.Sprintf("Latest team report for week containing %s", reportDate.Format("2006-01-02"))
	uploadChannel := cmd.ChannelID
	if sendPrivate {
		ch, _, _, err := api.OpenConversation(&slack.OpenConversationParameters{Users: []string{cmd.UserID}})
		if err != nil {
			postEphemeral(api, cmd, "Error opening DM to send private report. Check bot permissions.")
			log.Printf("post-report dm open error user=%s: %v", cmd.UserID, err)
			return
		}
		uploadChannel = ch.ID
	}

	_, err = api.UploadFileV2(slack.UploadFileV2Parameters{
		File:           filePath,
		FileSize:       int(fi.Size()),
		Filename:       filepath.Base(filePath),
		Channel:        uploadChannel,
		Title:          title,
		InitialComment: initialComment,
	})
	if err != nil {
		postEphemeral(api, cmd, "Error posting latest team report to channel.")
		log.Printf("post-report upload error path=%s err=%v", filePath, err)
		return
	}

	delivery := "channel"
	if sendPrivate {
		delivery = "private"
	}
	postEphemeral(api, cmd, fmt.Sprintf("Posted latest team report (%s): %s", delivery, filepath.Base(filePath)))
	log.Printf("post-report done path=%s private=%t", filePath, sendPrivate)
}

func findLatestTeamReportFile(outputDir, teamName string) (string, time.Time, error) {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("reading report output dir: %w", err)
	}

	prefix := sanitizeReportFilenamePart(teamName) + "_"
	var latestPath string
	var latestDate time.Time
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".md") {
			continue
		}
		rawDate := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".md")
		reportDate, err := time.Parse("20060102", rawDate)
		if err != nil {
			continue
		}
		if latestPath == "" || reportDate.After(latestDate) {
			latestPath = filepath.Join(outputDir, name)
			latestDate = reportDate
		}
	}

	if latestPath == "" {
		return "", time.Time{}, fmt.Errorf("no team report markdown file found in %s for team %s", outputDir, teamName)
	}
	return latestPath, latestDate, nil
}

func sanitizeReportFilenamePart(s string) string {
	var cleaned strings.Builder
	for _, r := range s {
		if r == 0 || (r >= 0x01 && r <= 0x1F) || r == 0x7F {
			continue
		}
		cleaned.WriteRune(r)
	}
	sanitized := cleaned.String()
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	sanitized = replacer.Replace(sanitized)
	sanitized = strings.Trim(sanitized, " .")
	if sanitized == "" || strings.Trim(sanitized, "_") == "" {
		return "report"
	}
	return sanitized
}

func handleListItems(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	scope, err := parseListScope(cmd.Text)
	if err != nil {
		postEphemeral(api, cmd, err.Error())
		return
	}
	renderListItems(api, db, cfg, cmd.ChannelID, cmd.UserID, 0, scope)
}

func renderListItems(api *slack.Client, db *sql.DB, cfg Config, channelID, userID string, page int, scope string) {
	scope = normalizeListScope(scope)
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	// The Weekly Report's items: open ones from any week plus those done this week.
	items, err := GetWeekItems(db, monday, nextMonday)
	if err != nil {
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("Error: %v", err))
		return
	}
	tax, err := LoadTaxonomy(db)
	if err != nil {
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("Error: %v", err))
		return
	}

	isManager, _ := isManagerUser(api, cfg, userID)
	user, _ := api.GetUserInfo(userID)
	if scope == listScopeMine {
		filtered := make([]WorkItem, 0, len(items))
		for _, item := range items {
			if listedAsMine(item, userID, user) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}

	if len(items) == 0 {
		msg := fmt.Sprintf("No Work Items for this week (%s - %s)",
			monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2"))
		if scope == listScopeMine {
			msg = fmt.Sprintf("You have no Work Items for this week (%s - %s)",
				monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2"))
		}
		postEphemeralTo(api, channelID, userID, msg)
		log.Printf("list-items empty")
		return
	}

	// Group by author (first name alphabetically), then by reported_at ascending.
	sortKeys := make(map[int64]string, len(items))
	for _, it := range items {
		if fields := strings.Fields(synthesizeName(it.Author)); len(fields) > 0 {
			sortKeys[it.ID] = strings.ToLower(fields[0])
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if sortKeys[items[i].ID] != sortKeys[items[j].ID] {
			return sortKeys[items[i].ID] < sortKeys[items[j].ID]
		}
		return items[i].ReportedAt.Before(items[j].ReportedAt)
	})

	if page < 0 {
		page = 0
	}
	start := page * listItemsPageSize
	if start >= len(items) {
		page = (len(items) - 1) / listItemsPageSize
		start = page * listItemsPageSize
	}
	end := min(start+listItemsPageSize, len(items))

	titlePrefix := "Your Work Items"
	if scope == listScopeAll {
		titlePrefix = "Team Work Items"
	}
	blocks := []slack.Block{slack.NewHeaderBlock(
		slack.NewTextBlockObject(slack.PlainTextType,
			fmt.Sprintf("%s for %s - %s (%d total)", titlePrefix, monday.Format("Jan 2"),
				nextMonday.AddDate(0, 0, -1).Format("Jan 2"), len(items)),
			false, false,
		),
	)}
	for idx, item := range items[start:end] {
		blocks = appendListItemBlock(blocks, start+idx+1, item, sectionLabel(tax, item), scope, isManager, userID)
	}

	if len(items) > listItemsPageSize {
		var nav []slack.BlockElement
		if page > 0 {
			nav = append(nav, slack.NewButtonBlockElement(actionPagePrev, fmt.Sprintf("%s|%d", scope, page-1),
				slack.NewTextBlockObject(slack.PlainTextType, "Prev", false, false)))
		}
		if end < len(items) {
			nav = append(nav, slack.NewButtonBlockElement(actionPageNext, fmt.Sprintf("%s|%d", scope, page+1),
				slack.NewTextBlockObject(slack.PlainTextType, "Next", false, false)))
		}
		if len(nav) > 0 {
			blocks = append(blocks, slack.NewActionBlock("list_items_nav", nav...))
		}
	}

	if _, err = api.PostEphemeral(channelID, userID, slack.MsgOptionBlocks(blocks...)); err != nil {
		log.Printf("Error posting list-items blocks: %v", err)
		postEphemeralTo(api, channelID, userID, "Error rendering list items.")
		return
	}
	log.Printf("list-items count=%d page=%d", len(items), page)
}

// sectionLabel names an item's Section for lists, or "Undetermined".
func sectionLabel(tax domain.Taxonomy, item WorkItem) string {
	if s, ok := tax.Section(item.SectionID); ok {
		return s.Label()
	}
	if item.FilingState == domain.FilingPending {
		return "filing…"
	}
	return report.UndeterminedHeading
}

func appendListItemBlock(blocks []slack.Block, lineNumber int, item WorkItem, section, scope string, isManager bool, userID string) []slack.Block {
	source := ""
	switch item.Source {
	case "gitlab":
		source = " [GitLab]"
	case "github":
		source = " [GitHub]"
	}
	text := formatListItemText(lineNumber, item, source, fmt.Sprintf(" _%s_", section))
	if canManageItem(item, isManager, userID) {
		editOpt := slack.NewOptionBlockObject(
			fmt.Sprintf("edit:%s:%d", scope, item.ID),
			slack.NewTextBlockObject(slack.PlainTextType, "Edit", false, false),
			nil,
		)
		deleteOpt := slack.NewOptionBlockObject(
			fmt.Sprintf("delete:%s:%d", scope, item.ID),
			slack.NewTextBlockObject(slack.PlainTextType, "Delete", false, false),
			nil,
		)
		menu := slack.NewOverflowBlockElement(actionRowMenu, editOpt, deleteOpt)
		return append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
			nil,
			slack.NewAccessory(menu),
		))
	}
	return append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
		nil,
		nil,
	))
}

func handleListMissing(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	isManager, err := isManagerUser(api, cfg, cmd.UserID)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error checking permissions: %v", err))
		log.Printf("list-missing auth error user=%s: %v", cmd.UserID, err)
		return
	}
	if !isManager {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		log.Printf("list-missing denied user=%s", cmd.UserID)
		return
	}

	if len(cfg.TeamMembers) == 0 {
		postEphemeral(api, cmd, "No team_members configured.")
		return
	}

	memberIDs, unresolved, err := resolveUserIDs(api, cfg.TeamMembers)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error resolving team members: %v", err))
		log.Printf("list-missing resolve error: %v", err)
		return
	}

	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	weekItems, err := GetItemsByDateRange(db, monday, nextMonday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading Work Items: %v", err))
		log.Printf("list-missing load error: %v", err)
		return
	}
	reportedAuthors := uniqueReportedAuthors(weekItems)

	reportedAuthorIDs, err := GetSlackAuthorIDsByDateRange(db, monday, nextMonday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading Work Items: %v", err))
		log.Printf("list-missing load error: %v", err)
		return
	}

	// Build an ID→User map from the cached users list to avoid N individual
	// GetUserInfo calls (one per team member) inside the loop below.
	cachedUsers, err := getCachedUsers(api)
	if err != nil {
		log.Printf("list-missing: getCachedUsers error: %v", err)
	}
	userByID := make(map[string]slack.User, len(cachedUsers))
	for _, u := range cachedUsers {
		userByID[u.ID] = u
	}

	type missingMember struct {
		display string
		userID  string
	}
	var missing []missingMember
	var missingIDs []string
	for _, uid := range memberIDs {
		u, found := userByID[uid]
		nameCandidates := []string{uid}
		if found {
			if u.Profile.DisplayName != "" {
				nameCandidates = append(nameCandidates, u.Profile.DisplayName)
			}
			if u.RealName != "" {
				nameCandidates = append(nameCandidates, u.RealName)
			}
		}
		if memberReportedThisWeek(uid, nameCandidates, reportedAuthorIDs, reportedAuthors) {
			continue
		}
		if !found {
			missing = append(missing, missingMember{display: uid, userID: uid})
			missingIDs = append(missingIDs, uid)
			continue
		}

		display := u.Profile.DisplayName
		if display == "" {
			display = u.RealName
		}
		if display == "" {
			display = u.Name
		}
		if display == "" {
			display = uid
		}
		missing = append(missing, missingMember{display: display, userID: uid})
		missingIDs = append(missingIDs, uid)
	}

	var blocks []slack.Block
	if len(missing) == 0 && len(unresolved) == 0 {
		blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType,
			fmt.Sprintf("Everyone has reported this week (%s - %s).", monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2")),
			false, false), nil, nil))
	} else {
		blocks = append(blocks, slack.NewHeaderBlock(
			slack.NewTextBlockObject(slack.PlainTextType,
				fmt.Sprintf("Missing reports for %s - %s (%d)",
					monday.Format("Jan 2"),
					nextMonday.AddDate(0, 0, -1).Format("Jan 2"),
					len(missing)+len(unresolved)),
				false, false),
		))
	}

	for _, m := range missing {
		text := fmt.Sprintf("%s (<@%s>)", m.display, m.userID)
		nudgeBtn := slack.NewButtonBlockElement(
			actionNudgeMember,
			m.userID,
			slack.NewTextBlockObject(slack.PlainTextType, "Nudge", false, false),
		)
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
			nil,
			slack.NewAccessory(nudgeBtn),
		))
	}

	for _, name := range unresolved {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf("%s (not found)", name), false, false),
			nil, nil,
		))
	}

	if len(missingIDs) > 0 {
		nudgeAllBtn := slack.NewButtonBlockElement(
			actionNudgeAll,
			strings.Join(missingIDs, ","),
			slack.NewTextBlockObject(slack.PlainTextType, "Nudge All", false, false),
		)
		blocks = append(blocks, slack.NewDividerBlock(), slack.NewActionBlock("", nudgeAllBtn))
	}
	blocks = append(blocks, mergedInTestingBlocks(db)...)

	_, err = api.PostEphemeral(cmd.ChannelID, cmd.UserID, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		log.Printf("Error posting list-missing blocks: %v", err)
		postEphemeral(api, cmd, "Error rendering missing members list.")
		return
	}
	log.Printf("list-missing count=%d", len(missing)+len(unresolved))
}

func uniqueReportedAuthors(items []WorkItem) []string {
	seen := make(map[string]bool)
	var out []string
	for _, item := range items {
		author := strings.TrimSpace(item.Author)
		if author == "" || seen[author] {
			continue
		}
		seen[author] = true
		out = append(out, author)
	}
	return out
}

func memberReportedThisWeek(userID string, nameCandidates []string, reportedAuthorIDs map[string]bool, reportedAuthors []string) bool {
	if reportedAuthorIDs[userID] {
		return true
	}
	for _, candidate := range nameCandidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		for _, author := range reportedAuthors {
			if strings.EqualFold(candidate, author) || nameMatches(candidate, author) || nameMatches(author, candidate) {
				return true
			}
		}
	}
	return false
}

func postEphemeral(api *slack.Client, cmd slack.SlashCommand, text string) {
	postEphemeralTo(api, cmd.ChannelID, cmd.UserID, text)
}

func postEphemeralTo(api *slack.Client, channelID, userID, text string) {
	_, err := api.PostEphemeral(channelID, userID, slack.MsgOptionText(text, false))
	if err != nil {
		log.Printf("Error posting ephemeral: %v", err)
	}
}

func handleInteraction(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback) {
	switch cb.Type {
	case slack.InteractionTypeBlockActions:
		handleBlockActions(api, db, cfg, cb)
	case slack.InteractionTypeViewSubmission:
		handleViewSubmission(api, db, cfg, cb)
	}
}

func handleBlockActions(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback) {
	if len(cb.ActionCallback.BlockActions) == 0 {
		return
	}
	act := cb.ActionCallback.BlockActions[0]
	channelID := cb.Channel.ID
	if channelID == "" {
		channelID = cb.Container.ChannelID
	}
	userID := cb.User.ID

	if strings.HasPrefix(act.ActionID, actionPlacePick+":") {
		handlePlacePick(api, db, cfg, cb, act.Value)
		return
	}
	switch act.ActionID {
	case actionPlaceOther:
		itemID, err := strconv.ParseInt(strings.TrimSpace(act.Value), 10, 64)
		if err != nil {
			postEphemeralTo(api, channelID, userID, "Invalid Work Item id.")
			return
		}
		openEditModal(api, db, cfg, cb.TriggerID, channelID, userID, itemID, listScopeMine)
		return
	case actionUpdateConfirm:
		handleUpdateConfirm(api, db, cfg, cb, act.Value)
		return
	case actionUpdateReject:
		handleUpdateReject(api, db, cfg, cb, act.Value)
		return
	case actionLinkConfirm:
		handleLinkConfirm(api, db, cfg, cb, act.Value)
		return
	case actionLinkReject:
		handleLinkReject(api, db, cfg, cb, act.Value)
		return
	case actionPlacementUndo:
		handlePlacementUndo(api, db, cfg, cb, act.Value)
		return
	case actionCheckDone:
		handleCheckDone(api, db, cfg, cb, act.Value)
		return
	case actionPublishAnyway:
		mode, private, _ := strings.Cut(act.Value, "|")
		if !cfg.IsManagerID(userID) {
			return
		}
		publishReport(api, db, cfg, channelID, userID, mode, private == "true", true)
		return
	case actionPagePrev, actionPageNext:
		scope, page := parseListPageValue(act.Value)
		renderListItems(api, db, cfg, channelID, userID, page, scope)
	case actionDeleteItem:
		itemID, err := strconv.ParseInt(strings.TrimSpace(act.Value), 10, 64)
		if err != nil {
			postEphemeralTo(api, channelID, userID, "Invalid Work Item id.")
			return
		}
		deleteItemAction(api, db, cfg, channelID, userID, itemID, listScopeMine)
	case actionEditItemOpen:
		itemID, err := strconv.ParseInt(strings.TrimSpace(act.Value), 10, 64)
		if err != nil {
			postEphemeralTo(api, channelID, userID, "Invalid Work Item id.")
			return
		}
		openEditModal(api, db, cfg, cb.TriggerID, channelID, userID, itemID, listScopeMine)
	case actionNudgeMember:
		openNudgeConfirmModal(api, cfg, cb.TriggerID, channelID, act.Value)
		return
	case actionNudgeAll:
		openNudgeConfirmModal(api, cfg, cb.TriggerID, channelID, act.Value)
		return
	case actionNudgeDone:
		handleNudgeDoneAction(api, db, cfg, cb, act)
		return
	case actionNudgeMore:
		handleNudgeMoreAction(api, db, cfg, cb, act)
		return
	case actionNudgePagePrev, actionNudgePageNext:
		handleNudgePageAction(api, db, cfg, cb, act)
		return
	case actionProposalApprove, actionProposalReject:
		handleProposalDecision(api, db, cfg, cb, act.Value, act.ActionID == actionProposalApprove)
		return
	case actionRowMenu:
		val := strings.TrimSpace(act.SelectedOption.Value)
		if val == "" {
			val = strings.TrimSpace(act.Value)
		}
		if strings.HasPrefix(val, "edit:") {
			scope, itemID, ok := parseListRowAction(val, "edit")
			if !ok {
				postEphemeralTo(api, channelID, userID, "Invalid Work Item id.")
				return
			}
			openEditModal(api, db, cfg, cb.TriggerID, channelID, userID, itemID, scope)
			return
		}
		if strings.HasPrefix(val, "delete:") {
			scope, itemID, ok := parseListRowAction(val, "delete")
			if !ok {
				postEphemeralTo(api, channelID, userID, "Invalid Work Item id.")
				return
			}
			openDeleteModal(api, db, cfg, cb.TriggerID, channelID, userID, itemID, scope)
			return
		}
	}
}

func handleViewSubmission(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback) {
	if cb.View.CallbackID == modalNudgeConfirmCallback {
		handleNudgeConfirm(api, db, cfg, cb)
		return
	}

	if cb.View.CallbackID == modalDeleteCallbackID {
		userID := cb.User.ID
		meta := strings.TrimSpace(cb.View.PrivateMetadata)
		parts := strings.Split(meta, "|")
		if len(parts) != 2 || !strings.HasPrefix(parts[0], modalMetaPrefix) {
			return
		}
		channelID := strings.TrimSpace(parts[1])
		if channelID == "" {
			channelID = cb.Container.ChannelID
		}
		if channelID == "" {
			channelID = cb.Channel.ID
		}
		scope, itemID, ok := parseListModalMeta(parts[0])
		if !ok {
			return
		}
		deleteItemAction(api, db, cfg, channelID, userID, itemID, scope)
		return
	}

	if cb.View.CallbackID != modalEditCallbackID {
		return
	}
	userID := cb.User.ID
	meta := strings.TrimSpace(cb.View.PrivateMetadata)
	parts := strings.Split(meta, "|")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], modalMetaPrefix) {
		return
	}
	channelID := strings.TrimSpace(parts[1])
	scope, itemID, ok := parseListModalMeta(parts[0])
	if !ok {
		return
	}
	if cb.View.State == nil {
		return
	}
	values := cb.View.State.Values
	if values == nil {
		return
	}
	description := strings.TrimSpace(values[editBlockDescription][editActionDescription].Value)
	status := strings.TrimSpace(values[editBlockStatus][editActionStatus].SelectedOption.Value)
	statusNote := strings.TrimSpace(values[editBlockStatusNote][editActionStatusNote].Value)
	sectionValue := strings.TrimSpace(values[editBlockSection][editActionSection].SelectedOption.Value)

	item, err := GetWorkItemByID(db, itemID)
	if err != nil {
		log.Printf("edit modal load item error id=%d: %v", itemID, err)
		return
	}
	if !domain.IsStatus(status) {
		status = item.Status
	}
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	if !itemEditable(item, monday, nextMonday) {
		return
	}
	isManager, _ := isManagerUser(api, cfg, userID)
	if !canManageItem(item, isManager, userID) || description == "" {
		return
	}
	if channelID == "" {
		channelID = cb.Container.ChannelID
	}
	if channelID == "" {
		channelID = cb.Channel.ID
	}

	if err := updateWorkItemFields(db, itemID, description, status, statusNote, item.TicketIDs, slackActor(userID)); err != nil {
		log.Printf("edit modal update error id=%d: %v", itemID, err)
		return
	}
	newSection, _ := strconv.ParseInt(sectionValue, 10, 64)
	switch {
	case sectionValue != "" && newSection != item.SectionID:
		// A Section the person chose is final for this edit; no re-filing.
		if err := setPlacement(db, itemID, Placement{SectionID: newSection, PlacedBy: domain.PlacedByPerson,
			HistoryNote: reasonEdit, ActorID: userID}); err != nil {
			log.Printf("edit modal placement error id=%d: %v", itemID, err)
		}
	case description != item.Description:
		// New text may belong elsewhere: file it again.
		payload := filing.Payload{Kind: filing.KindEdit, ChannelID: channelID, UserID: userID}
		if err := setFilingPending(db, itemID, payload.JSON()); err != nil {
			log.Printf("edit modal refile error id=%d: %v", itemID, err)
		}
		wakeFiling()
	}
	renderListItems(api, db, cfg, channelID, userID, 0, scope)
}

func deleteItemAction(api *slack.Client, db *sql.DB, cfg Config, channelID, userID string, itemID int64, scope string) {
	item, err := GetWorkItemByID(db, itemID)
	if err != nil {
		postEphemeralTo(api, channelID, userID, "Work Item not found.")
		return
	}
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	if !itemEditable(item, monday, nextMonday) {
		postEphemeralTo(api, channelID, userID, "You can only modify this week's items.")
		return
	}

	isManager, _ := isManagerUser(api, cfg, userID)
	if !canManageItem(item, isManager, userID) {
		postEphemeralTo(api, channelID, userID, "You are not allowed to delete this Work Item.")
		return
	}

	if err := DeleteWorkItemByID(db, itemID, userID); err != nil {
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("Delete failed: %v", err))
		return
	}
	renderListItems(api, db, cfg, channelID, userID, 0, scope)
}

func openEditModal(api *slack.Client, db *sql.DB, cfg Config, triggerID, channelID, userID string, itemID int64, scope string) {
	item, err := GetWorkItemByID(db, itemID)
	if err != nil {
		postEphemeralTo(api, channelID, userID, "Work Item not found.")
		return
	}
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	if !itemEditable(item, monday, nextMonday) {
		postEphemeralTo(api, channelID, userID, "You can only modify this week's items.")
		return
	}

	isManager, _ := isManagerUser(api, cfg, userID)
	if !canManageItem(item, isManager, userID) {
		postEphemeralTo(api, channelID, userID, "You are not allowed to edit this Work Item.")
		return
	}

	tax, err := LoadTaxonomy(db)
	if err != nil {
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("Error: %v", err))
		return
	}
	blocks := editModalBlocks(item, tax)

	view := slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Edit Work Item", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Save", false, false),
		CallbackID:      modalEditCallbackID,
		PrivateMetadata: fmt.Sprintf("%s%s:%d|%s", modalMetaPrefix, normalizeListScope(scope), itemID, channelID),
		Blocks:          slack.Blocks{BlockSet: blocks},
	}
	if _, err := api.OpenView(triggerID, view); err != nil {
		if apiErr, ok := err.(*slack.SlackErrorResponse); ok {
			log.Printf("openEditModal slack error err=%s messages=%v", apiErr.Err, apiErr.ResponseMetadata.Messages)
		}
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("Unable to open edit dialog: %v", err))
	}
}

func openDeleteModal(api *slack.Client, db *sql.DB, cfg Config, triggerID, channelID, userID string, itemID int64, scope string) {
	item, err := GetWorkItemByID(db, itemID)
	if err != nil {
		postEphemeralTo(api, channelID, userID, "Work Item not found.")
		return
	}
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	if !itemEditable(item, monday, nextMonday) {
		postEphemeralTo(api, channelID, userID, "You can only modify this week's items.")
		return
	}
	isManager, _ := isManagerUser(api, cfg, userID)
	if !canManageItem(item, isManager, userID) {
		postEphemeralTo(api, channelID, userID, "You are not allowed to delete this Work Item.")
		return
	}

	view := slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Delete Work Item", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Delete", false, false),
		CallbackID:      modalDeleteCallbackID,
		PrivateMetadata: fmt.Sprintf("%s%s:%d|%s", modalMetaPrefix, normalizeListScope(scope), itemID, channelID),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject(
					slack.MarkdownType,
					fmt.Sprintf("Delete this Work Item?\n\n*%s*: %s (%s)", item.Author, item.Description, item.Status),
					false,
					false,
				),
				nil,
				nil,
			),
		}},
	}
	if _, err := api.OpenView(triggerID, view); err != nil {
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("Unable to open delete confirmation: %v", err))
	}
}

// itemEditable allows changes to items on this week's report: open ones, and
// ones reported or done this week.
func itemEditable(item WorkItem, from, to time.Time) bool {
	if item.IsOpen() {
		return true
	}
	inWeek := func(t time.Time) bool { return !t.IsZero() && !t.Before(from) && t.Before(to) }
	return inWeek(item.ReportedAt) || inWeek(item.DoneAt)
}

func formatItemDescriptionForList(item WorkItem) string {
	description := strings.TrimSpace(item.Description)
	tickets := canonicalTicketList(item.TicketIDs)
	if tickets == "" {
		return description
	}
	if leading, ok := leadingTicketPrefix(description); ok && canonicalTicketList(leading) == tickets {
		return description
	}
	return fmt.Sprintf("[%s] %s", tickets, description)
}

func formatListItemText(lineNumber int, item WorkItem, source, section string) string {
	return fmt.Sprintf("%d. *%s*: %s (%s)%s%s",
		lineNumber, item.Author, formatItemDescriptionForList(item), report.StatusText(item), source, section)
}

func parseListScope(raw string) (string, error) {
	text := strings.TrimSpace(strings.ToLower(raw))
	switch text {
	case "", listScopeMine:
		return listScopeMine, nil
	case listScopeAll:
		return listScopeAll, nil
	default:
		return "", fmt.Errorf("Usage: `/list` or `/list all`")
	}
}

func normalizeListScope(scope string) string {
	if strings.EqualFold(strings.TrimSpace(scope), listScopeAll) {
		return listScopeAll
	}
	return listScopeMine
}

func parseListPageValue(raw string) (string, int) {
	parts := strings.Split(strings.TrimSpace(raw), "|")
	if len(parts) == 2 {
		page, err := strconv.Atoi(parts[1])
		if err == nil {
			return normalizeListScope(parts[0]), page
		}
	}
	page, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return listScopeMine, 0
	}
	return listScopeMine, page
}

func parseListRowAction(raw, expectedAction string) (string, int64, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) == 3 && parts[0] == expectedAction {
		itemID, err := strconv.ParseInt(parts[2], 10, 64)
		if err == nil {
			return normalizeListScope(parts[1]), itemID, true
		}
	}
	if len(parts) == 2 && parts[0] == expectedAction {
		itemID, err := strconv.ParseInt(parts[1], 10, 64)
		if err == nil {
			return listScopeMine, itemID, true
		}
	}
	return "", 0, false
}

func parseListModalMeta(raw string) (string, int64, bool) {
	meta := strings.TrimPrefix(strings.TrimSpace(raw), modalMetaPrefix)
	if meta == "" {
		return "", 0, false
	}
	if parts := strings.Split(meta, ":"); len(parts) == 2 {
		itemID, err := strconv.ParseInt(parts[1], 10, 64)
		if err == nil {
			return normalizeListScope(parts[0]), itemID, true
		}
	}
	itemID, err := strconv.ParseInt(meta, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return listScopeMine, itemID, true
}

func leadingTicketPrefix(description string) (string, bool) {
	description = strings.TrimSpace(description)
	if !strings.HasPrefix(description, "[") {
		return "", false
	}
	end := strings.Index(description, "]")
	if end <= 1 {
		return "", false
	}
	return description[1:end], true
}

func canonicalTicketList(ticketIDs string) string {
	if strings.TrimSpace(ticketIDs) == "" {
		return ""
	}
	parts := strings.Split(ticketIDs, ",")
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		cleaned = append(cleaned, p)
	}
	return strings.Join(cleaned, ",")
}

// canManageItem: managers manage every item; others only items whose Slack user ID is
// theirs. Names are never trusted for this, since users can change their display name.
func canManageItem(item WorkItem, isManager bool, userID string) bool {
	return isManager || ownsItem(item, userID)
}

func ownsItem(item WorkItem, userID string) bool {
	id := strings.TrimSpace(item.AuthorID)
	return id != "" && id == strings.TrimSpace(userID)
}

// listedAsMine decides what "/list" shows as the viewer's items. It also matches by name,
// because it only filters a view; it never grants a change.
func listedAsMine(item WorkItem, userID string, user *slack.User) bool {
	if ownsItem(item, userID) {
		return true
	}
	if item.AuthorID != "" || user == nil {
		return false
	}
	for _, c := range []string{user.Profile.DisplayName, user.RealName, user.Name} {
		if c = strings.TrimSpace(c); c != "" && (strings.EqualFold(strings.TrimSpace(item.Author), c) || nameMatches(c, item.Author) || nameMatches(item.Author, c)) {
			return true
		}
	}
	return false
}

func openNudgeConfirmModal(api *slack.Client, cfg Config, triggerID, channelID, targetIDs string) {
	var validIDs []string
	var names []string
	for _, id := range strings.Split(targetIDs, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		validIDs = append(validIDs, id)
		user, err := api.GetUserInfo(id)
		if err != nil {
			names = append(names, fmt.Sprintf("<@%s>", id))
			continue
		}
		display := user.Profile.DisplayName
		if display == "" {
			display = user.RealName
		}
		if display == "" {
			display = id
		}
		names = append(names, display)
	}

	if len(validIDs) == 0 {
		return
	}

	prompt := fmt.Sprintf("Send a nudge reminder DM to *%s*?", strings.Join(names, ", "))
	if len(validIDs) > 1 {
		prompt = fmt.Sprintf("Send a nudge reminder DM to *%d members*?\n%s", len(validIDs), strings.Join(names, ", "))
	}

	view := slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Confirm nudge", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Send Nudge", false, false),
		CallbackID:      modalNudgeConfirmCallback,
		PrivateMetadata: fmt.Sprintf("%s%s|%s", nudgeMetaPrefix, targetIDs, channelID),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, prompt, false, false),
				nil, nil,
			),
		}},
	}
	if _, err := api.OpenView(triggerID, view); err != nil {
		log.Printf("nudge confirm modal error: %v", err)
	}
}

func handleNudgeConfirm(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback) {
	userID := cb.User.ID
	if !cfg.IsManagerID(userID) {
		log.Printf("nudge confirm denied user=%s (not manager)", userID)
		return
	}
	meta := strings.TrimSpace(cb.View.PrivateMetadata)
	parts := strings.SplitN(meta, "|", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], nudgeMetaPrefix) {
		return
	}
	channelID := strings.TrimSpace(parts[1])
	if channelID == "" {
		channelID = cb.Container.ChannelID
	}
	if channelID == "" {
		channelID = cb.Channel.ID
	}

	targetIDsStr := strings.TrimPrefix(parts[0], nudgeMetaPrefix)
	var targetIDs []string
	for _, id := range strings.Split(targetIDsStr, ",") {
		id = strings.TrimSpace(id)
		if id != "" {
			targetIDs = append(targetIDs, id)
		}
	}

	if len(targetIDs) == 0 {
		return
	}

	sendNudges(api, db, cfg, targetIDs, cfg.ReportChannelID)
	postEphemeralTo(api, channelID, userID, fmt.Sprintf("Sent nudge to %d member(s).", len(targetIDs)))
	log.Printf("nudge sent from /check user=%s count=%d", userID, len(targetIDs))
}

func handleNudgeDoneAction(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, act *slack.BlockAction) {
	targetUserID, itemID, page, ok := parseNudgeDonePayload(act.Value)
	if !ok {
		log.Printf("nudge done invalid payload=%q", act.Value)
		return
	}
	if !authorizeNudgeAction(api, cb, targetUserID) {
		return
	}
	if !canActOnNudgeItem(db, itemID, targetUserID) {
		log.Printf("nudge done denied item=%d target=%s", itemID, targetUserID)
		return
	}
	if err := SetWorkItemStatus(db, itemID, domain.StatusDone, nudgeActor(cb.User.ID)); err != nil {
		log.Printf("nudge done update error item=%d: %v", itemID, err)
		return
	}
	refreshNudgeMessage(api, db, cfg, cb, targetUserID, page, true)
}

func handleNudgeMoreAction(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, act *slack.BlockAction) {
	val := strings.TrimSpace(act.Value)
	if strings.TrimSpace(act.SelectedOption.Value) != "" {
		val = strings.TrimSpace(act.SelectedOption.Value)
	}
	targetUserID, itemID, status, page, ok := parseNudgeStatusPayload(val)
	if !ok {
		log.Printf("nudge more invalid payload=%q", val)
		return
	}
	if !authorizeNudgeAction(api, cb, targetUserID) {
		return
	}
	if !canActOnNudgeItem(db, itemID, targetUserID) {
		log.Printf("nudge status denied item=%d target=%s", itemID, targetUserID)
		return
	}
	if err := SetWorkItemStatus(db, itemID, status, nudgeActor(cb.User.ID)); err != nil {
		log.Printf("nudge status update error item=%d status=%q: %v", itemID, status, err)
		return
	}
	refreshNudgeMessage(api, db, cfg, cb, targetUserID, page, true)
}

func handleNudgePageAction(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, act *slack.BlockAction) {
	targetUserID, page, ok := parseNudgePagePayload(act.Value)
	if !ok {
		log.Printf("nudge page invalid payload=%q", act.Value)
		return
	}
	if !authorizeNudgeAction(api, cb, targetUserID) {
		return
	}
	refreshNudgeMessage(api, db, cfg, cb, targetUserID, page, false)
}

func authorizeNudgeAction(api *slack.Client, cb slack.InteractionCallback, targetUserID string) bool {
	if strings.TrimSpace(cb.User.ID) != strings.TrimSpace(targetUserID) {
		channelID := nudgeActionChannelID(cb)
		if channelID != "" {
			postEphemeralTo(api, channelID, cb.User.ID, "This nudge action is only available to the message recipient.")
		}
		log.Printf("nudge action denied user=%s target=%s", cb.User.ID, targetUserID)
		return false
	}
	return true
}

func canActOnNudgeItem(db *sql.DB, itemID int64, targetUserID string) bool {
	item, err := GetWorkItemByID(db, itemID)
	if err != nil {
		log.Printf("canActOnNudgeItem: failed to get work item id=%d: %v", itemID, err)
		return false
	}
	return ownsItem(item, targetUserID)
}

func refreshNudgeMessage(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, targetUserID string, page int, updated bool) {
	channelID := nudgeActionChannelID(cb)
	messageTS := strings.TrimSpace(cb.Container.MessageTs)
	if messageTS == "" {
		messageTS = strings.TrimSpace(cb.MessageTs)
	}
	if channelID == "" || messageTS == "" {
		log.Printf("nudge refresh missing channel/message ts channel=%q ts=%q", channelID, messageTS)
		return
	}

	rendered, err := RenderNudgeForUser(api, db, cfg, targetUserID, cfg.ReportChannelID, time.Now().In(cfg.Location), page, updated)
	if err != nil {
		log.Printf("nudge refresh render error user=%s: %v", targetUserID, err)
		return
	}
	_, _, _, err = api.UpdateMessage(
		channelID,
		messageTS,
		slack.MsgOptionText(rendered.Text, false),
		slack.MsgOptionBlocks(rendered.Blocks...),
	)
	if err != nil {
		log.Printf("nudge refresh update error channel=%s ts=%s: %v", channelID, messageTS, err)
	}
}

func nudgeActionChannelID(cb slack.InteractionCallback) string {
	channelID := strings.TrimSpace(cb.Channel.ID)
	if channelID == "" {
		channelID = strings.TrimSpace(cb.Container.ChannelID)
	}
	return channelID
}

func parseNudgeDonePayload(raw string) (targetUserID string, itemID int64, page int, ok bool) {
	parts := strings.Split(strings.TrimSpace(raw), "|")
	if len(parts) != 4 || parts[0] != "done" {
		return "", 0, 0, false
	}
	itemID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	page, err = strconv.Atoi(parts[3])
	if err != nil {
		return "", 0, 0, false
	}
	return strings.TrimSpace(parts[1]), itemID, page, true
}

func parseNudgeStatusPayload(raw string) (targetUserID string, itemID int64, status string, page int, ok bool) {
	parts := strings.Split(strings.TrimSpace(raw), "|")
	if len(parts) != 5 || parts[0] != "status" {
		return "", 0, "", 0, false
	}
	itemID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", 0, "", 0, false
	}
	page, err = strconv.Atoi(parts[4])
	if err != nil {
		return "", 0, "", 0, false
	}
	status = strings.TrimSpace(parts[3])
	switch status {
	case "in testing", "in progress":
	default:
		return "", 0, "", 0, false
	}
	return strings.TrimSpace(parts[1]), itemID, status, page, true
}

func parseNudgePagePayload(raw string) (targetUserID string, page int, ok bool) {
	parts := strings.Split(strings.TrimSpace(raw), "|")
	if len(parts) != 3 || parts[0] != "page" {
		return "", 0, false
	}
	page, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", 0, false
	}
	return strings.TrimSpace(parts[1]), page, true
}

func handleHelp(api *slack.Client, cfg Config, cmd slack.SlashCommand) {
	isManager, err := isManagerUser(api, cfg, cmd.UserID)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error checking permissions: %v", err))
		log.Printf("help auth error user=%s: %v", cmd.UserID, err)
		return
	}

	lines := []string{
		"*ReportBot Commands*",
		"",
		"`/report <description> (status)` — Report a Work Item.",
		"`/rpt` — Alias of `/report`.",
		">*Example:* `/report [ticket_id] Add pagination to user list API (done)`",
		">*Multiline* (each with own status, use Shift+Enter for newlines):",
		">```/report Item A (in progress)",
		">Item B (done)```",
		">*Shared status* (last line applies to all Work Items):",
		">```/report Item A",
		">Item B",
		">(in progress)```",
		"",
		"Each Work Item is filed into the report as it arrives; you get a reply showing where it went.",
		"",
		"`/list` — List your Work Items on this week's report (`/list all` for the team).",
		"`/nudge` — Send yourself a test nudge DM.",
		"`/help` — Show this help.",
	}

	if isManager {
		lines = append(lines,
			"",
			"*Manager Commands*",
			"",
			"`/fetch` — Fetch *merged + open* GitLab MRs and/or GitHub PRs for this week.",
			"`/generate-report [team|boss|post] [private]` — Publish this week's report, or post the latest team report.",
			"`/gen` — Alias of `/generate-report`.",
			"`/check` — List missing members with nudge buttons, and merged code changes waiting in testing.",
			"`/nudge <member>` — Send a test nudge DM to one member.",
			"`/retrospect` — Generate Structure Proposals now (new, split or retired Sections) for managers to approve.",
			"`/stats` — Show this week's filing stats.",
		)
	}

	postEphemeral(api, cmd, strings.Join(lines, "\n"))
}

func isManagerUser(_ *slack.Client, cfg Config, userID string) (bool, error) {
	return cfg.IsManagerID(userID), nil
}

func resolveNudgeTarget(api *slack.Client, cfg Config, input string) (string, string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", fmt.Errorf("empty target")
	}

	if isLikelySlackID(input) {
		if user, err := api.GetUserInfo(input); err == nil {
			label := strings.TrimSpace(user.Profile.DisplayName)
			if label == "" {
				label = strings.TrimSpace(user.RealName)
			}
			if label == "" {
				label = fmt.Sprintf("<@%s>", input)
			}
			return input, label, nil
		}
		return input, fmt.Sprintf("<@%s>", input), nil
	}

	if member, ok := resolveDelegatedAuthorName(input, cfg.TeamMembers); ok {
		ids, unresolved, err := resolveUserIDs(api, []string{member})
		if err == nil && len(ids) == 1 && len(unresolved) == 0 {
			return ids[0], member, nil
		}
	}

	ids, unresolved, err := resolveUserIDs(api, []string{input})
	if err != nil && len(ids) == 0 {
		return "", "", err
	}
	if len(ids) == 1 {
		return ids[0], input, nil
	}
	if len(ids) > 1 {
		return "", "", fmt.Errorf("resolved to multiple Slack users")
	}
	if len(unresolved) > 0 {
		return "", "", fmt.Errorf("no matching Slack user found")
	}
	return "", "", fmt.Errorf("no matching Slack user found")
}

func resolveDelegatedAuthorName(input string, teamMembers []string) (string, bool) {
	input = strings.TrimSpace(input)
	if input == "" || len(teamMembers) == 0 {
		return "", false
	}

	// 1) Exact match first.
	for _, member := range teamMembers {
		if strings.EqualFold(strings.TrimSpace(member), input) {
			return member, true
		}
	}

	// 2) Fuzzy match using token-subset logic in both directions.
	var matches []string
	for _, member := range teamMembers {
		if nameMatches(input, member) || nameMatches(member, input) {
			matches = append(matches, member)
		}
	}

	if len(matches) == 1 {
		return matches[0], true
	}

	// Ambiguous/no match: reject unresolved delegated names.
	return "", false
}
