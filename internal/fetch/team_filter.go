package fetch

import (
	"log"
	"strings"

	"reportbot/internal/config"
)

// teamFilter is an import filter only: it decides which Code Changes are imported,
// never who owns them (ownership binds solely via author_ids account keys).
type teamFilter struct {
	cfg       Config
	names     []string        // name entries, plus Slack profile names of the ID entries
	ids       map[string]bool // Slack user ID entries
	lookupErr error
}

func newTeamFilter(cfg Config, hooks Hooks) *teamFilter {
	f := &teamFilter{cfg: cfg, ids: map[string]bool{}}
	var ids []string
	for _, m := range cfg.TeamMembers {
		m = strings.TrimSpace(m)
		switch {
		case config.IsSlackUserID(m):
			if !f.ids[m] {
				f.ids[m] = true
				ids = append(ids, m)
			}
		case m != "":
			f.names = append(f.names, m)
		}
	}
	if len(ids) > 0 && hooks.TeamNames != nil {
		names, err := hooks.TeamNames(ids)
		if err != nil {
			f.lookupErr = err
			log.Printf("auto-fetch WARNING: Slack profile lookup for team_members IDs failed, matching by name entries and author_ids only: %v", err)
		}
		f.names = append(f.names, names...)
	}
	return f
}

// includes reports whether a Code Change author is a team member: by a name entry or a
// Slack profile name of an ID entry (name or username), or by an author_ids account key
// (numeric ID or username) mapping to an ID entry.
func (f *teamFilter) includes(host string, accountID int64, name, username string) bool {
	if len(f.cfg.TeamMembers) == 0 {
		return true
	}
	if id, _ := f.cfg.AccountAuthorID(host, accountID, username); f.ids[id] {
		return true
	}
	return anyNameMatches(f.names, name) || anyNameMatches(f.names, username)
}

// warnSkipped logs how many skips may be wrong because the Slack profile lookup failed.
func (f *teamFilter) warnSkipped(skipped int) {
	if f.lookupErr != nil && skipped > 0 {
		log.Printf("auto-fetch WARNING: %d MRs/PRs skipped as non-team while the Slack profile lookup was failing; authors listed in team_members only by Slack ID may be among them", skipped)
	}
}
