package slackbot

import (
	"fmt"
	"sort"
	"strings"

	"github.com/slack-go/slack"
)

// SuggestAuthorIDs drafts author_ids for review (it grants nothing) from exact roster-name
// matches; it fails closed if any roster lookup fails.
func SuggestAuthorIDs(cfg Config, authors []string, accounts []Account, getUser func(id string) (*slack.User, error)) (string, error) {
	seen := map[string]bool{}
	var users []slack.User
	for _, id := range append(append([]string{}, cfg.ManagerSlackIDs...), cfg.TeamMembers...) {
		id = strings.TrimSpace(id)
		if !isLikelySlackID(id) || seen[id] {
			continue
		}
		seen[id] = true
		u, err := getUser(id)
		if err != nil {
			return "", fmt.Errorf("looking up roster member %s: %w", id, err)
		}
		users = append(users, *u)
	}
	if len(users) == 0 {
		return "", fmt.Errorf("no Slack user IDs in manager_slack_ids or team_members")
	}

	match := func(names ...string) []string {
		var ids []string
		for _, u := range users {
		user:
			for _, n := range []string{u.RealName, u.Profile.RealName, u.Profile.DisplayName, u.Name} {
				for _, want := range names {
					if want = strings.TrimSpace(want); want != "" && strings.EqualFold(strings.TrimSpace(n), want) {
						ids = append(ids, u.ID)
						break user
					}
				}
			}
		}
		return ids
	}
	line := func(b *strings.Builder, key, note string, ids []string) {
		switch len(ids) {
		case 1:
			fmt.Fprintf(b, "  %q: %q%s\n", key, ids[0], note)
		case 0:
			fmt.Fprintf(b, "  # %q: no roster member has this exact name%s\n", key, note)
		default:
			fmt.Fprintf(b, "  # %q: several roster members have this name (%s)%s\n", key, strings.Join(ids, ", "), note)
		}
	}

	uniq := map[string]bool{}
	var names []string
	for _, a := range authors {
		if a = strings.TrimSpace(a); a != "" && !uniq[strings.ToLower(a)] {
			uniq[strings.ToLower(a)] = true
			names = append(names, a)
		}
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# Suggested author_ids — unverified, check every line before use.\nauthor_ids:\n")
	b.WriteString("  # report author names (used by the migrate-v2 import)\n")
	for _, name := range names {
		line(&b, name, "", match(name))
	}
	b.WriteString("  # code-hosting accounts by numeric ID (used by fetch)\n")
	seenAcct := map[string]bool{}
	for _, a := range accounts {
		key := fmt.Sprintf("%s:%d", a.Host, a.ID)
		if a.ID <= 0 || seenAcct[key] {
			continue
		}
		seenAcct[key] = true
		line(&b, key, fmt.Sprintf("  # %s, %s", a.Username, a.Name), match(a.Name, a.Username))
	}
	return b.String(), nil
}

// Account is a code-hosting account seen on a recent MR/PR.
type Account struct {
	Host     string // "gitlab" | "github"
	ID       int64
	Username string
	Name     string
}
