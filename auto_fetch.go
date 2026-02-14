package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/slack-go/slack"
)

// FetchResult tracks separate counters for each skip reason.
type FetchResult struct {
	TotalFetched   int
	Inserted       int
	AlreadyTracked int
	SkippedNonTeam int
	Errors         []string
}

// FetchAndImportMRs fetches GitLab MRs and/or GitHub PRs for the current
// report week and inserts new items into the database. It has no Slack
// dependency so it can be called from both the slash command and the scheduler.
func FetchAndImportMRs(cfg Config, db *sql.DB) (FetchResult, error) {
	if !cfg.GitLabConfigured() && !cfg.GitHubConfigured() {
		return FetchResult{}, fmt.Errorf("neither GitLab nor GitHub is configured")
	}

	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	log.Printf("auto-fetch range %s - %s", monday.Format("2006-01-02"), nextMonday.Format("2006-01-02"))

	var result FetchResult
	var newItems []WorkItem

	// Fetch GitLab MRs if configured.
	if cfg.GitLabConfigured() {
		mrs, err := FetchMRs(cfg, monday, nextMonday)
		if err != nil {
			log.Printf("auto-fetch gitlab error: %v", err)
			result.Errors = append(result.Errors, fmt.Sprintf("GitLab: %v", err))
		} else {
			log.Printf("auto-fetch gitlab fetched=%d", len(mrs))
			result.TotalFetched += len(mrs)
			for _, mr := range mrs {
				if len(cfg.TeamMembers) > 0 {
					if !anyNameMatches(cfg.TeamMembers, mr.AuthorName) && !anyNameMatches(cfg.TeamMembers, mr.Author) {
						log.Printf("auto-fetch skipped non-team gitlab author=%s username=%s", mr.AuthorName, mr.Author)
						result.SkippedNonTeam++
						continue
					}
				}
				exists, dbErr := SourceRefExists(db, mr.WebURL)
				if dbErr != nil {
					log.Printf("Error checking MR existence: %v", dbErr)
					continue
				}
				if exists {
					result.AlreadyTracked++
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
		}
	}

	// Fetch GitHub PRs if configured.
	if cfg.GitHubConfigured() {
		prs, err := FetchGitHubPRs(cfg, monday, nextMonday)
		if err != nil {
			log.Printf("auto-fetch github error: %v", err)
			result.Errors = append(result.Errors, fmt.Sprintf("GitHub: %v", err))
		} else {
			log.Printf("auto-fetch github fetched=%d", len(prs))
			result.TotalFetched += len(prs)
			for _, pr := range prs {
				if len(cfg.TeamMembers) > 0 {
					if !anyNameMatches(cfg.TeamMembers, pr.AuthorName) && !anyNameMatches(cfg.TeamMembers, pr.Author) {
						log.Printf("auto-fetch skipped non-team github author=%s", pr.Author)
						result.SkippedNonTeam++
						continue
					}
				}
				exists, dbErr := SourceRefExists(db, pr.HTMLURL)
				if dbErr != nil {
					log.Printf("Error checking PR existence: %v", dbErr)
					continue
				}
				if exists {
					result.AlreadyTracked++
					continue
				}
				newItems = append(newItems, WorkItem{
					Description: pr.Title,
					Author:      pr.AuthorName,
					Source:      "github",
					SourceRef:   pr.HTMLURL,
					Status:      mapPRStatus(pr),
					ReportedAt:  prReportedAt(pr, cfg.Location),
				})
			}
		}
	}

	if len(result.Errors) > 0 && len(newItems) == 0 && result.TotalFetched == 0 {
		return result, fmt.Errorf("all fetches failed: %s", strings.Join(result.Errors, "; "))
	}

	if len(newItems) > 0 {
		inserted, err := InsertWorkItems(db, newItems)
		if err != nil {
			return result, fmt.Errorf("error storing MRs/PRs: %v", err)
		}
		result.Inserted = inserted
	}

	return result, nil
}

// FormatFetchSummary returns a human-readable summary of a FetchResult.
func FormatFetchSummary(result FetchResult) string {
	if len(result.Errors) > 0 && result.TotalFetched == 0 {
		return fmt.Sprintf("Error fetching MRs/PRs:\n%s", strings.Join(result.Errors, "\n"))
	}

	if result.Inserted == 0 {
		var reasons []string
		if result.AlreadyTracked > 0 {
			reasons = append(reasons, fmt.Sprintf("%d already tracked", result.AlreadyTracked))
		}
		if result.SkippedNonTeam > 0 {
			reasons = append(reasons, fmt.Sprintf("%d non-team", result.SkippedNonTeam))
		}
		msg := fmt.Sprintf("Found %d MRs/PRs (merged+open), none to add", result.TotalFetched)
		if len(reasons) > 0 {
			msg += fmt.Sprintf(" (%s)", strings.Join(reasons, ", "))
		}
		msg += "."
		if len(result.Errors) > 0 {
			msg += fmt.Sprintf("\nWarnings:\n%s", strings.Join(result.Errors, "\n"))
		}
		return msg
	}

	var summary []string
	summary = append(summary, fmt.Sprintf("%d new", result.Inserted))
	if result.AlreadyTracked > 0 {
		summary = append(summary, fmt.Sprintf("%d already tracked", result.AlreadyTracked))
	}
	if result.SkippedNonTeam > 0 {
		summary = append(summary, fmt.Sprintf("%d non-team", result.SkippedNonTeam))
	}
	msg := fmt.Sprintf("Fetched %d MRs/PRs (merged+open): %s",
		result.TotalFetched, strings.Join(summary, ", "))
	if len(result.Errors) > 0 {
		msg += fmt.Sprintf("\nWarnings:\n%s", strings.Join(result.Errors, "\n"))
	}
	return msg
}

// StartAutoFetchScheduler starts a cron-based scheduler that periodically
// fetches MRs/PRs and posts a summary to the report channel.
// The schedule is a standard 5-field cron expression (minute hour day-of-month month day-of-week).
// Examples: "0 9 * * *" (daily 9am), "0 9 * * 1-5" (weekdays 9am), "0 9 * * 5" (Fridays 9am).
func StartAutoFetchScheduler(cfg Config, db *sql.DB, api *slack.Client) {
	schedule := strings.TrimSpace(cfg.AutoFetchSchedule)
	if schedule == "" {
		log.Println("Auto-fetch disabled (auto_fetch_schedule not set)")
		return
	}
	if !cfg.GitLabConfigured() && !cfg.GitHubConfigured() {
		log.Println("Auto-fetch disabled: neither GitLab nor GitHub is configured")
		return
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(schedule)
	if err != nil {
		log.Printf("Invalid auto_fetch_schedule '%s': %v — auto-fetch disabled", schedule, err)
		return
	}

	var sources []string
	if cfg.GitLabConfigured() {
		sources = append(sources, "GitLab")
	}
	if cfg.GitHubConfigured() {
		sources = append(sources, "GitHub")
	}
	log.Printf("Auto-fetch scheduled (cron: %s) from %s", schedule, strings.Join(sources, " + "))

	go func() {
		for {
			now := time.Now().In(cfg.Location)
			next := sched.Next(now)
			wait := next.Sub(now)
			log.Printf("Next auto-fetch at %s (in %s)", next.Format("Mon Jan 2 15:04"), wait.Round(time.Minute))

			time.Sleep(wait)

			result, fetchErr := FetchAndImportMRs(cfg, db)
			summary := FormatFetchSummary(result)
			if fetchErr != nil {
				log.Printf("Auto-fetch error: %v", fetchErr)
			}
			log.Printf("Auto-fetch complete: %s", summary)

			if cfg.ReportChannelID != "" {
				_, _, postErr := api.PostMessage(cfg.ReportChannelID, slack.MsgOptionText(
					fmt.Sprintf("Auto-fetch complete: %s", summary), false))
				if postErr != nil {
					log.Printf("Auto-fetch post error: %v", postErr)
				}
			}
		}
	}()
}
