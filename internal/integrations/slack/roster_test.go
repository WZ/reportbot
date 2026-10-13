package slackbot

import (
	"fmt"
	"strings"
	"testing"
	"time"

	sqlitedb "reportbot/internal/storage/sqlite"

	"github.com/slack-go/slack"
)

func user(id, real, display string) slack.User {
	u := slack.User{ID: id, Name: "handle-" + id, RealName: real}
	u.Profile.DisplayName = display
	return u
}

func lookup(workspace map[string]slack.User) func(string) (*slack.User, error) {
	return func(id string) (*slack.User, error) {
		u, ok := workspace[id]
		if !ok {
			return nil, fmt.Errorf("user_not_found")
		}
		return &u, nil
	}
}

func TestSuggestAuthorIDs(t *testing.T) {
	workspace := map[string]slack.User{
		"UROSTER001": user("UROSTER001", "Dana Lee", "Dana"),
		"UROSTER002": user("UROSTER002", "Sam Park", "Sam"),
		"UROSTER003": user("UROSTER003", "Robin Hale", "Sam"),
		"UOUTSIDE01": user("UOUTSIDE01", "Taylor Kim", "Taylor"),
	}
	cfg := Config{ManagerSlackIDs: []string{"UROSTER001"}, TeamMembers: []string{"UROSTER002", "UROSTER003", "Some Name"}}
	accounts := []Account{{Host: "gitlab", ID: 42, Username: "dlee", Name: "Dana Lee"}, {Host: "github", ID: 9, Username: "tkim", Name: "tkim"},
		{Host: "gitlab", ID: 42, Username: "dlee", Name: "Dana Lee"}, {Host: "gitlab", ID: 51, Username: "", Name: "Sam Park"},
		{Host: "gitlab", Server: "corp", ID: 42, Username: "dlee", Name: "Dana Lee"}}
	out, err := SuggestAuthorIDs(cfg, []string{"Dana Lee", "Sam", "Taylor", "dana lee"}, accounts, lookup(workspace))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"unverified", `"Dana Lee": "UROSTER001"`, `# "Sam": several roster members`, `# "Taylor": no roster member`,
		`"gitlab:dlee": "UROSTER001"  # id 42, Dana Lee`, `# "github:tkim": no roster member has this exact name  # id 9, tkim`,
		`"gitlab:51": "UROSTER002"  # Sam Park`,
		`"gitlab@corp:dlee": "UROSTER001"  # id 42, Dana Lee`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, `"gitlab:dlee"`) != 1 || strings.Contains(out, `"gitlab:42"`) {
		t.Errorf("one username key per account, not its numeric key:\n%s", out)
	}
	if strings.Contains(out, "UOUTSIDE01") {
		t.Fatal("users outside the roster are never suggested")
	}

	cfg.TeamMembers = append(cfg.TeamMembers, "UMISSING01")
	if out, err := SuggestAuthorIDs(cfg, []string{"Dana Lee"}, accounts, lookup(workspace)); err == nil || out != "" {
		t.Fatalf("a failed roster lookup must give no suggestions: %q %v", out, err)
	}
}

// The mock users.info would match "Dev A" by display name; only author_ids may bind.
func TestDelegatedReportBindsOnlyViaAuthorIDs(t *testing.T) {
	db := newTestDB(t)
	api, _ := newSlackRecorder(t)
	cfg := Config{Location: time.UTC, ManagerSlackIDs: []string{"UMGR"}, TeamMembers: []string{"Dev A", "Dev B"},
		AuthorIDs: map[string]string{"dev b": "UDEVB0001"}}
	for _, text := range []string{"{Dev A} unmapped work (done)", "{Dev B} mapped work (done)"} {
		handleReport(api, db, cfg, slack.SlashCommand{Command: "/report", UserID: "UMGR", ChannelID: "C1", Text: text})
	}
	items, _ := sqlitedb.GetItemsByDateRange(db, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	got := map[string]string{}
	for _, it := range items {
		got[it.Author] = it.AuthorID
	}
	if got["Dev A"] != "" || got["Dev B"] != "UDEVB0001" {
		t.Fatalf("unexpected bindings: %v", got)
	}
	if id, _ := FetchHooks(api, cfg).ResolveAccount("gitlab", "", 1, "dev b"); id != "" {
		t.Fatal("fetch binds only gitlab:/github: account keys")
	}
}
