package slackbot

import (
	"fmt"
	"sort"
	"strings"

	"github.com/slack-go/slack"
)

// SuggestAuthorIDs proposes author_ids entries for the owner to review; it grants nothing.
// It matches each author exactly against the names of the roster (manager_slack_ids plus
// team_members given as Slack user IDs) and fails closed if any roster lookup fails.
func SuggestAuthorIDs(cfg Config, authors []string, getUser func(id string) (*slack.User, error)) (string, error) {
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
	for _, name := range names {
		var matches []string
		for _, u := range users {
			for _, n := range []string{u.RealName, u.Profile.RealName, u.Profile.DisplayName, u.Name} {
				if strings.EqualFold(strings.TrimSpace(n), name) {
					matches = append(matches, u.ID)
					break
				}
			}
		}
		switch len(matches) {
		case 1:
			fmt.Fprintf(&b, "  %q: %q\n", name, matches[0])
		case 0:
			fmt.Fprintf(&b, "  # %q: no roster member has this exact name\n", name)
		default:
			fmt.Fprintf(&b, "  # %q: several roster members have this name (%s)\n", name, strings.Join(matches, ", "))
		}
	}
	return b.String(), nil
}
