package fetch

import (
	"path/filepath"
	"testing"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/storage/sqlite"
)

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
	want := "Found 10 MRs/PRs (merged+open), none to add (7 already tracked, 3 non-team)."
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
	want := "Found 5 MRs/PRs (merged+open), none to add (5 already tracked)."
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
	want := "Fetched 15 MRs/PRs (merged+open): 8 new, 5 already tracked, 2 non-team"
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
	want := "Fetched 10 MRs/PRs (merged+open): 3 new\nWarnings:\nGitHub: rate limited"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFetchSummary_ZeroFetchedNoErrors(t *testing.T) {
	result := FetchResult{
		TotalFetched: 0,
	}
	got := FormatFetchSummary(result)
	want := "Found 0 MRs/PRs (merged+open), none to add."
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
	cfg := config.Config{AuthorIDs: map[string]string{"gitlab:42": "UDEVB0001"}}
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
	// Plain keys are report names; a code-hosting display name or username equal to one must not bind.
	cfg := config.Config{AuthorIDs: map[string]string{"dana lee": "UDANA0001", "dlee": "UDANA0001",
		"gitlab:7": "UREAL0007", "github:9": "UREAL0009"}}
	now := time.Now()
	changes := []fetched{
		{cc: domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/a", Title: "a", Author: "Dana Lee", State: "open"}, host: "gitlab", accountID: 100, reportedAt: now},
		{cc: domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/b", Title: "b", Author: "dlee", State: "open"}, host: "gitlab", accountID: 101, reportedAt: now},
		{cc: domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/c", Title: "c", Author: "Anyone", State: "open"}, host: "gitlab", accountID: 7, reportedAt: now},
		{cc: domain.CodeChange{Source: "github", SourceRef: "https://git.example/pr/d", Title: "d", Author: "Dana Lee", State: "open"}, host: "github", accountID: 7, reportedAt: now},
		{cc: domain.CodeChange{Source: "github", SourceRef: "https://git.example/pr/e", Title: "e", Author: "dlee", State: "open"}, host: "github", accountID: 9, reportedAt: now},
	}
	var res FetchResult
	if err := importCodeChanges(db, changes, Hooks{ResolveAccount: cfg.AccountAuthorID}, &res); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "", "b": "", "c": "UREAL0007", "d": "", "e": "UREAL0009"}
	for _, f := range changes {
		cc, _, _ := sqlite.GetCodeChangeByRef(db, f.cc.Source, f.cc.SourceRef)
		it, _ := sqlite.GetWorkItemByID(db, cc.WorkItemID)
		if it.AuthorID != want[f.cc.Title] {
			t.Errorf("%s: author_id %q, want %q", f.cc.Title, it.AuthorID, want[f.cc.Title])
		}
	}
}
