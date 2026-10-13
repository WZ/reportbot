package fetch

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/filing"
	gl "reportbot/internal/integrations/gitlab"
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
	// Sources breaks the counters down per source, in fetch order.
	Sources []SourceResult
}

// SourceResult is one source's share of a fetch. New counts items that became
// their own Work Items; Linked counts ones linked to an Open Work Item.
type SourceResult struct {
	Label   string
	Fetched int
	New     int
	Linked  int
}

// sourceResult returns the per-source entry for label, appending it if absent.
func (r *FetchResult) sourceResult(label string) *SourceResult {
	for i := range r.Sources {
		if r.Sources[i].Label == label {
			return &r.Sources[i]
		}
	}
	r.Sources = append(r.Sources, SourceResult{Label: label})
	return &r.Sources[len(r.Sources)-1]
}

// Hooks connect fetching to the rest of the bot without importing it.
type Hooks struct {
	// ResolveAccount returns the Slack user ID configured for a code-hosting account by numeric
	// ID or username, "" if none, and a warning when those disagree. Display names never bind.
	// server is the gitlab_sources name for GitLab ("gitlab@<server>:..." keys), "" for GitHub.
	ResolveAccount func(host, server string, accountID int64, username string) (slackID, warn string)
	// TeamNames returns the Slack profile names of the given Slack user IDs, for the team filter only.
	TeamNames func(slackIDs []string) ([]string, error)
	Wake      func() // start filing new Work Items now
}

// FetchAndImportMRs fetches GitLab MRs, GitLab branch commits and/or GitHub PRs
// for the current report week and records them as Code Changes: linked to an
// Open Work Item by exact ticket, or as their own Work Item queued for filing.
func FetchAndImportMRs(cfg Config, db *sql.DB, hooks Hooks) (FetchResult, error) {
	if !cfg.GitLabConfigured() && !cfg.GitHubConfigured() {
		return FetchResult{}, fmt.Errorf("neither GitLab nor GitHub is configured")
	}

	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	log.Printf("auto-fetch range %s - %s", monday.Format("2006-01-02"), nextMonday.Format("2006-01-02"))

	var result FetchResult
	var changes []fetched
	team := newTeamFilter(cfg, hooks)

	// GitLab sources fetch sequentially in config order; one failing source
	// (bad token, instance down) is reported and the rest still fetch.
	for _, src := range cfg.GitLabSources {
		switch {
		case strings.TrimSpace(src.GroupID) != "":
			mrs, err := FetchMRs(src, monday, nextMonday)
			if err != nil {
				log.Printf("auto-fetch gitlab@%s error: %v", src.Name, err)
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", src.Label(), err))
				continue
			}
			log.Printf("auto-fetch gitlab@%s fetched=%d", src.Name, len(mrs))
			result.TotalFetched += len(mrs)
			result.sourceResult(src.Label()).Fetched += len(mrs)
			for _, mr := range mrs {
				// Import filter only: it decides what is imported, never who owns it.
				if !team.includes("gitlab", src.Name, mr.AuthorID, mr.AuthorName, mr.Author) {
					log.Printf("auto-fetch skipped non-team gitlab@%s author=%s username=%s", src.Name, mr.AuthorName, mr.Author)
					result.SkippedNonTeam++
					continue
				}
				changes = append(changes, fetched{cc: domain.CodeChange{Source: "gitlab", SourceRef: mr.WebURL, Title: mr.Title,
					Author: mr.AuthorName, TicketIDs: mr.TicketIDs, State: codeChangeState(mr.State), MergedAt: mr.MergedAt},
					host: "gitlab", server: src.Name, label: src.Label(), accountID: mr.AuthorID, username: mr.Author, reportedAt: mrReportedAt(mr, cfg.Location)})
			}
		default:
			commits, err := FetchBranchCommits(src, monday, nextMonday)
			if err != nil {
				log.Printf("auto-fetch gitlab@%s error: %v", src.Name, err)
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", src.Label(), err))
				continue
			}
			log.Printf("auto-fetch gitlab@%s fetched=%d commits", src.Name, len(commits))
			result.TotalFetched += len(commits)
			result.sourceResult(src.Label()).Fetched += len(commits)
			for _, cm := range commits {
				// Import filter only: it decides what is imported, never who owns it.
				if !team.includes("gitlab", src.Name, 0, cm.AuthorName, "") {
					log.Printf("auto-fetch skipped non-team gitlab@%s commit author=%s", src.Name, cm.AuthorName)
					result.SkippedNonTeam++
					continue
				}
				// The commits API returns no username or account ID, so commits bind
				// their owner by plain author_ids name key, and land as done work.
				changes = append(changes, fetched{cc: domain.CodeChange{Source: "gitlab", SourceRef: cm.WebURL, Title: cm.Title,
					Author: cm.AuthorName, AuthorID: cfg.AuthorID(cm.AuthorName), TicketIDs: commitTicketIDs(cm, src), State: "merged", MergedAt: cm.CommittedAt},
					host: "gitlab", server: src.Name, label: src.Label(), status: domain.StatusDone, reportedAt: cm.CommittedAt.In(cfg.Location)})
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
			result.sourceResult("GitHub").Fetched += len(prs)
			for _, pr := range prs {
				// Import filter only: it decides what is imported, never who owns it.
				if !team.includes("github", "", pr.AuthorID, pr.AuthorName, pr.Author) {
					log.Printf("auto-fetch skipped non-team github author=%s", pr.Author)
					result.SkippedNonTeam++
					continue
				}
				changes = append(changes, fetched{cc: domain.CodeChange{Source: "github", SourceRef: pr.HTMLURL, Title: pr.Title,
					Author: pr.AuthorName, TicketIDs: githubTicketIDs(pr, cfg.GitHubRefTicketLabel), State: codeChangeState(pr.State), MergedAt: pr.MergedAt},
					host: "github", label: "GitHub", accountID: pr.AuthorID, username: pr.Author, reportedAt: prReportedAt(pr, cfg.Location)})
			}
		}
	}

	team.warnSkipped(result.SkippedNonTeam)

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

// githubTicketIDs reads tickets from the PR description's ticket field, as for GitLab,
// and otherwise from the title; the search API returns no branch name.
func githubTicketIDs(pr GitHubPR, fieldLabel string) string {
	if t := gl.ParseTicketIDs(pr.Body, fieldLabel); t != "" {
		return t
	}
	certain, _ := filing.FindTickets(pr.Title)
	return strings.Join(certain, ",")
}

// commitTicketIDs parses "<label>[- ]?<digits>" from a commit's title or message;
// the label comes from the source's ref_ticket_label.
func commitTicketIDs(cm GitLabCommit, src GitLabSource) string {
	return gl.ParseCommitTicketIDs(cm.Title+"\n"+cm.Message, strings.TrimSpace(src.RefTicketLabel))
}

type fetched struct {
	cc         domain.CodeChange
	host       string // "gitlab" | "github"
	server     string // gitlab_sources name for GitLab, "" for GitHub
	label      string // source label for the per-source summary
	accountID  int64
	username   string // GitLab username or GitHub login
	status     string // WorkItem status override; empty derives from the Code Change state
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
		src := result.sourceResult(f.label)
		if hooks.ResolveAccount != nil && f.status == "" {
			var warn string
			cc.AuthorID, warn = hooks.ResolveAccount(f.host, f.server, f.accountID, f.username)
			if warn != "" {
				log.Printf("auto-fetch WARNING: %s", warn)
			}
		}
		if target := exactTicketMatch(db, cc.TicketIDs); target != 0 {
			cc.WorkItemID = target
			if _, err := sqlite.LinkNewCodeChange(db, cc); err != nil {
				return err
			}
			result.Linked++
			src.Linked++
			if cc.State == "merged" {
				if err := moveToTesting(db, target, result); err != nil {
					return err
				}
			}
			continue
		}
		status := f.status
		if status == "" {
			status = statusForCodeChange(cc.State)
		}
		item := WorkItem{Description: cc.Title, Author: cc.Author, AuthorID: cc.AuthorID, Source: cc.Source,
			Status: status, TicketIDs: cc.TicketIDs, ReportedAt: f.reportedAt}
		if _, _, err := sqlite.InsertCodeChangeWithItem(db, cc, item, filing.Payload{Kind: filing.KindCodeChange}.JSON()); err != nil {
			return err
		}
		result.Inserted++
		src.New++
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
	return sqlite.SetWorkItemStatus(db, itemID, domain.StatusInTesting, sqlite.Actor{Via: sqlite.ViaFetch})
}

// FormatFetchSummary returns a human-readable summary of a FetchResult.
func FormatFetchSummary(result FetchResult) string {
	breakdown := sourceBreakdown(result)
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
		msg := fmt.Sprintf("Found %d MRs/PRs/commits, none to add", result.TotalFetched)
		if len(reasons) > 0 {
			msg += fmt.Sprintf(" (%s)", strings.Join(reasons, ", "))
		}
		msg += "."
		if breakdown != "" {
			msg += "\n" + breakdown
		}
		if len(result.Errors) > 0 {
			msg += fmt.Sprintf("\nWarnings:\n%s", strings.Join(result.Errors, "\n"))
		}
		return msg
	}

	var summary []string
	summary = append(summary, fmt.Sprintf("%d new", result.Inserted))
	if result.Linked > 0 {
		summary = append(summary, fmt.Sprintf("%d linked to open Work Items", result.Linked))
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
	msg := fmt.Sprintf("Fetched %d MRs/PRs/commits: %s",
		result.TotalFetched, strings.Join(summary, ", "))
	if breakdown != "" {
		msg += "\n" + breakdown
	}
	if len(result.Errors) > 0 {
		msg += fmt.Sprintf("\nWarnings:\n%s", strings.Join(result.Errors, "\n"))
	}
	return msg
}

// sourceBreakdown renders one line per fetched source; New and Linked reconcile
// with the headline counters.
func sourceBreakdown(result FetchResult) string {
	if len(result.Sources) < 2 {
		return ""
	}
	var lines []string
	for _, s := range result.Sources {
		lines = append(lines, fmt.Sprintf("  %s: %d fetched, %d new, %d linked", s.Label, s.Fetched, s.New, s.Linked))
	}
	return strings.Join(lines, "\n")
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
	for _, src := range cfg.GitLabSources {
		sources = append(sources, "gitlab@"+src.Label())
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
