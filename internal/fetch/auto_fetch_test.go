package fetch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/storage/sqlite"
)

// defaultSource makes unqualified gitlab: author_ids keys resolve, as in production.
func defaultSource() []config.GitLabSource {
	return []config.GitLabSource{{Name: "dops", Default: true}}
}

func TestFormatFetchSummary_AllFailed(t *testing.T) {
	result := FetchResult{
		TotalFetched: 0,
		Errors:       []string{"GitLab: connection refused"},
	}
	got := FormatFetchSummary(result)
	want := "Error fetching MRs/PRs:\nGitLab: connection refused"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFetchSummary_NoneToAdd(t *testing.T) {
	result := FetchResult{
		TotalFetched:   10,
		AlreadyTracked: 7,
		SkippedNonTeam: 3,
	}
	got := FormatFetchSummary(result)
	want := "Found 10 MRs/PRs/commits, none to add (7 already tracked, 3 non-team)."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFetchSummary_NoneToAddOnlyTracked(t *testing.T) {
	result := FetchResult{
		TotalFetched:   5,
		AlreadyTracked: 5,
	}
	got := FormatFetchSummary(result)
	want := "Found 5 MRs/PRs/commits, none to add (5 already tracked)."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFetchSummary_SomeInserted(t *testing.T) {
	result := FetchResult{
		TotalFetched:   15,
		Inserted:       8,
		AlreadyTracked: 5,
		SkippedNonTeam: 2,
	}
	got := FormatFetchSummary(result)
	want := "Fetched 15 MRs/PRs/commits: 8 new, 5 already tracked, 2 non-team"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFetchSummary_InsertedWithWarnings(t *testing.T) {
	result := FetchResult{
		TotalFetched: 10,
		Inserted:     3,
		Errors:       []string{"GitHub: rate limited"},
	}
	got := FormatFetchSummary(result)
	want := "Fetched 10 MRs/PRs/commits: 3 new\nWarnings:\nGitHub: rate limited"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFetchSummary_ZeroFetchedNoErrors(t *testing.T) {
	result := FetchResult{
		TotalFetched: 0,
	}
	got := FormatFetchSummary(result)
	want := "Found 0 MRs/PRs/commits, none to add."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFetchAndImportMRs_NeitherConfigured(t *testing.T) {
	cfg := Config{}
	_, err := FetchAndImportMRs(cfg, nil, Hooks{})
	if err == nil {
		t.Fatal("expected error when neither source is configured")
	}
	if got := err.Error(); got != "neither GitLab nor GitHub is configured" {
		t.Errorf("unexpected error: %q", got)
	}
}

func TestImportCodeChangesLinkMergeAndOwnItems(t *testing.T) {
	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "fetch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	open, _ := sqlite.InsertWorkItem(db, domain.WorkItem{Description: "shard timeout", Author: "Dev A", Source: "slack",
		Status: domain.StatusInProgress, TicketIDs: "1234567", ReportedAt: time.Now(), FilingState: domain.FilingFiled})

	now := time.Now()
	changes := []fetched{
		{cc: domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/1", Title: "fix shard timeout", Author: "Dev A", TicketIDs: "1234567", State: "open"}, reportedAt: now},
		{cc: domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/2", Title: "new exporter", Author: "Dev B", State: "merged"}, host: "gitlab", accountID: 42, reportedAt: now},
		{cc: domain.CodeChange{Source: "github", SourceRef: "https://git.example/pr/3", Title: "abandoned", Author: "Dev B", State: "closed"}, reportedAt: now},
	}
	cfg := config.Config{AuthorIDs: map[string]string{"gitlab:42": "UDEVB0001"}, GitLabSources: defaultSource()}
	hooks := Hooks{ResolveAccount: cfg.AccountAuthorID}
	var res FetchResult
	if err := importCodeChanges(db, changes, hooks, &res); err != nil {
		t.Fatal(err)
	}
	if res.Linked != 1 || res.Inserted != 1 || res.SkippedClosed != 1 || res.MovedToTesting != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// The exporter became its own Work Item: merged means in testing, never done.
	cc, ok, _ := sqlite.GetCodeChangeByRef(db, "gitlab", "https://git.example/mr/2")
	own, _ := sqlite.GetWorkItemByID(db, cc.WorkItemID)
	if !ok || cc.LinkKind != domain.LinkOwn || own.Status != domain.StatusInTesting || own.FilingState != domain.FilingPending || own.AuthorID != "UDEVB0001" {
		t.Fatalf("unexpected own Work Item: %+v %+v", cc, own)
	}

	// Next fetch: the linked MR merged, so its Work Item moves to in testing.
	changes[0].cc.State = "merged"
	res = FetchResult{}
	if err := importCodeChanges(db, changes[:1], hooks, &res); err != nil {
		t.Fatal(err)
	}
	if it, _ := sqlite.GetWorkItemByID(db, open); it.Status != domain.StatusInTesting || res.MovedToTesting != 1 || res.AlreadyTracked != 1 {
		t.Fatalf("expected in testing after merge: %+v %+v", it, res)
	}
}

func TestCodeChangeOwnersBindOnlyByAccountID(t *testing.T) {
	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "bind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Plain keys are report names; a code-hosting display name or username equal to one must not bind,
	// nor may a display name equal to a username key.
	cfg := config.Config{GitLabSources: defaultSource(), AuthorIDs: map[string]string{"dana lee": "UDANA0001", "dlee": "UDANA0001",
		"gitlab:7": "UREAL0007", "github:9": "UREAL0009", "gitlab:grant": "UGRANT0001"}}
	now := time.Now()
	changes := []fetched{
		{cc: domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/a", Title: "a", Author: "Dana Lee", State: "open"}, host: "gitlab", accountID: 100, username: "dana", reportedAt: now},
		{cc: domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/b", Title: "b", Author: "dlee", State: "open"}, host: "gitlab", accountID: 101, username: "dlee", reportedAt: now},
		{cc: domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/c", Title: "c", Author: "Anyone", State: "open"}, host: "gitlab", accountID: 7, username: "anyone", reportedAt: now},
		{cc: domain.CodeChange{Source: "github", SourceRef: "https://git.example/pr/d", Title: "d", Author: "Dana Lee", State: "open"}, host: "github", accountID: 7, username: "dana", reportedAt: now},
		{cc: domain.CodeChange{Source: "github", SourceRef: "https://git.example/pr/e", Title: "e", Author: "dlee", State: "open"}, host: "github", accountID: 9, username: "dlee", reportedAt: now},
		{cc: domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/f", Title: "f", Author: "Grant", State: "open"}, host: "gitlab", accountID: 102, username: "gguo", reportedAt: now},
	}
	var res FetchResult
	if err := importCodeChanges(db, changes, Hooks{ResolveAccount: cfg.AccountAuthorID}, &res); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "", "b": "", "c": "UREAL0007", "d": "", "e": "UREAL0009", "f": ""}
	for _, f := range changes {
		cc, _, _ := sqlite.GetCodeChangeByRef(db, f.cc.Source, f.cc.SourceRef)
		it, _ := sqlite.GetWorkItemByID(db, cc.WorkItemID)
		if it.AuthorID != want[f.cc.Title] {
			t.Errorf("%s: author_id %q, want %q", f.cc.Title, it.AuthorID, want[f.cc.Title])
		}
	}
}

func TestCodeChangeOwnersBindByUsernameKey(t *testing.T) {
	buf := captureLog(t)
	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "bind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := config.Config{GitLabSources: defaultSource(), AuthorIDs: map[string]string{"gitlab:yfma": "UYFMA0001", "github:octo-cat": "UOCTO0001",
		"gitlab:24": "UID000024", "gitlab:dlee": "UDLEE0001"}}
	now := time.Now()
	changes := []fetched{
		{cc: domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/a", Title: "a", Author: "Dev Y", State: "open"}, host: "gitlab", accountID: 31, username: "YFMA", reportedAt: now},
		{cc: domain.CodeChange{Source: "github", SourceRef: "https://git.example/pr/b", Title: "b", Author: "Octo-Cat", State: "open"}, host: "github", accountID: 5, username: "Octo-Cat", reportedAt: now},
		{cc: domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/c", Title: "c", Author: "Dana Lee", State: "open"}, host: "gitlab", accountID: 24, username: "dlee", reportedAt: now},
	}
	if err := importCodeChanges(db, changes, Hooks{ResolveAccount: cfg.AccountAuthorID}, &FetchResult{}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "UYFMA0001", "b": "UOCTO0001", "c": "UID000024"}
	for _, f := range changes {
		cc, _, _ := sqlite.GetCodeChangeByRef(db, f.cc.Source, f.cc.SourceRef)
		it, _ := sqlite.GetWorkItemByID(db, cc.WorkItemID)
		if it.AuthorID != want[f.cc.Title] {
			t.Errorf("%s: author_id %q, want %q", f.cc.Title, it.AuthorID, want[f.cc.Title])
		}
	}
	if log := buf.String(); !strings.Contains(log, `"gitlab:24"`) || !strings.Contains(log, `"gitlab:dlee"`) {
		t.Fatalf("conflicting ID and username keys must be warned about: %q", log)
	}
}

// End to end with a fake GitLab: the MR's author.username reaches binding.
func TestFetchBindsGitLabUsername(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			w.Write([]byte("[]"))
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{{"title": "speed up export", "web_url": "https://git.example/mr/1",
			"updated_at": now, "created_at": now, "state": "opened",
			"author": map[string]any{"id": 31, "username": "yfma", "name": "Dev Y"}}})
	}))
	defer srv.Close()
	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "fetch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Loaded through config so the keys fold as in production.
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := "gitlab_sources:\n  - name: dops\n    url: " + srv.URL + "\n    token: test\n    group_id: g\n    default: true\n"
	if err := os.WriteFile(cfgPath, []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", cfgPath)
	for k, v := range map[string]string{"SLACK_BOT_TOKEN": "xoxb-test", "SLACK_APP_TOKEN": "xapp-test", "TIMEZONE": "UTC",
		"DECISION_MODEL_URL": "https://api.compat.example/", "DECISION_MODEL_API_KEY": "dm-test", "DECISION_MODEL_NAME": "test-model",
		"AUTHOR_IDS": `{"gitlab:YFMA": "UYFMA0001"}`} {
		t.Setenv(k, v)
	}
	cfg := config.LoadConfig()
	cfg.GitHubToken = "" // never reach the real GitHub from a test
	if _, err := FetchAndImportMRs(cfg, db, Hooks{ResolveAccount: cfg.AccountAuthorID}); err != nil {
		t.Fatal(err)
	}
	cc, _, _ := sqlite.GetCodeChangeByRef(db, "gitlab", "https://git.example/mr/1")
	if it, _ := sqlite.GetWorkItemByID(db, cc.WorkItemID); it.AuthorID != "UYFMA0001" {
		t.Fatalf("author_id %q, want the username key's Slack ID", it.AuthorID)
	}
}

func TestGitHubTicketIDs(t *testing.T) {
	if got := githubTicketIDs(GitHubPR{Body: "Summary\n\nJira: 1234567, 7654321\n", Title: "fix 1111111"}, "Jira"); got != "1234567,7654321" {
		t.Fatalf("description ticket field first, got %q", got)
	}
	if got := githubTicketIDs(GitHubPR{Title: "[2345678] fix shard timeout"}, "Jira"); got != "2345678" {
		t.Fatalf("title tickets otherwise, got %q", got)
	}
	if got := githubTicketIDs(GitHubPR{Title: "bump to 7.6.3 build 1459"}, "Jira"); got != "" {
		t.Fatalf("no ticket in a version bump, got %q", got)
	}
}

// End to end: a failing source is reported and the other sources still fetch.
func TestFetchContinuesWhenOneSourceFails(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	mrs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"title": "healthy change", "web_url": "https://git.example/mr/1",
			"updated_at": now, "created_at": now, "state": "opened",
			"author": map[string]any{"id": 1, "username": "yliu", "name": "Yufan Liu"}}})
	}))
	defer mrs.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", 401)
	}))
	defer broken.Close()

	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "fetch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := config.Config{Location: time.UTC,
		GitLabSources: []config.GitLabSource{
			{Name: "dops", URL: mrs.URL, Token: "test", GroupID: "g", Default: true},
			{Name: "corp", URL: broken.URL, Token: "bad", Project: "my-group/my-project", Branch: "br_8-0"},
		},
		TeamMembers: []string{"Yufan Liu"}}
	res, err := FetchAndImportMRs(cfg, db, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 1 {
		t.Fatalf("healthy source must still import: %+v", res)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "corp/") {
		t.Fatalf("broken source must be reported with its label: %+v", res.Errors)
	}
	summary := FormatFetchSummary(res)
	if !strings.Contains(summary, "Warnings:") || !strings.Contains(summary, "corp/") {
		t.Fatalf("summary must warn per source: %q", summary)
	}
}

// End to end: branch commits become done Work Items owned via the plain name key.
func TestFetchCommitsBecomeDoneWorkItems(t *testing.T) {
	now := time.Now().UTC()
	in := now.Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/repository/commits") {
			http.Error(w, "unexpected path", 404)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "aaa111", "title": "Mantis 0987654: fix unified image build", "message": "Mantis 0987654: fix unified image build",
				"author_name": "Dana Lee", "author_email": "dlee@example.com", "committed_date": in, "authored_date": in,
				"web_url": "https://corp.example/my-group/my-project/-/commit/aaa111"},
			{"id": "bbb222", "title": "Merge branch 'x' into br_8-0", "message": "merge",
				"author_name": "Dana Lee", "author_email": "dlee@example.com", "committed_date": in, "authored_date": in,
				"web_url": "https://corp.example/my-group/my-project/-/commit/bbb222"},
		})
	}))
	defer srv.Close()

	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "fetch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := config.Config{Location: time.UTC,
		GitLabSources: []config.GitLabSource{{Name: "corp", URL: srv.URL, Token: "test", Project: "my-group/my-project", Branch: "br_8-0", RefTicketLabel: "Mantis", Default: true}},
		TeamMembers:   []string{"Dana"}, AuthorIDs: map[string]string{"dana lee": "UDANA0001"}} // keys are case-folded, as LoadConfig does
	res, err := FetchAndImportMRs(cfg, db, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 1 {
		t.Fatalf("only the real commit imports (merge commit skipped): %+v", res)
	}
	cc, ok, _ := sqlite.GetCodeChangeByRef(db, "gitlab", "https://corp.example/my-group/my-project/-/commit/aaa111")
	if !ok {
		t.Fatal("commit must be tracked as a Code Change")
	}
	it, _ := sqlite.GetWorkItemByID(db, cc.WorkItemID)
	if it.Status != domain.StatusDone {
		t.Fatalf("commit Work Item status %q, want done", it.Status)
	}
	if it.AuthorID != "UDANA0001" {
		t.Fatalf("commit binds by plain name key, got %q", it.AuthorID)
	}
	if it.TicketIDs != "0987654" {
		t.Fatalf("commit ticket IDs %q, want 0987654", it.TicketIDs)
	}
}
