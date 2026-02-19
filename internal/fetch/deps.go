package fetch

import (
	"database/sql"
	"regexp"
	"reportbot/internal/config"
	"reportbot/internal/domain"
	gh "reportbot/internal/integrations/github"
	gl "reportbot/internal/integrations/gitlab"
	"reportbot/internal/storage/sqlite"
	"strings"
	"time"
)

type Config = config.Config
type WorkItem = domain.WorkItem
type GitLabMR = domain.GitLabMR
type GitHubPR = domain.GitHubPR

func ReportWeekRange(cfg Config, now time.Time) (time.Time, time.Time) {
	return domain.ReportWeekRange(cfg, now)
}

func FetchMRs(cfg Config, from, to time.Time) ([]GitLabMR, error) {
	return gl.FetchMRs(cfg, from, to)
}

func FetchGitHubPRs(cfg Config, from, to time.Time) ([]GitHubPR, error) {
	return gh.FetchGitHubPRs(cfg, from, to)
}

func SourceRefExists(db *sql.DB, sourceRef string) (bool, error) {
	return sqlite.SourceRefExists(db, sourceRef)
}

func InsertWorkItems(db *sql.DB, items []WorkItem) (int, error) {
	return sqlite.InsertWorkItems(db, items)
}

func mapMRStatus(mr GitLabMR) string {
	if mr.State == "merged" {
		return "done"
	}
	if mr.State == "opened" {
		return "in progress"
	}
	return "in progress"
}

func mrReportedAt(mr GitLabMR, loc *time.Location) time.Time {
	if mr.State == "opened" && !mr.UpdatedAt.IsZero() {
		return mr.UpdatedAt.In(loc)
	}
	if !mr.MergedAt.IsZero() {
		return mr.MergedAt.In(loc)
	}
	if !mr.CreatedAt.IsZero() {
		return mr.CreatedAt.In(loc)
	}
	return time.Now().In(loc)
}

func mapPRStatus(pr GitHubPR) string {
	if pr.State == "merged" {
		return "done"
	}
	if pr.State == "open" {
		return "in progress"
	}
	return "in progress"
}

func prReportedAt(pr GitHubPR, loc *time.Location) time.Time {
	if pr.State == "open" && !pr.UpdatedAt.IsZero() {
		return pr.UpdatedAt.In(loc)
	}
	if !pr.MergedAt.IsZero() {
		return pr.MergedAt.In(loc)
	}
	if !pr.ClosedAt.IsZero() {
		return pr.ClosedAt.In(loc)
	}
	if !pr.CreatedAt.IsZero() {
		return pr.CreatedAt.In(loc)
	}
	return time.Now().In(loc)
}

var parenPattern = regexp.MustCompile(`\([^)]*\)|（[^）]*）`)

func normalizeNameTokens(s string) []string {
	if s == "" {
		return nil
	}
	s = parenPattern.ReplaceAllString(s, " ")
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	parts := strings.Fields(b.String())
	if len(parts) == 0 {
		return nil
	}
	return parts
}

func nameMatches(teamEntry, candidate string) bool {
	teamTokens := normalizeNameTokens(teamEntry)
	candTokens := normalizeNameTokens(candidate)
	if len(teamTokens) == 0 || len(candTokens) == 0 {
		return false
	}
	if allIn(teamTokens, candTokens) || allIn(candTokens, teamTokens) {
		return true
	}
	return false
}

func allIn(needles, haystack []string) bool {
	set := make(map[string]bool, len(haystack))
	for _, t := range haystack {
		set[t] = true
	}
	for _, t := range needles {
		if !set[t] {
			return false
		}
	}
	return true
}

func anyNameMatches(teamEntries []string, candidate string) bool {
	for _, entry := range teamEntries {
		if nameMatches(entry, candidate) {
			return true
		}
	}
	return false
}
