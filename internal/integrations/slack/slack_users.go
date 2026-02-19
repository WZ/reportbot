package slackbot

import (
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

const userCacheTTL = 5 * time.Minute

var userCache struct {
	sync.Mutex
	users     []slack.User
	fetchedAt time.Time
}

func getCachedUsers(api *slack.Client) ([]slack.User, error) {
	userCache.Lock()
	defer userCache.Unlock()

	if userCache.users != nil && time.Since(userCache.fetchedAt) < userCacheTTL {
		return userCache.users, nil
	}

	users, err := api.GetUsers()
	if err != nil {
		return nil, err
	}
	userCache.users = users
	userCache.fetchedAt = time.Now()
	return users, nil
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
		log.Printf("resolve users: ids=%d names=0", len(ids))
		return uniqueStrings(ids), nil, nil
	}

	users, err := getCachedUsers(api)
	if err != nil {
		log.Printf("resolve users: get users error: %v", err)
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

	log.Printf("resolve users: ids=%d unresolved=%d", len(ids), len(unresolved))
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
	// Check both directions: team⊆candidate OR candidate⊆team.
	// This handles short GitLab names (e.g. "Alice" matching "Alice Smith").
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
