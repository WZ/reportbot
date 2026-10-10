package slackbot

import (
	"fmt"
	"testing"

	"reportbot/internal/fetch"

	"github.com/slack-go/slack"
)

func user(id, real, display string) slack.User {
	u := slack.User{ID: id, Name: "handle-" + id, RealName: real}
	u.Profile.DisplayName = display
	return u
}

func testRoster(cfg Config, workspace map[string]slack.User) *Roster {
	r := NewRoster(slack.New("x"), cfg)
	r.getUser = func(id string) (*slack.User, error) {
		u, ok := workspace[id]
		if !ok {
			return nil, fmt.Errorf("user_not_found")
		}
		return &u, nil
	}
	return r
}

func TestRosterBindsOnlyUniqueRosterMembers(t *testing.T) {
	workspace := map[string]slack.User{
		"UROSTER001": user("UROSTER001", "Dana Lee", "Dana"),
		"UROSTER002": user("UROSTER002", "Sam Park", "Sam"),
		"UROSTER003": user("UROSTER003", "Robin Hale", "Sam"), // renamed to copy a colleague
		"UOUTSIDE01": user("UOUTSIDE01", "Taylor Kim", "Dana Lee"),
	}
	cfg := Config{ManagerSlackIDs: []string{"UROSTER001"}, TeamMembers: []string{"UROSTER002", "UROSTER003", "Some Name"}}
	r := testRoster(cfg, workspace)

	if id, _ := r.Bind("dana lee"); id != "UROSTER001" {
		t.Fatalf("a unique roster match binds, got %q", id)
	}
	if id, why := r.Bind("Taylor Kim"); id != "" || why != "no roster member has this name" {
		t.Fatalf("a workspace user outside the roster is never bound: %q %q", id, why)
	}
	if id, why := r.Bind("Sam"); id != "" || why != "several roster members have this name" {
		t.Fatalf("two roster members with the same name stay unbound: %q %q", id, why)
	}
	if id, _ := r.Bind("Dana Le"); id != "" {
		t.Fatal("matching is exact")
	}
	if id, why := testRoster(Config{TeamMembers: []string{"Dana Lee"}}, workspace).Bind("Dana Lee"); id != "" || why == "" {
		t.Fatalf("team_members given as names give no roster: %q %q", id, why)
	}
}

func TestFetchHooksUseTheRoster(t *testing.T) {
	workspace := map[string]slack.User{
		"UROSTER001": user("UROSTER001", "Dana Lee", "Dana"),
		"UOUTSIDE01": user("UOUTSIDE01", "Taylor Kim", "Taylor"),
	}
	r := testRoster(Config{TeamMembers: []string{"UROSTER001"}}, workspace)
	hooks := fetch.Hooks{ResolveAuthorID: r.ID}
	if got := hooks.ResolveAuthorID("Dana Lee"); got != "UROSTER001" {
		t.Fatalf("expected the roster member, got %q", got)
	}
	if got := hooks.ResolveAuthorID("Taylor"); got != "" {
		t.Fatalf("an author outside the roster stays unbound, got %q", got)
	}
}
