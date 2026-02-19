package nudge

import (
	"reportbot/internal/config"
	"reportbot/internal/domain"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

type Config = config.Config

func ReportWeekRange(cfg Config, now time.Time) (time.Time, time.Time) {
	return domain.ReportWeekRange(cfg, now)
}

func resolveUserIDs(api *slack.Client, identifiers []string) ([]string, []string, error) {
	var ids []string
	var names []string
	for _, raw := range identifiers {
		val := strings.TrimSpace(raw)
		if val == "" {
			continue
		}
		if isLikelySlackID(val) {
			ids = append(ids, val)
		} else {
			names = append(names, val)
		}
	}
	if len(names) == 0 {
		return uniqueStrings(ids), nil, nil
	}
	users, err := api.GetUsers()
	if err != nil {
		return uniqueStrings(ids), names, err
	}
	nameToID := make(map[string]string)
	for _, user := range users {
		addName := func(n string) {
			n = strings.ToLower(strings.TrimSpace(n))
			if n == "" {
				return
			}
			if _, exists := nameToID[n]; !exists {
				nameToID[n] = user.ID
			}
		}
		addName(user.Name)
		addName(user.RealName)
		addName(user.Profile.DisplayName)
	}
	var unresolved []string
	for _, name := range names {
		key := strings.ToLower(strings.TrimSpace(name))
		if id, ok := nameToID[key]; ok {
			ids = append(ids, id)
		} else {
			unresolved = append(unresolved, name)
		}
	}
	return uniqueStrings(ids), unresolved, nil
}

func isLikelySlackID(val string) bool {
	if len(val) < 9 {
		return false
	}
	for i, r := range val {
		if i == 0 {
			if r != 'U' && r != 'W' {
				return false
			}
			continue
		}
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func uniqueStrings(vals []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, v := range vals {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
