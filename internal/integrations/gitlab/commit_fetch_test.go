package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchBranchCommitsFiltersAndParses(t *testing.T) {
	from := time.Date(2026, 2, 16, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 7)

	in := from.Add(24 * time.Hour).Format(time.RFC3339)
	out := to.Add(24 * time.Hour).Format(time.RFC3339)
	old := from.Add(-48 * time.Hour).Format(time.RFC3339)

	var gotPath, gotToken, gotRef, gotSince string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		gotRef = r.URL.Query().Get("ref_name")
		gotSince = r.URL.Query().Get("since")
		payload := []map[string]any{
			{"id": "aaa111", "title": "Mantis 0987654: fix unified image build", "message": "Mantis 0987654: fix unified image build",
				"author_name": "Dana Lee", "author_email": "dlee@example.com", "committed_date": in, "authored_date": in,
				"web_url": "https://corp.example/my-group/my-project/-/commit/aaa111"},
			{"id": "bbb222", "title": "Merge branch 'x' into br_8-0", "message": "Merge branch 'x' into br_8-0",
				"author_name": "Dana Lee", "author_email": "dlee@example.com", "committed_date": in, "authored_date": in,
				"web_url": "https://corp.example/my-group/my-project/-/commit/bbb222"},
			{"id": "ccc333", "title": "next week's commit", "message": "next week's commit",
				"author_name": "Dana Lee", "author_email": "dlee@example.com", "committed_date": out, "authored_date": out,
				"web_url": "https://corp.example/my-group/my-project/-/commit/ccc333"},
			{"id": "ddd444", "title": "revert older commit", "message": "Revert \"older\"",
				"author_name": "Dana Lee", "author_email": "dlee@example.com", "committed_date": old, "authored_date": old,
				"web_url": "https://corp.example/my-group/my-project/-/commit/ddd444"},
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()

	src := Source{Name: "corp", URL: server.URL, Token: "glpat-test", Project: "my-group/my-project", Branch: "br_8-0"}
	commits, err := FetchBranchCommits(src, from, to)
	if err != nil {
		t.Fatalf("FetchBranchCommits: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("expected 1 commit after filters, got %d", len(commits))
	}
	if gotPath != "/api/v4/projects/my-group%2Fmy-project/repository/commits" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotToken != "glpat-test" || gotRef != "br_8-0" || gotSince == "" {
		t.Fatalf("unexpected request: token=%q ref=%q since=%q", gotToken, gotRef, gotSince)
	}
	// since must carry the UTC instant: a literal-Z local time would shift the window
	// by the timezone offset and silently drop the week's first hours.
	if gotSince != from.UTC().Format(time.RFC3339) {
		t.Fatalf("since must be UTC, got %q want %q", gotSince, from.UTC().Format(time.RFC3339))
	}
	cm := commits[0]
	if cm.SHA != "aaa111" || cm.Title != "Mantis 0987654: fix unified image build" || cm.AuthorName != "Dana Lee" {
		t.Fatalf("unexpected commit: %+v", cm)
	}
	if cm.WebURL != "https://corp.example/my-group/my-project/-/commit/aaa111" {
		t.Fatalf("unexpected web URL: %q", cm.WebURL)
	}
	if !cm.CommittedAt.Equal(from.Add(24 * time.Hour)) {
		t.Fatalf("committed date: %v", cm.CommittedAt)
	}
}

func TestParseCommitTicketIDs(t *testing.T) {
	cases := []struct {
		name, label, text, want string
	}{
		{"title form", "Mantis", "Mantis 0987654: fix unified image build", "0987654"},
		{"dash form", "Mantis", "Mantis-0987654 fix", "0987654"},
		{"hash form", "Mantis", "Mantis #123: fix", "123"},
		{"in message body", "Mantis", "fix image build\n\nMantis 0987654: detail line", "0987654"},
		{"two tickets deduped", "Mantis", "Mantis 0987654 and Mantis 0987654, Mantis 1234567", "0987654,1234567"},
		{"no label", "", "Mantis 0987654: fix", ""},
		{"no ticket", "Mantis", "bump to 7.6.3 build 1459", ""},
		{"case-insensitive label", "Mantis", "mantis 0987654: fix", "0987654"},
	}
	for _, c := range cases {
		if got := ParseCommitTicketIDs(c.text, c.label); got != c.want {
			t.Errorf("%s: ParseCommitTicketIDs(%q, %q) = %q, want %q", c.name, c.text, c.label, got, c.want)
		}
	}
}
