package main

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

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

var statusRegex = regexp.MustCompile(`\(([^)]+)\)\s*$`)
var delegatedAuthorRegex = regexp.MustCompile(`^\{([^{}]+)\}\s*`)

const (
	listItemsPageSize     = 15
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
	editBlockCategory     = "edit_category"
	editActionCategory    = "category_input"

	actionUncertaintySelect = "uncertainty_select"
	actionUncertaintyOther  = "uncertainty_other"

	actionRetroApply   = "retro_apply"
	actionRetroDismiss = "retro_dismiss"

	actionNudgeMember         = "nudge_member"
	actionNudgeAll            = "nudge_all"
	modalNudgeConfirmCallback = "nudge_confirm_modal"
	nudgeMetaPrefix           = "nudge:"
)

func StartSlackBot(cfg Config, db *sql.DB, api *slack.Client) error {
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
	case "/fetch-mrs":
		handleFetchMRs(api, db, cfg, cmd)
	case "/generate-report":
		handleGenerateReport(api, db, cfg, cmd)
	case "/gen":
		handleGenerateReport(api, db, cfg, cmd)
	case "/list":
		handleListItems(api, db, cfg, cmd)
	case "/check":
		handleListMissing(api, db, cfg, cmd)
	case "/retrospective":
		handleRetrospective(api, db, cfg, cmd)
	case "/report-stats":
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

	intro := fmt.Sprintf("Welcome to %s! I'm ReportBot — I help track work items and generate weekly reports.\n\n"+
		"Here's how to get started:\n"+
		"• `/report <description> (status)` — Report a work item (e.g. `/report Fix login bug (done)`)\n"+
		"• `/list` — View this week's items\n"+
		"• `/help` — See all available commands\n\n"+
		"You can report multiple items at once with newlines, and set a shared status on the last line.",
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
		postEphemeral(api, cmd, "Usage: /report <description> (status)\nExample: /report [mantis_id] Add pagination to user list API (done)\nMultiline (separate items with newlines, e.g. Shift+Enter): /report Item A (in progress)\\nItem B (done)")
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
				author = resolveDelegatedAuthorName(delegated, cfg.TeamMembers)
				reportText = remaining
				if ids, _, err := resolveUserIDs(api, []string{author}); err == nil && len(ids) > 0 {
					authorID = ids[0]
				}
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
	if len(items) == 1 {
		if err := InsertWorkItem(db, items[0]); err != nil {
			postEphemeral(api, cmd, fmt.Sprintf("Error saving item: %v", err))
			log.Printf("report insert error user=%s: %v", cmd.UserID, err)
			return
		}
	} else {
		if _, err := InsertWorkItems(db, items); err != nil {
			postEphemeral(api, cmd, fmt.Sprintf("Error saving items: %v", err))
			log.Printf("report batch insert error user=%s: %v", cmd.UserID, err)
			return
		}
	}

	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	weekItems, err := GetSlackItemsByAuthorAndDateRange(db, author, monday, nextMonday)
	if err != nil {
		log.Printf("report weekly items lookup error user=%s author=%s: %v", cmd.UserID, author, err)
		postEphemeral(api, cmd, fmt.Sprintf("Recorded %d item(s) for %s.", len(items), author))
		return
	}

	msg := fmt.Sprintf("Recorded %d item(s) for %s.", len(items), author)
	previewLimit := 5
	if len(items) <= previewLimit {
		for _, it := range items {
			msg += fmt.Sprintf("\n• %s (%s)", it.Description, normalizeStatus(it.Status))
		}
	} else {
		for i := 0; i < previewLimit; i++ {
			msg += fmt.Sprintf("\n• %s (%s)", items[i].Description, normalizeStatus(items[i].Status))
		}
		msg += fmt.Sprintf("\n• ... and %d more", len(items)-previewLimit)
	}
	if len(weekItems) > 0 {
		msg += "\n\nItems reported this week:"
		limit := 8
		for i, p := range weekItems {
			if i >= limit {
				msg += fmt.Sprintf("\n• ... and %d more", len(weekItems)-limit)
				break
			}
			msg += fmt.Sprintf("\n• %s (%s)", p.Description, normalizeStatus(p.Status))
		}
	}
	postEphemeral(api, cmd, msg)
	log.Printf("report saved user=%s author=%s count=%d", cmd.UserID, author, len(items))
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
		status := "done"
		description := line
		if match := statusRegex.FindStringSubmatch(line); len(match) > 1 {
			status = strings.TrimSpace(match[1])
			description = strings.TrimSpace(line[:len(line)-len(match[0])])
		} else if sharedStatus != "" {
			status = sharedStatus
		}
		if description == "" {
			return nil, fmt.Errorf("Error: empty report item line.")
		}
		items = append(items, WorkItem{
			Description: description,
			Author:      author,
			Source:      "slack",
			Status:      status,
			ReportedAt:  now,
		})
	}
	return items, nil
}

func handleFetchMRs(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	isManager, err := isManagerUser(api, cfg, cmd.UserID)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error checking permissions: %v", err))
		log.Printf("fetch-mrs auth error user=%s: %v", cmd.UserID, err)
		return
	}
	if !isManager {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		log.Printf("fetch-mrs denied user=%s", cmd.UserID)
		return
	}

	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	log.Printf("fetch-mrs range %s - %s", monday.Format("2006-01-02"), nextMonday.Format("2006-01-02"))

	postEphemeral(api, cmd, fmt.Sprintf("Fetching merged MRs for %s to %s...",
		monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2")))

	mrs, err := FetchMRs(cfg, monday, nextMonday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error fetching MRs: %v", err))
		log.Printf("fetch-mrs error: %v", err)
		return
	}
	log.Printf("fetch-mrs fetched=%d", len(mrs))

	teamMembers := cfg.TeamMembers

	var newItems []WorkItem
	for _, mr := range mrs {
		if len(teamMembers) > 0 {
			if !anyNameMatches(teamMembers, mr.AuthorName) && !anyNameMatches(teamMembers, mr.Author) {
				log.Printf("fetch-mrs skipped non-team author=%s username=%s", mr.AuthorName, mr.Author)
				continue
			}
		}

		exists, err := SourceRefExists(db, mr.WebURL)
		if err != nil {
			log.Printf("Error checking MR existence: %v", err)
			continue
		}
		if exists {
			continue
		}

		newItems = append(newItems, WorkItem{
			Description: mr.Title,
			Author:      mr.AuthorName,
			Source:      "gitlab",
			SourceRef:   mr.WebURL,
			Status:      mapMRStatus(mr),
			ReportedAt:  mrReportedAt(mr, cfg.Location),
		})
	}

	if len(newItems) == 0 {
		postEphemeral(api, cmd, fmt.Sprintf("Found %d MRs (merged+open), all already tracked.", len(mrs)))
		log.Printf("fetch-mrs all tracked")
		return
	}

	inserted, err := InsertWorkItems(db, newItems)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error storing MRs: %v", err))
		log.Printf("fetch-mrs insert error: %v", err)
		return
	}

	postEphemeral(api, cmd, fmt.Sprintf("Fetched %d MRs (merged+open) (%d new, %d already tracked)",
		len(mrs), inserted, len(mrs)-inserted))
	log.Printf("fetch-mrs inserted=%d skipped=%d", inserted, len(mrs)-inserted)
}

func handleGenerateReport(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	isManager, err := isManagerUser(api, cfg, cmd.UserID)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error checking permissions: %v", err))
		log.Printf("generate-report auth error user=%s: %v", cmd.UserID, err)
		return
	}
	if !isManager {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		log.Printf("generate-report denied user=%s", cmd.UserID)
		return
	}

	mode := strings.TrimSpace(cmd.Text)
	if mode != "team" && mode != "boss" {
		mode = "team"
	}

	postEphemeral(api, cmd, fmt.Sprintf("Generating report (mode: %s)...", mode))
	log.Printf("generate-report mode=%s", mode)

	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	items, err := GetItemsByDateRange(db, monday, nextMonday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading items: %v", err))
		log.Printf("generate-report load error: %v", err)
		return
	}
	log.Printf("generate-report items=%d", len(items))

	if len(items) == 0 {
		postEphemeral(api, cmd, "No work items found for this week.")
		return
	}

	// Load recent corrections for LLM feedback loop.
	fourWeeksAgo := monday.AddDate(0, 0, -28)
	corrections, corrErr := GetRecentCorrections(db, fourWeeksAgo, 50)
	if corrErr != nil {
		log.Printf("generate-report corrections load error (non-fatal): %v", corrErr)
	}

	// Load historical classified items for TF-IDF example selection.
	twelveWeeksAgo := monday.AddDate(0, 0, -84)
	historicalItems, histErr := GetClassifiedItemsWithSections(db, twelveWeeksAgo, 500)
	if histErr != nil {
		log.Printf("generate-report historical items load error (non-fatal): %v", histErr)
	}

	result, err := BuildReportsFromLast(cfg, items, monday, corrections, historicalItems)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error building report: %v", err))
		log.Printf("report build error: %v", err)
		return
	}
	merged := result.Template
	llmUsage := result.Usage

	// Persist classification history (non-fatal on error).
	if len(result.Decisions) > 0 {
		optionLabels := make(map[string]string, len(result.Options))
		for _, opt := range result.Options {
			optionLabels[opt.ID] = opt.Label
		}
		var records []ClassificationRecord
		for itemID, dec := range result.Decisions {
			records = append(records, ClassificationRecord{
				WorkItemID:       itemID,
				SectionID:        dec.SectionID,
				SectionLabel:     optionLabels[dec.SectionID],
				Confidence:       dec.Confidence,
				NormalizedStatus: dec.NormalizedStatus,
				TicketIDs:        dec.TicketIDs,
				DuplicateOf:      dec.DuplicateOf,
				LLMProvider:      cfg.LLMProvider,
				LLMModel:         cfg.LLMModel,
			})
		}
		if err := InsertClassificationHistory(db, records); err != nil {
			log.Printf("generate-report history persist error (non-fatal): %v", err)
		} else {
			log.Printf("generate-report persisted %d classification records", len(records))
		}
	}

	var filePath string
	var fileTitle string
	if mode == "boss" {
		bossReport := renderBossMarkdown(merged)
		filePath, err = WriteEmailDraftFile(bossReport, cfg.ReportOutputDir, monday, cfg.TeamName)
		fileTitle = fmt.Sprintf("%s report email draft", cfg.TeamName)
		log.Printf("generate-report boss-report-length=%d file=%s", len(bossReport), filePath)
	} else {
		teamReport := renderTeamMarkdown(merged)
		filePath, err = WriteReportFile(teamReport, cfg.ReportOutputDir, monday, cfg.TeamName)
		fileTitle = fmt.Sprintf("%s team report", cfg.TeamName)
		log.Printf("generate-report team-report-length=%d file=%s", len(teamReport), filePath)
	}
	if err != nil {
		log.Printf("Error writing report file: %v", err)
		postEphemeral(api, cmd, fmt.Sprintf("Error writing report file: %v", err))
		return
	}
	log.Printf("generate-report file=%s mode=%s", filePath, mode)

	fi, err := os.Stat(filePath)
	if err != nil {
		log.Printf("Error stating report file: %v", err)
		postEphemeral(api, cmd, fmt.Sprintf("Error reading generated file: %v", err))
		return
	}
	if fi.Size() <= 0 {
		log.Printf("Error uploading report file: generated file is empty path=%s", filePath)
		postEphemeral(api, cmd, "Error uploading report file: generated file is empty.")
		return
	}

	tokenUsedText := formatTokenCount(llmUsage.TotalTokens())

	_, err = api.UploadFileV2(slack.UploadFileV2Parameters{
		File:           filePath,
		FileSize:       int(fi.Size()),
		Filename:       filepath.Base(filePath),
		Channel:        cmd.ChannelID,
		Title:          fileTitle,
		InitialComment: fmt.Sprintf("Generated report for week starting %s (mode: %s, tokens used: %s)", monday.Format("2006-01-02"), mode, tokenUsedText),
	})
	if err != nil {
		log.Printf("Error uploading report file: %v", err)
		postEphemeral(api, cmd, "Error uploading report file to channel. Check bot permissions.")
		return
	}

	msg := fmt.Sprintf("Report generated with %d items (mode: %s, tokens used: %s)", len(items), mode, tokenUsedText)
	if filePath != "" {
		msg += fmt.Sprintf("\nSaved to: %s", filePath)
	}
	postEphemeral(api, cmd, msg)
	log.Printf("generate-report done items=%d", len(items))

	// Uncertainty sampling: send messages for low-confidence items.
	sendUncertaintyMessages(api, cfg, cmd, result)
}

func formatTokenCount(tokens int64) string {
	if tokens < 1000 {
		return fmt.Sprintf("%d", tokens)
	}
	rounded := (tokens + 50) / 100
	whole := rounded / 10
	decimal := rounded % 10
	if decimal == 0 {
		return fmt.Sprintf("%dk", whole)
	}
	return fmt.Sprintf("%d.%dk", whole, decimal)
}

func handleListItems(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	renderListItems(api, db, cfg, cmd.ChannelID, cmd.UserID, 0)
}

func renderListItems(api *slack.Client, db *sql.DB, cfg Config, channelID, userID string, page int) {
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	items, err := GetItemsByDateRange(db, monday, nextMonday)
	if err != nil {
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("Error: %v", err))
		return
	}

	if len(items) == 0 {
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("No items for this week (%s - %s)",
			monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2")))
		log.Printf("list-items empty")
		return
	}

	// Precompute sort keys: lowercase first name from synthesized display name.
	sortKeys := make([]string, len(items))
	for idx := range items {
		if fields := strings.Fields(synthesizeName(items[idx].Author)); len(fields) > 0 {
			sortKeys[idx] = strings.ToLower(fields[0])
		}
	}
	// Sort: group by author (first name alphabetically), then by reported_at ascending.
	sort.SliceStable(items, func(i, j int) bool {
		if sortKeys[i] != sortKeys[j] {
			return sortKeys[i] < sortKeys[j]
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
	end := start + listItemsPageSize
	if end > len(items) {
		end = len(items)
	}

	blocks := []slack.Block{
		slack.NewHeaderBlock(
			slack.NewTextBlockObject(slack.PlainTextType,
				fmt.Sprintf("Items for %s - %s (%d total)",
					monday.Format("Jan 2"),
					nextMonday.AddDate(0, 0, -1).Format("Jan 2"),
					len(items)),
				false, false,
			),
		),
	}

	isManager, _ := isManagerUser(api, cfg, userID)
	user, _ := api.GetUserInfo(userID)
	for _, item := range items[start:end] {
		source := ""
		if item.Source == "gitlab" {
			source = " [GitLab]"
		}
		category := ""
		if item.Category != "" {
			category = fmt.Sprintf(" _%s_", item.Category)
		}
		text := fmt.Sprintf("*%s*: %s (%s)%s%s",
			item.Author, item.Description, item.Status, source, category)
		if canManageItem(item, isManager, user) {
			editOpt := slack.NewOptionBlockObject(
				fmt.Sprintf("edit:%d", item.ID),
				slack.NewTextBlockObject(slack.PlainTextType, "Edit", false, false),
				nil,
			)
			deleteOpt := slack.NewOptionBlockObject(
				fmt.Sprintf("delete:%d", item.ID),
				slack.NewTextBlockObject(slack.PlainTextType, "Delete", false, false),
				nil,
			)
			menu := slack.NewOverflowBlockElement(actionRowMenu, editOpt, deleteOpt)
			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
				nil,
				slack.NewAccessory(menu),
			))
		} else {
			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
				nil,
				nil,
			))
		}
	}

	if len(items) > listItemsPageSize {
		var nav []slack.BlockElement
		if page > 0 {
			nav = append(nav, slack.NewButtonBlockElement(
				actionPagePrev,
				strconv.Itoa(page-1),
				slack.NewTextBlockObject(slack.PlainTextType, "Prev", false, false),
			))
		}
		if end < len(items) {
			nav = append(nav, slack.NewButtonBlockElement(
				actionPageNext,
				strconv.Itoa(page+1),
				slack.NewTextBlockObject(slack.PlainTextType, "Next", false, false),
			))
		}
		if len(nav) > 0 {
			blocks = append(blocks, slack.NewActionBlock("list_items_nav", nav...))
		}
	}

	_, err = api.PostEphemeral(channelID, userID, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		log.Printf("Error posting list-items blocks: %v", err)
		postEphemeralTo(api, channelID, userID, "Error rendering list items.")
		return
	}
	log.Printf("list-items count=%d page=%d", len(items), page)
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
	authors, err := GetSlackAuthorsByDateRange(db, monday, nextMonday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading items: %v", err))
		log.Printf("list-missing load error: %v", err)
		return
	}

	var reported []string
	for author := range authors {
		reported = append(reported, author)
	}

	type missingMember struct {
		display string
		userID  string
	}
	var missing []missingMember
	var missingIDs []string
	for _, uid := range memberIDs {
		user, err := api.GetUserInfo(uid)
		if err != nil {
			missing = append(missing, missingMember{display: uid, userID: uid})
			missingIDs = append(missingIDs, uid)
			continue
		}

		candidates := []string{user.Name, user.RealName, user.Profile.DisplayName}
		hasReported := false
		for _, c := range candidates {
			if c != "" && anyNameMatches(reported, c) {
				hasReported = true
				break
			}
		}
		if !hasReported {
			display := user.Profile.DisplayName
			if display == "" {
				display = user.RealName
			}
			if display == "" {
				display = user.Name
			}
			if display == "" {
				display = uid
			}
			missing = append(missing, missingMember{display: display, userID: uid})
			missingIDs = append(missingIDs, uid)
		}
	}

	if len(missing) == 0 && len(unresolved) == 0 {
		postEphemeral(api, cmd, fmt.Sprintf("Everyone has reported this week (%s - %s).",
			monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2")))
		log.Printf("list-missing none")
		return
	}

	blocks := []slack.Block{
		slack.NewHeaderBlock(
			slack.NewTextBlockObject(slack.PlainTextType,
				fmt.Sprintf("Missing reports for %s - %s (%d)",
					monday.Format("Jan 2"),
					nextMonday.AddDate(0, 0, -1).Format("Jan 2"),
					len(missing)+len(unresolved)),
				false, false),
		),
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

	_, err = api.PostEphemeral(cmd.ChannelID, cmd.UserID, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		log.Printf("Error posting list-missing blocks: %v", err)
		postEphemeral(api, cmd, "Error rendering missing members list.")
		return
	}
	log.Printf("list-missing count=%d", len(missing)+len(unresolved))
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

	switch act.ActionID {
	case actionPagePrev, actionPageNext:
		page, err := strconv.Atoi(strings.TrimSpace(act.Value))
		if err != nil {
			page = 0
		}
		renderListItems(api, db, cfg, channelID, userID, page)
	case actionDeleteItem:
		itemID, err := strconv.ParseInt(strings.TrimSpace(act.Value), 10, 64)
		if err != nil {
			postEphemeralTo(api, channelID, userID, "Invalid item id.")
			return
		}
		deleteItemAction(api, db, cfg, channelID, userID, itemID)
	case actionEditItemOpen:
		itemID, err := strconv.ParseInt(strings.TrimSpace(act.Value), 10, 64)
		if err != nil {
			postEphemeralTo(api, channelID, userID, "Invalid item id.")
			return
		}
		openEditModal(api, db, cfg, cb.TriggerID, channelID, userID, itemID)
	case actionUncertaintySelect:
		handleUncertaintySelect(api, db, cfg, cb, act)
		return
	case actionUncertaintyOther:
		itemID, err := strconv.ParseInt(strings.TrimSpace(act.Value), 10, 64)
		if err != nil {
			postEphemeralTo(api, channelID, userID, "Invalid item id.")
			return
		}
		openEditModal(api, db, cfg, cb.TriggerID, channelID, userID, itemID)
		return
	case actionNudgeMember:
		openNudgeConfirmModal(api, cfg, cb.TriggerID, channelID, act.Value)
		return
	case actionNudgeAll:
		openNudgeConfirmModal(api, cfg, cb.TriggerID, channelID, act.Value)
		return
	case actionRetroApply:
		handleRetroApply(api, db, cfg, cb, act)
		return
	case actionRetroDismiss:
		channelForMsg := channelID
		if channelForMsg == "" {
			channelForMsg = cb.Container.ChannelID
		}
		postEphemeralTo(api, channelForMsg, userID, "Suggestion dismissed.")
		return
	case actionRowMenu:
		val := strings.TrimSpace(act.SelectedOption.Value)
		if val == "" {
			val = strings.TrimSpace(act.Value)
		}
		if strings.HasPrefix(val, "edit:") {
			itemID, err := strconv.ParseInt(strings.TrimPrefix(val, "edit:"), 10, 64)
			if err != nil {
				postEphemeralTo(api, channelID, userID, "Invalid item id.")
				return
			}
			openEditModal(api, db, cfg, cb.TriggerID, channelID, userID, itemID)
			return
		}
		if strings.HasPrefix(val, "delete:") {
			itemID, err := strconv.ParseInt(strings.TrimPrefix(val, "delete:"), 10, 64)
			if err != nil {
				postEphemeralTo(api, channelID, userID, "Invalid item id.")
				return
			}
			openDeleteModal(api, db, cfg, cb.TriggerID, channelID, userID, itemID)
			return
		}
	}
}

func handleViewSubmission(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback) {
	if cb.View.CallbackID == modalNudgeConfirmCallback {
		handleNudgeConfirm(api, cfg, cb)
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
		itemID, err := strconv.ParseInt(strings.TrimPrefix(parts[0], modalMetaPrefix), 10, 64)
		if err != nil {
			return
		}
		deleteItemAction(api, db, cfg, channelID, userID, itemID)
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
	itemID, err := strconv.ParseInt(strings.TrimPrefix(parts[0], modalMetaPrefix), 10, 64)
	if err != nil {
		return
	}
	if cb.View.State == nil {
		return
	}
	values := cb.View.State.Values
	if values == nil {
		return
	}
	descAction := values[editBlockDescription][editActionDescription]
	statusAction := values[editBlockStatus][editActionStatus]
	description := strings.TrimSpace(descAction.Value)
	status := strings.TrimSpace(statusAction.SelectedOption.Value)
	if status == "" {
		status = "done"
	}

	item, err := GetWorkItemByID(db, itemID)
	if err != nil {
		log.Printf("edit modal load item error id=%d: %v", itemID, err)
		return
	}

	// Preserve original free-text status when "other" is selected.
	if status == "other" {
		status = item.Status
	}
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	if !itemInRange(item, monday, nextMonday) {
		return
	}
	isManager, _ := isManagerUser(api, cfg, userID)
	user, _ := api.GetUserInfo(userID)
	if !canManageItem(item, isManager, user) {
		return
	}
	if description == "" {
		return
	}

	if err := UpdateWorkItemTextAndStatus(db, itemID, description, status); err != nil {
		log.Printf("edit modal update error id=%d: %v", itemID, err)
		return
	}

	// Check for category change and record correction.
	if catBlock, ok := values[editBlockCategory]; ok {
		if catAction, ok2 := catBlock[editActionCategory]; ok2 {
			newCategoryID := strings.TrimSpace(catAction.SelectedOption.Value)
			if newCategoryID != "" && newCategoryID != item.Category {
				recordCategoryCorrection(db, cfg, item, newCategoryID, userID)
				if err := UpdateWorkItemCategory(db, itemID, newCategoryID); err != nil {
					log.Printf("edit modal category update error id=%d: %v", itemID, err)
				}
			}
		}
	}

	if channelID == "" {
		channelID = cb.Container.ChannelID
	}
	if channelID == "" {
		channelID = cb.Channel.ID
	}
	renderListItems(api, db, cfg, channelID, userID, 0)
}

func deleteItemAction(api *slack.Client, db *sql.DB, cfg Config, channelID, userID string, itemID int64) {
	item, err := GetWorkItemByID(db, itemID)
	if err != nil {
		postEphemeralTo(api, channelID, userID, "Item not found.")
		return
	}
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	if !itemInRange(item, monday, nextMonday) {
		postEphemeralTo(api, channelID, userID, "You can only modify this week's items.")
		return
	}

	isManager, _ := isManagerUser(api, cfg, userID)
	user, _ := api.GetUserInfo(userID)
	if !canManageItem(item, isManager, user) {
		postEphemeralTo(api, channelID, userID, "You are not allowed to delete this item.")
		return
	}

	if err := DeleteWorkItemByID(db, itemID); err != nil {
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("Delete failed: %v", err))
		return
	}
	renderListItems(api, db, cfg, channelID, userID, 0)
}

func openEditModal(api *slack.Client, db *sql.DB, cfg Config, triggerID, channelID, userID string, itemID int64) {
	item, err := GetWorkItemByID(db, itemID)
	if err != nil {
		postEphemeralTo(api, channelID, userID, "Item not found.")
		return
	}
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	if !itemInRange(item, monday, nextMonday) {
		postEphemeralTo(api, channelID, userID, "You can only modify this week's items.")
		return
	}

	isManager, _ := isManagerUser(api, cfg, userID)
	user, _ := api.GetUserInfo(userID)
	if !canManageItem(item, isManager, user) {
		postEphemeralTo(api, channelID, userID, "You are not allowed to edit this item.")
		return
	}

	descInput := slack.NewPlainTextInputBlockElement(
		slack.NewTextBlockObject(slack.PlainTextType, "Description", false, false),
		editActionDescription,
	).WithInitialValue(item.Description)
	statusOptions := []*slack.OptionBlockObject{
		slack.NewOptionBlockObject("done", slack.NewTextBlockObject(slack.PlainTextType, "done", false, false), nil),
		slack.NewOptionBlockObject("in testing", slack.NewTextBlockObject(slack.PlainTextType, "in testing", false, false), nil),
		slack.NewOptionBlockObject("in progress", slack.NewTextBlockObject(slack.PlainTextType, "in progress", false, false), nil),
	}
	cur := normalizeStatus(item.Status)
	if cur == "" {
		cur = "done"
	}
	// If the current status is a free-text value, add it as a custom option.
	if cur != "done" && cur != "in testing" && cur != "in progress" {
		displayStatus := item.Status
		runes := []rune(displayStatus)
		if len(runes) > 70 {
			displayStatus = string(runes[:70]) + "..."
		}
		statusOptions = append(statusOptions,
			slack.NewOptionBlockObject("other", slack.NewTextBlockObject(slack.PlainTextType, displayStatus, false, false), nil),
		)
	}
	statusSelect := slack.NewOptionsSelectBlockElement(
		slack.OptTypeStatic,
		slack.NewTextBlockObject(slack.PlainTextType, "Status", false, false),
		editActionStatus,
		statusOptions...,
	)
	found := false
	for _, o := range statusOptions {
		if o.Value == cur {
			statusSelect.InitialOption = o
			found = true
			break
		}
	}
	if !found && len(statusOptions) == 4 {
		// Pre-select the custom status option.
		statusSelect.InitialOption = statusOptions[3]
	} else if !found {
		statusSelect.InitialOption = statusOptions[0]
	}

	// Build category dropdown from template sections.
	var categoryBlock slack.Block
	sectionOpts := loadSectionOptionsForModal(cfg)
	if len(sectionOpts) > 0 {
		noChangeOpt := slack.NewOptionBlockObject(
			"",
			slack.NewTextBlockObject(slack.PlainTextType, "(no change)", false, false),
			nil,
		)
		catOptions := []*slack.OptionBlockObject{noChangeOpt}
		var initialCatOpt *slack.OptionBlockObject
		for _, so := range sectionOpts {
			label := so.Label
			if len(label) > 75 {
				label = label[:72] + "..."
			}
			opt := slack.NewOptionBlockObject(
				so.ID,
				slack.NewTextBlockObject(slack.PlainTextType, label, false, false),
				nil,
			)
			catOptions = append(catOptions, opt)
			if so.ID == item.Category {
				initialCatOpt = opt
			}
		}
		catSelect := slack.NewOptionsSelectBlockElement(
			slack.OptTypeStatic,
			slack.NewTextBlockObject(slack.PlainTextType, "Category", false, false),
			editActionCategory,
			catOptions...,
		)
		if initialCatOpt != nil {
			catSelect.InitialOption = initialCatOpt
		} else {
			catSelect.InitialOption = noChangeOpt
		}
		categoryBlock = slack.NewInputBlock(
			editBlockCategory,
			slack.NewTextBlockObject(slack.PlainTextType, "Category", false, false),
			nil,
			catSelect,
		)
	}

	blocks := []slack.Block{
		slack.NewInputBlock(
			editBlockDescription,
			slack.NewTextBlockObject(slack.PlainTextType, "Description", false, false),
			nil,
			descInput,
		),
		slack.NewInputBlock(
			editBlockStatus,
			slack.NewTextBlockObject(slack.PlainTextType, "Status", false, false),
			nil,
			statusSelect,
		),
	}
	if categoryBlock != nil {
		blocks = append(blocks, categoryBlock)
	}

	view := slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Edit item", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Save", false, false),
		CallbackID:      modalEditCallbackID,
		PrivateMetadata: fmt.Sprintf("%s%d|%s", modalMetaPrefix, itemID, channelID),
		Blocks:          slack.Blocks{BlockSet: blocks},
	}
	if _, err := api.OpenView(triggerID, view); err != nil {
		postEphemeralTo(api, channelID, userID, fmt.Sprintf("Unable to open edit dialog: %v", err))
	}
}

func openDeleteModal(api *slack.Client, db *sql.DB, cfg Config, triggerID, channelID, userID string, itemID int64) {
	item, err := GetWorkItemByID(db, itemID)
	if err != nil {
		postEphemeralTo(api, channelID, userID, "Item not found.")
		return
	}
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	if !itemInRange(item, monday, nextMonday) {
		postEphemeralTo(api, channelID, userID, "You can only modify this week's items.")
		return
	}
	isManager, _ := isManagerUser(api, cfg, userID)
	user, _ := api.GetUserInfo(userID)
	if !canManageItem(item, isManager, user) {
		postEphemeralTo(api, channelID, userID, "You are not allowed to delete this item.")
		return
	}

	view := slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Delete item", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Delete", false, false),
		CallbackID:      modalDeleteCallbackID,
		PrivateMetadata: fmt.Sprintf("%s%d|%s", modalMetaPrefix, itemID, channelID),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject(
					slack.MarkdownType,
					fmt.Sprintf("Delete this item?\n\n*%s*: %s (%s)", item.Author, item.Description, item.Status),
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

func itemInRange(item WorkItem, from, to time.Time) bool {
	return !item.ReportedAt.Before(from) && item.ReportedAt.Before(to)
}

func canManageItem(item WorkItem, isManager bool, user *slack.User) bool {
	if isManager {
		return true
	}
	if user == nil {
		return false
	}
	// Prefer immutable Slack user ID when available.
	if item.AuthorID != "" {
		return user.ID == item.AuthorID
	}
	// Fallback: fuzzy name matching for legacy items without AuthorID.
	candidates := []string{
		strings.TrimSpace(user.Profile.DisplayName),
		strings.TrimSpace(user.RealName),
		strings.TrimSpace(user.Name),
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(item.Author), c) || nameMatches(c, item.Author) || nameMatches(item.Author, c) {
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

func handleNudgeConfirm(api *slack.Client, cfg Config, cb slack.InteractionCallback) {
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

	sendNudges(api, cfg, targetIDs, cfg.ReportChannelID)
	postEphemeralTo(api, channelID, userID, fmt.Sprintf("Sent nudge to %d member(s).", len(targetIDs)))
	log.Printf("nudge sent from /check user=%s count=%d", userID, len(targetIDs))
}

func handleReportStats(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	isManager, err := isManagerUser(api, cfg, cmd.UserID)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error checking permissions: %v", err))
		log.Printf("report-stats auth error user=%s: %v", cmd.UserID, err)
		return
	}
	if !isManager {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		log.Printf("report-stats denied user=%s", cmd.UserID)
		return
	}

	// Load all-time stats.
	allTimeStats, err := GetClassificationStats(db, time.Time{})
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading stats: %v", err))
		log.Printf("report-stats all-time error: %v", err)
		return
	}

	// Load last 4 weeks stats.
	fourWeeksAgo := time.Now().In(cfg.Location).AddDate(0, 0, -28)
	recentStats, err := GetClassificationStats(db, fourWeeksAgo)
	if err != nil {
		log.Printf("report-stats recent error (non-fatal): %v", err)
		recentStats = ClassificationStats{}
	}

	// Load per-section corrections.
	sectionCorr, err := GetCorrectionsBySection(db, fourWeeksAgo)
	if err != nil {
		log.Printf("report-stats section corrections error (non-fatal): %v", err)
	}

	// Load 8-week trend.
	eightWeeksAgo := time.Now().In(cfg.Location).AddDate(0, 0, -56)
	trends, err := GetWeeklyClassificationTrend(db, eightWeeksAgo)
	if err != nil {
		log.Printf("report-stats trend error (non-fatal): %v", err)
	}

	var sb strings.Builder
	sb.WriteString("*Classification Accuracy Dashboard*\n\n")

	// Overview.
	sb.WriteString("*All-time Overview*\n")
	sb.WriteString(fmt.Sprintf("- Classifications: %d\n", allTimeStats.TotalClassifications))
	sb.WriteString(fmt.Sprintf("- Corrections: %d\n", allTimeStats.TotalCorrections))
	if allTimeStats.TotalClassifications > 0 {
		accuracy := 100.0 * float64(allTimeStats.TotalClassifications-allTimeStats.TotalCorrections) / float64(allTimeStats.TotalClassifications)
		if accuracy < 0 {
			accuracy = 0
		}
		sb.WriteString(fmt.Sprintf("- Accuracy: %.1f%%\n", accuracy))
		sb.WriteString(fmt.Sprintf("- Avg confidence: %.2f\n", allTimeStats.AvgConfidence))
	}

	sb.WriteString(fmt.Sprintf("\n*Last 4 Weeks*\n"))
	sb.WriteString(fmt.Sprintf("- Classifications: %d\n", recentStats.TotalClassifications))
	sb.WriteString(fmt.Sprintf("- Corrections: %d\n", recentStats.TotalCorrections))
	if recentStats.TotalClassifications > 0 {
		accuracy := 100.0 * float64(recentStats.TotalClassifications-recentStats.TotalCorrections) / float64(recentStats.TotalClassifications)
		if accuracy < 0 {
			accuracy = 0
		}
		sb.WriteString(fmt.Sprintf("- Accuracy: %.1f%%\n", accuracy))
		sb.WriteString(fmt.Sprintf("- Avg confidence: %.2f\n", recentStats.AvgConfidence))
	}

	// Confidence distribution.
	sb.WriteString(fmt.Sprintf("\n*Confidence Distribution (last 4 weeks)*\n"))
	sb.WriteString(fmt.Sprintf("- <50%%: %d\n", recentStats.BucketBelow50))
	sb.WriteString(fmt.Sprintf("- 50-70%%: %d\n", recentStats.Bucket50to70))
	sb.WriteString(fmt.Sprintf("- 70-90%%: %d\n", recentStats.Bucket70to90))
	sb.WriteString(fmt.Sprintf("- 90%%+: %d\n", recentStats.Bucket90Plus))

	// Most corrected sections.
	if len(sectionCorr) > 0 {
		sb.WriteString(fmt.Sprintf("\n*Most Corrected Sections (last 4 weeks)*\n"))
		for _, sc := range sectionCorr {
			label := sc.OriginalSectionID
			if sc.OriginalLabel != "" {
				label = fmt.Sprintf("%s (%s)", sc.OriginalSectionID, sc.OriginalLabel)
			}
			sb.WriteString(fmt.Sprintf("- %s: %d corrections\n", label, sc.CorrectionCount))
		}
	}

	// Weekly trend.
	if len(trends) > 0 {
		sb.WriteString(fmt.Sprintf("\n*Weekly Trend (last 8 weeks)*\n"))
		for _, t := range trends {
			sb.WriteString(fmt.Sprintf("- %s: %d classified, %d corrected, avg conf %.2f\n",
				t.WeekStart, t.Classifications, t.Corrections, t.AvgConfidence))
		}
	}

	postEphemeral(api, cmd, sb.String())
	log.Printf("report-stats sent user=%s", cmd.UserID)
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
		"`/report <description> (status)` — Report a work item.",
		"`/rpt` — Alias of `/report`.",
		">*Example:* `/report [mantis_id] Add pagination to user list API (done)`",
		">*Multiline* (each with own status, use Shift+Enter for newlines):",
		">```/report Item A (in progress)",
		">Item B (done)```",
		">*Shared status* (last line applies to all items):",
		">```/report Item A",
		">Item B",
		">(in progress)```",
		"",
		"`/list` — List this week's items.",
		"`/help` — Show this help.",
	}

	if isManager {
		lines = append(lines,
			"",
			"*Manager Commands*",
			"",
			"`/fetch-mrs` — Fetch *merged + open* GitLab MRs for this week.",
			"`/generate-report team|boss` — Generate weekly report.",
			"`/gen` — Alias of `/generate-report`.",
			"`/check` — List missing members with inline nudge buttons.",
			"`/retrospective` — Analyze recent corrections and suggest improvements.",
			"`/report-stats` — Show classification accuracy dashboard.",
		)
	}

	postEphemeral(api, cmd, strings.Join(lines, "\n"))
}

func isManagerUser(_ *slack.Client, cfg Config, userID string) (bool, error) {
	return cfg.IsManagerID(userID), nil
}


func resolveDelegatedAuthorName(input string, teamMembers []string) string {
	input = strings.TrimSpace(input)
	if input == "" || len(teamMembers) == 0 {
		return input
	}

	normalizedInput := normalizeTextToken(input)

	// 1) Exact match first.
	for _, member := range teamMembers {
		if normalizeTextToken(member) == normalizedInput {
			return member
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
		return matches[0]
	}

	// Ambiguous/no match: keep caller input unchanged.
	return input
}

func mapMRStatus(mr GitLabMR) string {
	if mr.State == "opened" {
		return "in progress"
	}
	return "done"
}

func mrReportedAt(mr GitLabMR, loc *time.Location) time.Time {
	if mr.State == "opened" && !mr.UpdatedAt.IsZero() {
		return mr.UpdatedAt
	}
	if !mr.MergedAt.IsZero() {
		return mr.MergedAt
	}
	if !mr.CreatedAt.IsZero() {
		return mr.CreatedAt
	}
	return time.Now().In(loc)
}

// --- Correction helpers ---

func loadSectionOptionsForModal(cfg Config) []sectionOption {
	template, _, err := loadTemplateForGeneration(cfg.ReportOutputDir, cfg.TeamName, time.Now().In(cfg.Location))
	if err != nil {
		log.Printf("edit modal load template error (non-fatal): %v", err)
		return nil
	}
	return templateOptions(template)
}

func recordCategoryCorrection(db *sql.DB, cfg Config, item WorkItem, newCategoryID, userID string) {
	originalSectionID := item.Category
	originalLabel := ""

	// Try to get the LLM's original classification.
	if hist, err := GetLatestClassification(db, item.ID); err == nil {
		originalSectionID = hist.SectionID
		originalLabel = hist.SectionLabel
	}

	// Resolve new section label.
	correctedLabel := ""
	sectionOpts := loadSectionOptionsForModal(cfg)
	for _, so := range sectionOpts {
		if so.ID == newCategoryID {
			correctedLabel = so.Label
			break
		}
	}

	correction := ClassificationCorrection{
		WorkItemID:         item.ID,
		OriginalSectionID:  originalSectionID,
		OriginalLabel:      originalLabel,
		CorrectedSectionID: newCategoryID,
		CorrectedLabel:     correctedLabel,
		Description:        item.Description,
		CorrectedBy:        userID,
	}
	if err := InsertClassificationCorrection(db, correction); err != nil {
		log.Printf("correction insert error id=%d: %v", item.ID, err)
		return
	}
	log.Printf("correction recorded item=%d from=%s to=%s by=%s", item.ID, originalSectionID, newCategoryID, userID)

	// Auto-grow glossary if configured.
	tryAutoGrowGlossary(db, cfg, item.Description, newCategoryID, correctedLabel)
}

func tryAutoGrowGlossary(db *sql.DB, cfg Config, description, sectionID, sectionLabel string) {
	if strings.TrimSpace(cfg.LLMGlossaryPath) == "" {
		return
	}
	count, err := CountCorrectionsByPhrase(db, description, sectionID)
	if err != nil {
		log.Printf("glossary auto-grow count error: %v", err)
		return
	}
	if count < 2 {
		return
	}
	phrase := extractGlossaryPhrase(description)
	if phrase == "" {
		return
	}
	section := sectionLabel
	if section == "" {
		section = sectionID
	}
	if err := AppendGlossaryTerm(cfg.LLMGlossaryPath, phrase, section); err != nil {
		log.Printf("glossary auto-grow error: %v", err)
		return
	}
	log.Printf("glossary auto-grown phrase=%q section=%s", phrase, section)
}

// --- Uncertainty sampling ---

func sendUncertaintyMessages(api *slack.Client, cfg Config, cmd slack.SlashCommand, result BuildResult) {
	if len(result.Decisions) == 0 || len(result.Options) == 0 {
		return
	}

	threshold := cfg.LLMConfidence
	if threshold <= 0 || threshold > 1 {
		threshold = 0.70
	}

	type uncertainItem struct {
		itemID     int64
		decision   LLMSectionDecision
		confidence float64
	}
	var uncertain []uncertainItem
	for itemID, dec := range result.Decisions {
		if dec.Confidence > 0 && dec.Confidence < threshold {
			uncertain = append(uncertain, uncertainItem{itemID: itemID, decision: dec, confidence: dec.Confidence})
		}
	}

	if len(uncertain) == 0 || len(uncertain) > 10 {
		return
	}

	optionLabels := make(map[string]string, len(result.Options))
	for _, opt := range result.Options {
		optionLabels[opt.ID] = opt.Label
	}

	for _, u := range uncertain {
		bestGuess := u.decision.SectionID
		if label, ok := optionLabels[bestGuess]; ok {
			bestGuess = label
		}

		headerText := fmt.Sprintf("Uncertain classification (%.0f%% confidence)\nItem ID: %d\nBest guess: %s", u.confidence*100, u.itemID, bestGuess)

		// Build section buttons (up to 4 most common sections).
		var buttons []slack.BlockElement
		limit := 4
		if len(result.Options) < limit {
			limit = len(result.Options)
		}
		for i := 0; i < limit; i++ {
			opt := result.Options[i]
			label := opt.Label
			if len(label) > 30 {
				label = label[:27] + "..."
			}
			buttons = append(buttons, slack.NewButtonBlockElement(
				actionUncertaintySelect,
				fmt.Sprintf("%d:%s", u.itemID, opt.ID),
				slack.NewTextBlockObject(slack.PlainTextType, label, false, false),
			))
		}
		buttons = append(buttons, slack.NewButtonBlockElement(
			actionUncertaintyOther,
			fmt.Sprintf("%d", u.itemID),
			slack.NewTextBlockObject(slack.PlainTextType, "Other...", false, false),
		))

		blocks := []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, headerText, false, false),
				nil, nil,
			),
			slack.NewActionBlock("", buttons...),
		}

		_, err := api.PostEphemeral(cmd.ChannelID, cmd.UserID, slack.MsgOptionBlocks(blocks...))
		if err != nil {
			log.Printf("uncertainty message error item=%d: %v", u.itemID, err)
		}
	}
	log.Printf("uncertainty messages sent count=%d", len(uncertain))
}

func handleUncertaintySelect(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, act *slack.BlockAction) {
	channelID := cb.Channel.ID
	if channelID == "" {
		channelID = cb.Container.ChannelID
	}
	userID := cb.User.ID

	// Parse "itemID:sectionID" from button value.
	parts := strings.SplitN(strings.TrimSpace(act.Value), ":", 2)
	if len(parts) != 2 {
		postEphemeralTo(api, channelID, userID, "Invalid selection.")
		return
	}
	itemID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		postEphemeralTo(api, channelID, userID, "Invalid item id.")
		return
	}
	sectionID := parts[1]

	item, err := GetWorkItemByID(db, itemID)
	if err != nil {
		postEphemeralTo(api, channelID, userID, "Item not found.")
		return
	}

	recordCategoryCorrection(db, cfg, item, sectionID, userID)
	if err := UpdateWorkItemCategory(db, itemID, sectionID); err != nil {
		log.Printf("uncertainty category update error id=%d: %v", itemID, err)
	}

	postEphemeralTo(api, channelID, userID, fmt.Sprintf("Item %d reclassified to %s.", itemID, sectionID))
}

// --- Retrospective ---

func handleRetrospective(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	isManager, err := isManagerUser(api, cfg, cmd.UserID)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error checking permissions: %v", err))
		log.Printf("retrospective auth error user=%s: %v", cmd.UserID, err)
		return
	}
	if !isManager {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		log.Printf("retrospective denied user=%s", cmd.UserID)
		return
	}

	fourWeeksAgo := time.Now().In(cfg.Location).AddDate(0, 0, -28)
	corrections, err := GetRecentCorrections(db, fourWeeksAgo, 200)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading corrections: %v", err))
		log.Printf("retrospective load error: %v", err)
		return
	}

	if len(corrections) == 0 {
		postEphemeral(api, cmd, "No corrections found in the last 4 weeks.")
		return
	}

	postEphemeral(api, cmd, fmt.Sprintf("Analyzing %d corrections from the last 4 weeks...", len(corrections)))

	sectionOpts := loadSectionOptionsForModal(cfg)
	suggestions, usage, err := analyzeCorrections(cfg, corrections, sectionOpts)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error analyzing corrections: %v", err))
		log.Printf("retrospective analysis error: %v", err)
		return
	}

	if len(suggestions) == 0 {
		postEphemeral(api, cmd, fmt.Sprintf("No actionable patterns found (tokens used: %s).", formatTokenCount(usage.TotalTokens())))
		return
	}

	for i, suggestion := range suggestions {
		var actionDesc string
		switch suggestion.Action {
		case "glossary_term":
			actionDesc = fmt.Sprintf("Add glossary term: \"%s\" -> %s", suggestion.Phrase, suggestion.Section)
		case "guide_update":
			text := suggestion.GuideText
			if len(text) > 200 {
				text = text[:200] + "..."
			}
			actionDesc = fmt.Sprintf("Add guide rule: %s", text)
		default:
			actionDesc = suggestion.Action
		}

		text := fmt.Sprintf("*Suggestion %d: %s*\n%s\n_%s_", i+1, suggestion.Title, suggestion.Reasoning, actionDesc)

		applyBtn := slack.NewButtonBlockElement(
			actionRetroApply,
			fmt.Sprintf("%d", i),
			slack.NewTextBlockObject(slack.PlainTextType, "Apply", false, false),
		)
		dismissBtn := slack.NewButtonBlockElement(
			actionRetroDismiss,
			fmt.Sprintf("%d", i),
			slack.NewTextBlockObject(slack.PlainTextType, "Dismiss", false, false),
		)

		blocks := []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
				nil, nil,
			),
			slack.NewActionBlock("", applyBtn, dismissBtn),
		}

		_, postErr := api.PostEphemeral(cmd.ChannelID, cmd.UserID, slack.MsgOptionBlocks(blocks...))
		if postErr != nil {
			log.Printf("retrospective message error suggestion=%d: %v", i, postErr)
		}
	}

	postEphemeral(api, cmd, fmt.Sprintf("Found %d suggestions (tokens used: %s).", len(suggestions), formatTokenCount(usage.TotalTokens())))
	log.Printf("retrospective done suggestions=%d tokens=%d", len(suggestions), usage.TotalTokens())
}

func handleRetroApply(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, act *slack.BlockAction) {
	channelID := cb.Channel.ID
	if channelID == "" {
		channelID = cb.Container.ChannelID
	}
	userID := cb.User.ID

	// The button value is the suggestion index. We need to re-derive the suggestion
	// from the message text since we don't persist them.
	// Parse the action description from the message blocks.
	var actionLine string
	for _, block := range cb.Message.Blocks.BlockSet {
		section, ok := block.(*slack.SectionBlock)
		if !ok {
			continue
		}
		if section.Text != nil {
			actionLine = section.Text.Text
		}
	}

	if strings.Contains(actionLine, "Add glossary term:") {
		// Extract phrase and section from the action line.
		// Format: Add glossary term: "phrase" -> section
		idx := strings.Index(actionLine, "Add glossary term:")
		if idx >= 0 {
			rest := strings.TrimSpace(actionLine[idx+len("Add glossary term:"):])
			// Remove italic markers
			rest = strings.TrimPrefix(rest, "_")
			rest = strings.TrimSuffix(rest, "_")
			parts := strings.SplitN(rest, "->", 2)
			if len(parts) == 2 {
				phrase := strings.Trim(strings.TrimSpace(parts[0]), "\"")
				section := strings.TrimSpace(parts[1])
				if cfg.LLMGlossaryPath != "" && phrase != "" && section != "" {
					if err := AppendGlossaryTerm(cfg.LLMGlossaryPath, phrase, section); err != nil {
						postEphemeralTo(api, channelID, userID, fmt.Sprintf("Error applying glossary term: %v", err))
						return
					}
					postEphemeralTo(api, channelID, userID, fmt.Sprintf("Applied: glossary term \"%s\" -> %s", phrase, section))
					log.Printf("retrospective applied glossary phrase=%q section=%s", phrase, section)
					return
				}
			}
		}
		postEphemeralTo(api, channelID, userID, "Could not apply glossary term (glossary path not configured or parse error).")
		return
	}

	if strings.Contains(actionLine, "Add guide rule:") {
		idx := strings.Index(actionLine, "Add guide rule:")
		if idx >= 0 {
			rest := strings.TrimSpace(actionLine[idx+len("Add guide rule:"):])
			rest = strings.TrimPrefix(rest, "_")
			rest = strings.TrimSuffix(rest, "_")
			if strings.HasSuffix(rest, "...") {
				postEphemeralTo(api, channelID, userID, "Guide text was truncated. Please add manually.")
				return
			}
			guidePath := cfg.LLMGuidePath
			if guidePath != "" && rest != "" {
				if err := appendToFile(guidePath, "\n"+rest+"\n"); err != nil {
					postEphemeralTo(api, channelID, userID, fmt.Sprintf("Error appending to guide: %v", err))
					return
				}
				postEphemeralTo(api, channelID, userID, fmt.Sprintf("Applied: guide rule appended to %s", guidePath))
				log.Printf("retrospective applied guide update path=%s", guidePath)
				return
			}
		}
		postEphemeralTo(api, channelID, userID, "Could not apply guide update (guide path not configured or parse error).")
		return
	}

	postEphemeralTo(api, channelID, userID, "Unknown suggestion action type.")
}

func appendToFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}
