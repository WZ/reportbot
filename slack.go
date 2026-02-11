package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
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
)

func StartSlackBot(cfg Config, db *sql.DB, api *slack.Client) error {
	client := socketmode.New(api)

	go func() {
		for evt := range client.Events {
			switch evt.Type {
			case socketmode.EventTypeSlashCommand:
				cmd, ok := evt.Data.(slack.SlashCommand)
				if !ok {
					continue
				}
				log.Printf("Slash command received: %s from user=%s channel=%s", cmd.Command, cmd.UserID, cmd.ChannelID)
				client.Ack(*evt.Request)
				go handleSlashCommand(client, api, db, cfg, cmd)
			case socketmode.EventTypeInteractive:
				callback, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					continue
				}
				client.Ack(*evt.Request)
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
	case "/list-missing":
		handleListMissing(api, db, cfg, cmd)
	case "/nudge":
		handleNudge(api, db, cfg, cmd)
	case "/help":
		handleHelp(api, cfg, cmd)
	}
}

func handleReport(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	text := strings.TrimSpace(cmd.Text)
	if text == "" {
		postEphemeral(api, cmd, "Usage: /report <description> (status)\nExample: /report [mantis_id] Add pagination to user list API (done)")
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
	if match := delegatedAuthorRegex.FindStringSubmatch(text); len(match) > 1 {
		if cfg.IsManagerName(author) {
			delegated := strings.TrimSpace(match[1])
			remaining := strings.TrimSpace(text[len(match[0]):])
			if delegated != "" && remaining != "" {
				author = resolveDelegatedAuthorName(delegated, cfg.TeamMembers)
				reportText = remaining
			}
		}
	}

	status := "done"
	description := reportText

	if match := statusRegex.FindStringSubmatch(reportText); len(match) > 1 {
		status = strings.TrimSpace(match[1])
		description = strings.TrimSpace(reportText[:len(reportText)-len(match[0])])
	}

	item := WorkItem{
		Description: description,
		Author:      author,
		Source:      "slack",
		Status:      status,
		ReportedAt:  time.Now(),
	}

	if err := InsertWorkItem(db, item); err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error saving item: %v", err))
		log.Printf("report insert error user=%s: %v", cmd.UserID, err)
		return
	}

	monday, nextMonday := ReportWeekRange(cfg, time.Now())
	pending, err := GetPendingSlackItemsByAuthorAndDateRange(db, author, monday, nextMonday)
	if err != nil {
		log.Printf("report pending lookup error user=%s author=%s: %v", cmd.UserID, author, err)
		postEphemeral(api, cmd, fmt.Sprintf("Recorded: %s [%s]", description, status))
		return
	}

	msg := fmt.Sprintf("Recorded: %s (%s) for %s", description, status, author)
	if len(pending) > 0 {
		msg += "\n\nYour not-done items this week:"
		limit := 8
		for i, p := range pending {
			if i >= limit {
				msg += fmt.Sprintf("\n- ... and %d more", len(pending)-limit)
				break
			}
			msg += fmt.Sprintf("\n- %s (%s)", p.Description, normalizeStatus(p.Status))
		}
	}
	postEphemeral(api, cmd, msg)
	log.Printf("report saved user=%s author=%s status=%s", cmd.UserID, author, status)
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

	monday, nextMonday := ReportWeekRange(cfg, time.Now())
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
			ReportedAt:  mrReportedAt(mr),
		})
	}

	if len(newItems) == 0 {
		postEphemeral(api, cmd, fmt.Sprintf("Found %d merged MRs, all already tracked.", len(mrs)))
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

	monday, nextMonday := ReportWeekRange(cfg, time.Now())
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

	merged, llmUsage, err := BuildReportsFromLast(cfg, items, monday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error building report: %v", err))
		log.Printf("report build error: %v", err)
		return
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
	monday, nextMonday := ReportWeekRange(cfg, time.Now())
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

	monday, nextMonday := ReportWeekRange(cfg, time.Now())
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

	var missing []string
	for _, userID := range memberIDs {
		user, err := api.GetUserInfo(userID)
		if err != nil {
			missing = append(missing, fmt.Sprintf("<@%s> (lookup failed)", userID))
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
				display = userID
			}
			missing = append(missing, fmt.Sprintf("%s (<@%s>)", display, userID))
		}
	}

	for _, name := range unresolved {
		missing = append(missing, fmt.Sprintf("%s (not found)", name))
	}

	if len(missing) == 0 {
		postEphemeral(api, cmd, fmt.Sprintf("Everyone has reported this week (%s - %s).",
			monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2")))
		log.Printf("list-missing none")
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Missing reports for %s - %s* (%d total)\n\n",
		monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2"), len(missing)))
	for _, m := range missing {
		sb.WriteString(fmt.Sprintf("- %s\n", m))
	}

	postEphemeral(api, cmd, sb.String())
	log.Printf("list-missing count=%d", len(missing))
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
	if cb.View.CallbackID == modalDeleteCallbackID {
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
	monday, nextMonday := ReportWeekRange(cfg, time.Now())
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
	monday, nextMonday := ReportWeekRange(cfg, time.Now())
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
	monday, nextMonday := ReportWeekRange(cfg, time.Now())
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
		// Allow preserving uncommon statuses.
		slack.NewOptionBlockObject("other", slack.NewTextBlockObject(slack.PlainTextType, "other", false, false), nil),
	}
	statusSelect := slack.NewOptionsSelectBlockElement(
		slack.OptTypeStatic,
		slack.NewTextBlockObject(slack.PlainTextType, "Status", false, false),
		editActionStatus,
		statusOptions...,
	)
	cur := normalizeStatus(item.Status)
	if cur == "" {
		cur = "done"
	}
	found := false
	for _, o := range statusOptions {
		if o.Value == cur {
			statusSelect.InitialOption = o
			found = true
			break
		}
	}
	if !found {
		statusSelect.InitialOption = statusOptions[len(statusOptions)-1]
	}

	view := slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Edit item", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Save", false, false),
		CallbackID:      modalEditCallbackID,
		PrivateMetadata: fmt.Sprintf("%s%d|%s", modalMetaPrefix, itemID, channelID),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
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
		}},
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
	monday, nextMonday := ReportWeekRange(cfg, time.Now())
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

func handleNudge(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	isManager, err := isManagerUser(api, cfg, cmd.UserID)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error checking permissions: %v", err))
		log.Printf("nudge auth error user=%s: %v", cmd.UserID, err)
		return
	}
	if !isManager {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		log.Printf("nudge denied user=%s", cmd.UserID)
		return
	}

	if len(cfg.TeamMembers) == 0 {
		postEphemeral(api, cmd, "No team_members configured.")
		return
	}

	memberIDs, unresolved, err := resolveUserIDs(api, cfg.TeamMembers)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error resolving team members: %v", err))
		log.Printf("nudge resolve error: %v", err)
		return
	}
	if len(unresolved) > 0 {
		postEphemeral(api, cmd, fmt.Sprintf("Warning: could not resolve: %s", strings.Join(unresolved, ", ")))
		log.Printf("nudge unresolved: %s", strings.Join(unresolved, ", "))
	}

	text := strings.TrimSpace(cmd.Text)
	if text != "" {
		targetIDs, err := resolveNudgeTargets(api, text)
		if err != nil {
			postEphemeral(api, cmd, err.Error())
			log.Printf("nudge target resolve error: %v", err)
			return
		}
		for _, id := range targetIDs {
			if !containsString(memberIDs, id) {
				postEphemeral(api, cmd, "Error: mentioned user is not in team_members.")
				log.Printf("nudge target not in team_members id=%s", id)
				return
			}
		}
		sendNudges(api, cfg, targetIDs, cfg.ReportChannelID)
		postEphemeral(api, cmd, fmt.Sprintf("Sent nudges to %d team member(s).", len(targetIDs)))
		log.Printf("nudge sent target-count=%d", len(targetIDs))
		return
	}

	// No parameter: nudge only members who haven't reported this week.
	monday, nextMonday := ReportWeekRange(cfg, time.Now())
	authors, err := GetSlackAuthorsByDateRange(db, monday, nextMonday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading items: %v", err))
		log.Printf("nudge load error: %v", err)
		return
	}

	var reported []string
	for author := range authors {
		reported = append(reported, author)
	}

	var missingIDs []string
	for _, userID := range memberIDs {
		user, err := api.GetUserInfo(userID)
		if err != nil {
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
			missingIDs = append(missingIDs, userID)
		}
	}

	if len(missingIDs) == 0 {
		postEphemeral(api, cmd, fmt.Sprintf("Everyone has reported this week (%s - %s).",
			monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2")))
		log.Printf("nudge none missing")
		return
	}

	sendNudges(api, cfg, missingIDs, cfg.ReportChannelID)
	postEphemeral(api, cmd, fmt.Sprintf("Sent nudges to %d team member(s).", len(missingIDs)))
	log.Printf("nudge sent missing-count=%d", len(missingIDs))
}

func handleHelp(api *slack.Client, cfg Config, cmd slack.SlashCommand) {
	isManager, err := isManagerUser(api, cfg, cmd.UserID)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error checking permissions: %v", err))
		log.Printf("help auth error user=%s: %v", cmd.UserID, err)
		return
	}

	lines := []string{
		"ReportBot Commands",
		"",
		"/report <description> (status) - Report a work item.",
		"/rpt <description> (status) - Alias of /report.",
		"  Example: /report [mantis_id] Add pagination to user list API (done)",
		"",
		"/list - List this week's items.",
		"/help - Show this help.",
	}

	if isManager {
		lines = append(lines,
			"",
			"/fetch-mrs - Fetch merged GitLab MRs for this week.",
			"/generate-report team|boss - Generate weekly report.",
			"/gen team|boss - Alias of /generate-report.",
			"/list-missing - List team members who haven't reported this week.",
			"/nudge [@name] - Nudge missing members, or a specific user.",
		)
	}

	postEphemeral(api, cmd, strings.Join(lines, "\n"))
}

func isManagerUser(api *slack.Client, cfg Config, userID string) (bool, error) {
	user, err := api.GetUserInfo(userID)
	if err != nil {
		return false, err
	}

	candidates := []string{
		user.RealName,
		user.Profile.DisplayName,
		user.Name,
	}
	for _, c := range candidates {
		if c != "" && cfg.IsManagerName(c) {
			return true, nil
		}
	}
	return false, nil
}

func resolveNudgeTargets(api *slack.Client, text string) ([]string, error) {
	mentionID := extractMentionID(text)
	if mentionID != "" {
		return []string{mentionID}, nil
	}

	name := strings.TrimSpace(strings.TrimPrefix(text, "@"))
	if name == "" {
		return nil, fmt.Errorf("Error: invalid nudge target.")
	}
	ids, unresolved, err := resolveUserIDs(api, []string{name})
	if err != nil {
		return nil, fmt.Errorf("Error resolving user: %v", err)
	}
	if len(ids) == 0 || len(unresolved) > 0 {
		return nil, fmt.Errorf("Error: mentioned user was not found.")
	}
	return ids, nil
}

func extractMentionID(text string) string {
	re := regexp.MustCompile(`<@([A-Z0-9]+)(?:\\|[^>]+)?>`)
	if match := re.FindStringSubmatch(text); len(match) > 1 {
		return match[1]
	}
	return ""
}

func containsString(vals []string, target string) bool {
	for _, v := range vals {
		if v == target {
			return true
		}
	}
	return false
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

func mrReportedAt(mr GitLabMR) time.Time {
	if mr.State == "opened" && !mr.UpdatedAt.IsZero() {
		return mr.UpdatedAt
	}
	if !mr.MergedAt.IsZero() {
		return mr.MergedAt
	}
	if !mr.CreatedAt.IsZero() {
		return mr.CreatedAt
	}
	return time.Now()
}
