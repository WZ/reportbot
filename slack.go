package main

import (
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

var statusRegex = regexp.MustCompile(`\(([^)]+)\)\s*$`)

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
				client.Ack(*evt.Request)
				go handleSlashCommand(client, api, db, cfg, cmd)
			}
		}
	}()

	log.Println("Slack bot connected via Socket Mode")
	return client.Run()
}

func handleSlashCommand(client *socketmode.Client, api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	switch cmd.Command {
	case "/report":
		handleReport(api, db, cmd)
	case "/fetch-mrs":
		handleFetchMRs(api, db, cfg, cmd)
	case "/generate-report":
		handleGenerateReport(api, db, cfg, cmd)
	case "/list-items":
		handleListItems(api, db, cmd)
	}
}

func handleReport(api *slack.Client, db *sql.DB, cmd slack.SlashCommand) {
	text := strings.TrimSpace(cmd.Text)
	if text == "" {
		postEphemeral(api, cmd, "Usage: /report <description> (status)\nExample: /report Fix consul timeout (done)")
		return
	}

	status := "done"
	description := text

	if match := statusRegex.FindStringSubmatch(text); len(match) > 1 {
		status = strings.TrimSpace(match[1])
		description = strings.TrimSpace(text[:len(text)-len(match[0])])
	}

	item := WorkItem{
		Description: description,
		Author:      cmd.UserName,
		Source:      "slack",
		Status:      status,
		ReportedAt:  time.Now(),
	}

	if err := InsertWorkItem(db, item); err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error saving item: %v", err))
		return
	}

	postEphemeral(api, cmd, fmt.Sprintf("Recorded: %s [%s]", description, status))
}

func handleFetchMRs(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	if !cfg.IsManager(cmd.UserID) {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		return
	}

	monday, nextMonday := CurrentWeekRange()

	postEphemeral(api, cmd, fmt.Sprintf("Fetching merged MRs for %s to %s...",
		monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2")))

	mrs, err := FetchMergedMRs(cfg, monday, nextMonday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error fetching MRs: %v", err))
		return
	}

	var newItems []WorkItem
	for _, mr := range mrs {
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
			Status:      "done",
			ReportedAt:  mr.MergedAt,
		})
	}

	if len(newItems) == 0 {
		postEphemeral(api, cmd, fmt.Sprintf("Found %d merged MRs, all already tracked.", len(mrs)))
		return
	}

	inserted, err := InsertWorkItems(db, newItems)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error storing MRs: %v", err))
		return
	}

	postEphemeral(api, cmd, fmt.Sprintf("Fetched %d merged MRs (%d new, %d already tracked)",
		len(mrs), inserted, len(mrs)-inserted))
}

func handleGenerateReport(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	if !cfg.IsManager(cmd.UserID) {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		return
	}

	mode := strings.TrimSpace(cmd.Text)
	if mode != "team" && mode != "boss" {
		mode = "team"
	}

	postEphemeral(api, cmd, fmt.Sprintf("Generating report (mode: %s)...", mode))

	monday, nextMonday := CurrentWeekRange()
	items, err := GetItemsByDateRange(db, monday, nextMonday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading items: %v", err))
		return
	}

	if len(items) == 0 {
		postEphemeral(api, cmd, "No work items found for this week.")
		return
	}

	// Categorize uncategorized items
	var uncategorized []WorkItem
	for _, item := range items {
		if item.Category == "" {
			uncategorized = append(uncategorized, item)
		}
	}

	if len(uncategorized) > 0 {
		categoryMap, ticketMap, err := CategorizeItems(cfg, uncategorized)
		if err != nil {
			postEphemeral(api, cmd, fmt.Sprintf("Warning: AI categorization failed: %v. Generating with 'Uncategorized' items.", err))
		} else {
			if err := UpdateCategories(db, categoryMap); err != nil {
				log.Printf("Error updating categories: %v", err)
			}
			if err := UpdateTicketIDs(db, ticketMap); err != nil {
				log.Printf("Error updating ticket IDs: %v", err)
			}

			// Reload items with updated categories
			items, err = GetItemsByDateRange(db, monday, nextMonday)
			if err != nil {
				postEphemeral(api, cmd, fmt.Sprintf("Error reloading items: %v", err))
				return
			}
		}
	}

	report := GenerateReport(items, monday, mode, cfg.Categories, cfg.TeamName)

	filePath, err := WriteReportFile(report, cfg.ReportOutputDir, monday, cfg.TeamName)
	if err != nil {
		log.Printf("Error writing report file: %v", err)
	}

	// Post report to channel
	_, _, err = api.PostMessage(cmd.ChannelID, slack.MsgOptionText("```\n"+report+"\n```", false))
	if err != nil {
		log.Printf("Error posting report: %v", err)
		postEphemeral(api, cmd, "Error posting report to channel. Check bot permissions.")
		return
	}

	msg := fmt.Sprintf("Report generated with %d items (mode: %s)", len(items), mode)
	if filePath != "" {
		msg += fmt.Sprintf("\nSaved to: %s", filePath)
	}
	postEphemeral(api, cmd, msg)
}

func handleListItems(api *slack.Client, db *sql.DB, cmd slack.SlashCommand) {
	monday, nextMonday := CurrentWeekRange()
	items, err := GetItemsByDateRange(db, monday, nextMonday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error: %v", err))
		return
	}

	if len(items) == 0 {
		postEphemeral(api, cmd, fmt.Sprintf("No items for this week (%s - %s)",
			monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2")))
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Items for %s - %s* (%d total)\n\n",
		monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2"), len(items)))

	for _, item := range items {
		source := ""
		if item.Source == "gitlab" {
			source = " [GitLab]"
		}
		category := ""
		if item.Category != "" {
			category = fmt.Sprintf(" _%s_", item.Category)
		}
		sb.WriteString(fmt.Sprintf("- *%s*: %s (%s)%s%s\n",
			item.Author, item.Description, item.Status, source, category))
	}

	postEphemeral(api, cmd, sb.String())
}

func postEphemeral(api *slack.Client, cmd slack.SlashCommand, text string) {
	_, err := api.PostEphemeral(cmd.ChannelID, cmd.UserID, slack.MsgOptionText(text, false))
	if err != nil {
		log.Printf("Error posting ephemeral: %v", err)
	}
}
