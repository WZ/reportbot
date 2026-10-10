package fetch

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/storage/sqlite"

	"github.com/robfig/cron/v3"
	"github.com/slack-go/slack"
)

// FetchResult tracks separate counters for each outcome.
type FetchResult struct {
	TotalFetched   int
	Inserted       int // became their own Work Items
	Linked         int // linked to an Open Work Item by exact ticket
	MovedToTesting int // a merge moved a Work Item from in progress to in testing
	AlreadyTracked int
	SkippedNonTeam int
	SkippedClosed  int // closed without merging
	Errors         []string
}

// Hooks connect fetching to the rest of the bot without importing it.
type Hooks struct {
	ResolveAuthorID func(name string) string // Slack user ID for a Code Change author, "" if unknown
	Wake            func()                   // start filing new Work Items now
}

// FetchAndImportMRs fetches GitLab MRs and/or GitHub PRs for the current report
// week and records them as Code Changes: linked to an Open Work Item by exact
// ticket, or as their own Work Item queued for filing.
func FetchAndImportMRs(cfg Config, db *sql.DB, hooks Hooks) (FetchResult, error) {
	if !cfg.GitLabConfigured() && !cfg.GitHubConfigured() {
		return FetchResult{}, fmt.Errorf("neither GitLab nor GitHub is configured")
	}

	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	log.Printf("auto-fetch range %s - %s", monday.Format("2006-01-02"), nextMonday.Format("2006-01-02"))

	var result FetchResult
	var changes []fetched

	if cfg.GitLabConfigured() {
		mrs, err := FetchMRs(cfg, monday, nextMonday)
		if err != nil {
			log.Printf("auto-fetch gitlab error: %v", err)
			result.Errors = append(result.Errors, fmt.Sprintf("GitLab: %v", err))
		} else {
			log.Printf("auto-fetch gitlab fetched=%d", len(mrs))
			result.TotalFetched += len(mrs)
			for _, mr := range mrs {
				if len(cfg.TeamMembers) > 0 && !anyNameMatches(cfg.TeamMembers, mr.AuthorName) && !anyNameMatches(cfg.TeamMembers, mr.Author) {
					log.Printf("auto-fetch skipped non-team gitlab author=%s username=%s", mr.AuthorName, mr.Author)
					result.SkippedNonTeam++
					continue
				}
				changes = append(changes, fetched{cc: domain.CodeChange{Source: "gitlab", SourceRef: mr.WebURL, Title: mr.Title,
					Author: mr.AuthorName, TicketIDs: mr.TicketIDs, State: codeChangeState(mr.State), MergedAt: mr.MergedAt},
					reportedAt: mrReportedAt(mr, cfg.Location)})
			}
		}
	}

	if cfg.GitHubConfigured() {
		prs, err := FetchGitHubPRs(cfg, monday, nextMonday)
		if err != nil {
			log.Printf("auto-fetch github error: %v", err)
			result.Errors = append(result.Errors, fmt.Sprintf("GitHub: %v", err))
		} else {
			log.Printf("auto-fetch github fetched=%d", len(prs))
			result.TotalFetched += len(prs)
			for _, pr := range prs {
				if len(cfg.TeamMembers) > 0 && !anyNameMatches(cfg.TeamMembers, pr.AuthorName) && !anyNameMatches(cfg.TeamMembers, pr.Author) {
					log.Printf("auto-fetch skipped non-team github author=%s", pr.Author)
					result.SkippedNonTeam++
					continue
				}
				changes = append(changes, fetched{cc: domain.CodeChange{Source: "github", SourceRef: pr.HTMLURL, Title: pr.Title,
					Author: pr.AuthorName, State: codeChangeState(pr.State), MergedAt: pr.MergedAt},
					reportedAt: prReportedAt(pr, cfg.Location)})
			}
		}
	}

	if len(result.Errors) > 0 && len(changes) == 0 && result.TotalFetched == 0 {
		return result, fmt.Errorf("all fetches failed: %s", strings.Join(result.Errors, "; "))
	}
	if err := importCodeChanges(db, changes, hooks, &result); err != nil {
		return result, fmt.Errorf("error storing MRs/PRs: %v", err)
	}
	if result.Inserted > 0 && hooks.Wake != nil {
		hooks.Wake()
	}
	return result, nil
}

type fetched struct {
	cc         domain.CodeChange
	reportedAt time.Time
}

func codeChangeState(state string) string {
	switch state {
	case "merged":
		return "merged"
	case "closed":
		return "closed"
	default:
		return "open"
	}
}

// statusForCodeChange: done is always a person's decision, so a merge means in testing.
func statusForCodeChange(state string) string {
	if state == "merged" {
		return domain.StatusInTesting
	}
	return domain.StatusInProgress
}

func importCodeChanges(db *sql.DB, changes []fetched, hooks Hooks, result *FetchResult) error {
	for _, f := range changes {
		cc := f.cc
		tracked, ok, err := sqlite.GetCodeChangeByRef(db, cc.Source, cc.SourceRef)
		if err != nil {
			return err
		}
		if ok {
			result.AlreadyTracked++
			if tracked.State == cc.State {
				continue
			}
			if err := sqlite.UpdateCodeChangeState(db, tracked.ID, cc.State, cc.MergedAt); err != nil {
				return err
			}
			if cc.State == "merged" && tracked.WorkItemID != 0 {
				if err := moveToTesting(db, tracked.WorkItemID, result); err != nil {
					return err
				}
			}
			continue
		}
		if cc.State == "closed" {
			result.SkippedClosed++
			continue
		}
		if hooks.ResolveAuthorID != nil {
			cc.AuthorID = hooks.ResolveAuthorID(cc.Author)
		}
		if target := exactTicketMatch(db, cc.TicketIDs); target != 0 {
			cc.WorkItemID = target
			if _, err := sqlite.LinkNewCodeChange(db, cc); err != nil {
				return err
			}
			result.Linked++
			if cc.State == "merged" {
				if err := moveToTesting(db, target, result); err != nil {
					return err
				}
			}
			continue
		}
		item := WorkItem{Description: cc.Title, Author: cc.Author, AuthorID: cc.AuthorID, Source: cc.Source,
			Status: statusForCodeChange(cc.State), TicketIDs: cc.TicketIDs, ReportedAt: f.reportedAt}
		if _, _, err := sqlite.InsertCodeChangeWithItem(db, cc, item, `{"kind":"code_change"}`); err != nil {
			return err
		}
		result.Inserted++
	}
	return nil
}

// exactTicketMatch returns the newest Open Work Item sharing a ticket ID, or 0.
func exactTicketMatch(db *sql.DB, tickets string) int64 {
	for _, t := range strings.Split(tickets, ",") {
		if t = strings.TrimSpace(t); !sqlite.ValidTicketID(t) {
			continue
		}
		items, err := sqlite.FindOpenItemsByTicket(db, t)
		if err == nil && len(items) > 0 {
			return items[0].ID
		}
	}
	return 0
}

func moveToTesting(db *sql.DB, itemID int64, result *FetchResult) error {
	item, err := sqlite.GetWorkItemByID(db, itemID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if item.Status != domain.StatusInProgress {
		return nil
	}
	result.MovedToTesting++
	return sqlite.SetWorkItemStatus(db, itemID, domain.StatusInTesting)
}

// FormatFetchSummary returns a human-readable summary of a FetchResult.
func FormatFetchSummary(result FetchResult) string {
	if len(result.Errors) > 0 && result.TotalFetched == 0 {
		return fmt.Sprintf("Error fetching MRs/PRs:\n%s", strings.Join(result.Errors, "\n"))
	}

	if result.Inserted == 0 && result.Linked == 0 && result.MovedToTesting == 0 {
		var reasons []string
		if result.AlreadyTracked > 0 {
			reasons = append(reasons, fmt.Sprintf("%d already tracked", result.AlreadyTracked))
		}
		if result.SkippedNonTeam > 0 {
			reasons = append(reasons, fmt.Sprintf("%d non-team", result.SkippedNonTeam))
		}
		if result.SkippedClosed > 0 {
			reasons = append(reasons, fmt.Sprintf("%d closed unmerged", result.SkippedClosed))
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
	if result.Linked > 0 {
		summary = append(summary, fmt.Sprintf("%d linked to open items", result.Linked))
	}
	if result.MovedToTesting > 0 {
		summary = append(summary, fmt.Sprintf("%d moved to in testing", result.MovedToTesting))
	}
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
func StartAutoFetchScheduler(cfg Config, db *sql.DB, api *slack.Client, hooks Hooks) {
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

			result, fetchErr := FetchAndImportMRs(cfg, db, hooks)
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
