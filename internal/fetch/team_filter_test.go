package fetch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/storage/sqlite"
)

const grantID = "UGRANT0001"

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// profileNames fakes the Slack profile lookup: Grant's real name and display name.
func profileNames(t *testing.T) func([]string) ([]string, error) {
	return func(ids []string) ([]string, error) {
		if !reflect.DeepEqual(ids, []string{grantID}) {
			t.Errorf("TeamNames asked for %v, want only the Slack-ID entries", ids)
		}
		return []string{"Grant Guo", "grantg"}, nil
	}
}

func TestTeamFilter(t *testing.T) {
	cfg := config.Config{TeamMembers: []string{"Yufan", grantID},
		AuthorIDs: map[string]string{"gitlab:42": grantID, "github:77": grantID}}
	f := newTeamFilter(cfg, Hooks{TeamNames: profileNames(t)})

	cases := []struct {
		name           string
		host           string
		accountID      int64
		author, handle string
		want           bool
	}{
		{"name entry", "gitlab", 1, "Yufan Liu", "yliu", true},
		{"ID entry via Slack profile real name", "gitlab", 2, "Grant Guo", "gguo", true},
		{"ID entry via author_ids", "gitlab", 42, "Build Bot", "bot", true},
		{"non-team", "gitlab", 3, "Andy Xu", "axu", false},
		{"github name entry", "github", 1, "yufan", "yufan", true},
		{"github ID entry via Slack profile display name", "github", 2, "grantg", "grantg", true},
		{"github ID entry via author_ids", "github", 77, "octo", "octo", true},
		{"author_ids is per host", "github", 42, "octo", "octo", false},
		{"github non-team", "github", 3, "andyxu", "andyxu", false},
	}
	for _, c := range cases {
		if got := f.includes(c.host, c.accountID, c.author, c.handle); got != c.want {
			t.Errorf("%s: includes = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTeamFilterEmptyTeamIncludesAll(t *testing.T) {
	f := newTeamFilter(config.Config{}, Hooks{TeamNames: func([]string) ([]string, error) {
		t.Fatal("no lookup without team_members")
		return nil, nil
	}})
	if !f.includes("gitlab", 3, "Andy Xu", "axu") {
		t.Fatal("empty team_members imports everyone")
	}
}

func TestTeamFilterLookupErrorFallsBackToNamesAndAuthorIDs(t *testing.T) {
	buf := captureLog(t)
	cfg := config.Config{TeamMembers: []string{"Yufan", grantID}, AuthorIDs: map[string]string{"gitlab:42": grantID}}
	f := newTeamFilter(cfg, Hooks{TeamNames: func([]string) ([]string, error) { return nil, errors.New("slack down") }})

	if !f.includes("gitlab", 1, "Yufan Liu", "yliu") || !f.includes("gitlab", 42, "Build Bot", "bot") {
		t.Fatal("name entries and author_ids still apply when the lookup fails")
	}
	if f.includes("gitlab", 2, "Grant Guo", "gguo") {
		t.Fatal("profile names are unknown when the lookup fails")
	}
	if !strings.Contains(buf.String(), "slack down") {
		t.Fatalf("lookup error not logged: %q", buf.String())
	}
}

// End to end through FetchAndImportMRs with a fake GitLab: the skip count is warned about.
func TestFetchWarnsWhenLookupFailedAndMRsSkipped(t *testing.T) {
	buf := captureLog(t)
	now := time.Now().UTC().Format(time.RFC3339)
	mr := func(n int, id int64, name, user string) map[string]any {
		return map[string]any{"title": name + " change", "web_url": fmt.Sprintf("https://git.example/mr/%d", n),
			"updated_at": now, "created_at": now, "state": "opened",
			"author": map[string]any{"id": id, "username": user, "name": name}}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			w.Write([]byte("[]"))
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{
			mr(1, 1, "Yufan Liu", "yliu"), mr(2, 42, "Build Bot", "bot"), mr(3, 2, "Grant Guo", "gguo"), mr(4, 3, "Andy Xu", "axu")})
	}))
	defer srv.Close()

	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "fetch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := config.Config{Location: time.UTC, GitLabURL: srv.URL, GitLabToken: "test", GitLabGroupID: "g",
		TeamMembers: []string{"Yufan", grantID}, AuthorIDs: map[string]string{"gitlab:42": grantID}}
	hooks := Hooks{TeamNames: func([]string) ([]string, error) { return nil, errors.New("slack down") }}

	res, err := FetchAndImportMRs(cfg, db, hooks)
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 2 || res.SkippedNonTeam != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(buf.String(), "2 MRs/PRs skipped as non-team") {
		t.Fatalf("no warning with the skip count: %q", buf.String())
	}
}
