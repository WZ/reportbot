package slackbot

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

// Roster binds free-text author names to Slack user IDs. An author_id grants edit
// rights and receives DMs, and names are user-editable, so only the configured roster
// (manager_slack_ids plus team_members given as Slack user IDs) is ever considered,
// and only a unique exact match binds.
type Roster struct {
	ids     []string
	getUser func(id string) (*slack.User, error)

	mu        sync.Mutex
	users     []slack.User
	fetchedAt time.Time
}

func NewRoster(api *slack.Client, cfg Config) *Roster {
	seen := map[string]bool{}
	var ids []string
	for _, v := range append(append([]string{}, cfg.ManagerSlackIDs...), cfg.TeamMembers...) {
		v = strings.TrimSpace(v)
		if isLikelySlackID(v) && !seen[v] {
			seen[v] = true
			ids = append(ids, v)
		}
	}
	return &Roster{ids: ids, getUser: api.GetUserInfo}
}

func (r *Roster) rosterUsers() []slack.User {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.users != nil && time.Since(r.fetchedAt) < userCacheTTL {
		return r.users
	}
	users := make([]slack.User, 0, len(r.ids))
	for _, id := range r.ids {
		u, err := r.getUser(id)
		if err != nil {
			log.Printf("roster: users.info %s: %v", id, err)
			continue
		}
		users = append(users, *u)
	}
	r.users, r.fetchedAt = users, time.Now()
	return users
}

// Bind returns the roster user's ID for name, or "" and why it stayed unbound.
func (r *Roster) Bind(name string) (string, string) {
	if r == nil {
		return "", "no roster"
	}
	return bindAuthor(name, r.rosterUsers())
}

// User returns the roster user uniquely matching name.
func (r *Roster) User(name string) (slack.User, bool) {
	id, _ := r.Bind(name)
	if id == "" {
		return slack.User{}, false
	}
	for _, u := range r.rosterUsers() {
		if u.ID == id {
			return u, true
		}
	}
	return slack.User{}, false
}

func displayName(u slack.User) string {
	for _, n := range []string{u.Profile.DisplayName, u.RealName, u.Name} {
		if n = strings.TrimSpace(n); n != "" {
			return n
		}
	}
	return u.ID
}

// ID is Bind without the reason.
func (r *Roster) ID(name string) string {
	id, _ := r.Bind(name)
	return id
}

func bindAuthor(name string, users []slack.User) (string, string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "empty name"
	}
	if len(users) == 0 {
		return "", "no Slack user IDs in manager_slack_ids or team_members"
	}
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
		return matches[0], ""
	case 0:
		return "", "no roster member has this name"
	default:
		return "", "several roster members have this name"
	}
}
