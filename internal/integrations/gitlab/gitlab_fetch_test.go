package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFetchMRsFiltersByStateAndWeekRange(t *testing.T) {
	from := time.Date(2026, 2, 16, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 7)

	inMerged := from.Add(24 * time.Hour).Format(time.RFC3339)
	outMerged := to.Add(24 * time.Hour).Format(time.RFC3339)
	inOpenUpdated := from.Add(48 * time.Hour).Format(time.RFC3339)
	outOpenUpdated := from.Add(-24 * time.Hour).Format(time.RFC3339)
	createdAt := from.Add(6 * time.Hour).Format(time.RFC3339)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/v4/groups/my-group/merge_requests") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "glpat-test" {
			t.Fatalf("unexpected PRIVATE-TOKEN header: %q", got)
		}
		if got := r.URL.Query().Get("state"); got != "all" {
			t.Fatalf("unexpected state query: %q", got)
		}

		payload := []map[string]any{
			{
				"title":      "Merged in range",
				"web_url":    "https://gitlab.example.com/group/proj/-/merge_requests/1",
				"merged_at":  inMerged,
				"updated_at": inMerged,
				"created_at": createdAt,
				"state":      "merged",
				"author": map[string]any{
					"username": "alice",
					"name":     "Alice",
				},
				"labels": []string{"backend"},
			},
			{
				"title":      "Merged out of range",
				"web_url":    "https://gitlab.example.com/group/proj/-/merge_requests/2",
				"merged_at":  outMerged,
				"updated_at": outMerged,
				"created_at": createdAt,
				"state":      "merged",
				"author": map[string]any{
					"username": "alice",
					"name":     "Alice",
				},
				"labels": []string{"backend"},
			},
			{
				"title":      "Open in range",
				"web_url":    "https://gitlab.example.com/group/proj/-/merge_requests/3",
				"merged_at":  "",
				"updated_at": inOpenUpdated,
				"created_at": createdAt,
				"state":      "opened",
				"author": map[string]any{
					"username": "bob",
					"name":     "Bob",
				},
				"labels": []string{"frontend"},
			},
			{
				"title":      "Open out of range",
				"web_url":    "https://gitlab.example.com/group/proj/-/merge_requests/4",
				"merged_at":  "",
				"updated_at": outOpenUpdated,
				"created_at": createdAt,
				"state":      "opened",
				"author": map[string]any{
					"username": "carol",
					"name":     "Carol",
				},
				"labels": []string{"ops"},
			},
			{
				"title":      "Closed not merged",
				"web_url":    "https://gitlab.example.com/group/proj/-/merge_requests/5",
				"merged_at":  "",
				"updated_at": inMerged,
				"created_at": createdAt,
				"state":      "closed",
				"author": map[string]any{
					"username": "dave",
					"name":     "Dave",
				},
				"labels": []string{"ops"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()

	cfg := Config{
		GitLabURL:     server.URL,
		GitLabToken:   "glpat-test",
		GitLabGroupID: "my-group",
	}
	mrs, err := FetchMRs(cfg, from, to)
	if err != nil {
		t.Fatalf("FetchMRs failed: %v", err)
	}
	if len(mrs) != 2 {
		t.Fatalf("expected 2 MRs after filtering, got %d", len(mrs))
	}

	titles := []string{mrs[0].Title, mrs[1].Title}
	joined := strings.Join(titles, "|")
	if !strings.Contains(joined, "Merged in range") || !strings.Contains(joined, "Open in range") {
		t.Fatalf("unexpected MR titles: %v", titles)
	}
}

func TestFetchMRsPagination(t *testing.T) {
	from := time.Date(2026, 2, 16, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 7)
	mergedAt := from.Add(24 * time.Hour).Format(time.RFC3339)
	createdAt := from.Add(6 * time.Hour).Format(time.RFC3339)

	pageHits := make(map[int]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pageHits[page]++

		w.Header().Set("Content-Type", "application/json")
		switch page {
		case 1:
			// Exactly 100 results forces fetch loop to request page 2.
			payload := make([]map[string]any, 0, 100)
			for i := 0; i < 100; i++ {
				payload = append(payload, map[string]any{
					"title":      "Closed item",
					"web_url":    "https://gitlab.example.com/group/proj/-/merge_requests/closed-" + strconv.Itoa(i),
					"merged_at":  "",
					"updated_at": mergedAt,
					"created_at": createdAt,
					"state":      "closed",
					"author": map[string]any{
						"username": "x",
						"name":     "X",
					},
					"labels": []string{},
				})
			}
			_ = json.NewEncoder(w).Encode(payload)
		default:
			payload := []map[string]any{
				{
					"title":      "Merged on page 2",
					"web_url":    "https://gitlab.example.com/group/proj/-/merge_requests/200",
					"merged_at":  mergedAt,
					"updated_at": mergedAt,
					"created_at": createdAt,
					"state":      "merged",
					"author": map[string]any{
						"username": "alice",
						"name":     "Alice",
					},
					"labels": []string{},
				},
			}
			_ = json.NewEncoder(w).Encode(payload)
		}
	}))
	defer server.Close()

	cfg := Config{
		GitLabURL:     server.URL,
		GitLabToken:   "glpat-test",
		GitLabGroupID: "my-group",
	}
	mrs, err := FetchMRs(cfg, from, to)
	if err != nil {
		t.Fatalf("FetchMRs failed: %v", err)
	}
	if len(mrs) != 1 {
		t.Fatalf("expected 1 filtered MR, got %d", len(mrs))
	}
	if mrs[0].Title != "Merged on page 2" {
		t.Fatalf("unexpected title on paginated result: %q", mrs[0].Title)
	}
	if pageHits[1] == 0 || pageHits[2] == 0 {
		t.Fatalf("expected both page 1 and page 2 to be fetched, hits=%v", pageHits)
	}
}

func TestExtractProjectPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://gitlab.example.com/group/proj/-/merge_requests/1", "group/proj"},
		{"https://gitlab.example.com/a/b/c/-/merge_requests/2", "a/b/c"},
		{"bad-url", ""},
	}
	for _, tt := range tests {
		got := extractProjectPath(tt.in)
		if got != tt.want {
			t.Fatalf("extractProjectPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
