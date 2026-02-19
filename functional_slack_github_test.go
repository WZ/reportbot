package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

type rewriteGitHubTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t *rewriteGitHubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if clone.URL.Host == "api.github.com" {
		clone.URL.Scheme = t.target.Scheme
		clone.URL.Host = t.target.Host
		clone.Host = t.target.Host
	}
	return t.base.RoundTrip(clone)
}

func withMockGitHubAPI(t *testing.T) {
	t.Helper()

	mergedFromRe := regexp.MustCompile(`merged:>=([0-9]{4}-[0-9]{2}-[0-9]{2})`)
	mergedToRe := regexp.MustCompile(`merged:<([0-9]{4}-[0-9]{2}-[0-9]{2})`)
	updatedRe := regexp.MustCompile(`updated:>=([0-9]{4}-[0-9]{2}-[0-9]{2})`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/search/issues") {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Fatalf("expected bearer token auth header, got %q", got)
		}

		q := r.URL.Query().Get("q")
		resp := githubSearchResponse{}

		switch {
		case strings.Contains(q, "is:merged"):
			mFrom := mergedFromRe.FindStringSubmatch(q)
			mTo := mergedToRe.FindStringSubmatch(q)
			if len(mFrom) != 2 || len(mTo) != 2 {
				t.Fatalf("merged query missing inclusive/exclusive date constraints: %q", q)
			}
			start, err := time.Parse("2006-01-02", mFrom[1])
			if err != nil {
				t.Fatalf("parse merged start date: %v", err)
			}
			end, err := time.Parse("2006-01-02", mTo[1])
			if err != nil {
				t.Fatalf("parse merged end date: %v", err)
			}
			closedAt := start.Add(24 * time.Hour).UTC().Format(time.RFC3339)
			// Boundary item at exact "to" should be excluded by [from, to) filtering.
			boundaryClosedAt := end.UTC().Format(time.RFC3339)
			resp.Items = []githubPRItem{
				{
					Title:         "Merged PR from GitHub",
					HTMLURL:       "https://github.com/acme/repo-a/pull/1",
					State:         "closed",
					CreatedAt:     start.Add(-24 * time.Hour).UTC().Format(time.RFC3339),
					UpdatedAt:     closedAt,
					ClosedAt:      closedAt,
					User:          githubUser{Login: "alice"},
					Labels:        []githubLabel{{Name: "backend"}},
					PullRequest:   &githubPRLinks{},
					RepositoryURL: "https://api.github.com/repos/acme/repo-a",
				},
				{
					Title:         "Merged PR on boundary",
					HTMLURL:       "https://github.com/acme/repo-a/pull/99",
					State:         "closed",
					CreatedAt:     end.Add(-24 * time.Hour).UTC().Format(time.RFC3339),
					UpdatedAt:     boundaryClosedAt,
					ClosedAt:      boundaryClosedAt,
					User:          githubUser{Login: "alice"},
					Labels:        []githubLabel{{Name: "boundary"}},
					PullRequest:   &githubPRLinks{},
					RepositoryURL: "https://api.github.com/repos/acme/repo-a",
				},
			}
		case strings.Contains(q, "is:open"):
			m := updatedRe.FindStringSubmatch(q)
			if len(m) != 2 {
				t.Fatalf("open query missing updated constraint: %q", q)
			}
			start, err := time.Parse("2006-01-02", m[1])
			if err != nil {
				t.Fatalf("parse open start date: %v", err)
			}
			updatedAt := start.Add(48 * time.Hour).UTC().Format(time.RFC3339)
			resp.Items = []githubPRItem{
				{
					Title:         "Open PR from GitHub",
					HTMLURL:       "https://github.com/acme/repo-b/pull/2",
					State:         "open",
					CreatedAt:     start.Add(-12 * time.Hour).UTC().Format(time.RFC3339),
					UpdatedAt:     updatedAt,
					User:          githubUser{Login: "bob"},
					Labels:        []githubLabel{{Name: "frontend"}},
					PullRequest:   &githubPRLinks{},
					RepositoryURL: "https://api.github.com/repos/acme/repo-b",
				},
			}
		default:
			t.Fatalf("unexpected search query: %q", q)
		}
		resp.TotalCount = len(resp.Items)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)

	targetURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse mock github URL failed: %v", err)
	}
	orig := http.DefaultTransport
	http.DefaultTransport = &rewriteGitHubTransport{
		target: targetURL,
		base:   orig,
	}
	t.Cleanup(func() {
		http.DefaultTransport = orig
	})
}

func newMockSlackAPI(t *testing.T) (*slack.Client, *int) {
	t.Helper()

	postEphemeralCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/")
		switch path {
		case "users.info":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"user": map[string]any{
					"id":        "U123",
					"name":      "alice",
					"real_name": "Alice Real",
					"profile": map[string]any{
						"display_name": "Alice Display",
					},
				},
			})
		case "chat.postEphemeral":
			postEphemeralCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(server.Close)

	api := slack.New("xoxb-test", slack.OptionAPIURL(server.URL+"/api/"))
	return api, &postEphemeralCalls
}

func TestFunctional_HandleReport_WithMockSlack(t *testing.T) {
	db := newTestDB(t)
	api, postCalls := newMockSlackAPI(t)

	cfg := Config{Location: time.UTC}
	cmd := slack.SlashCommand{
		Command:   "/report",
		Text:      "Implement OAuth refresh (done)",
		UserID:    "U123",
		UserName:  "alice-raw",
		ChannelID: "C123",
	}

	handleReport(api, db, cfg, cmd)

	from := time.Now().UTC().Add(-1 * time.Hour)
	to := time.Now().UTC().Add(1 * time.Hour)
	items, err := GetItemsByDateRange(db, from, to)
	if err != nil {
		t.Fatalf("GetItemsByDateRange failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 inserted item, got %d", len(items))
	}
	if items[0].Author != "Alice Display" {
		t.Fatalf("expected author resolved from Slack users.info, got %q", items[0].Author)
	}
	if *postCalls == 0 {
		t.Fatal("expected chat.postEphemeral to be called")
	}
}

func TestFunctional_FetchAndImportGitHub_EndToEnd(t *testing.T) {
	withMockGitHubAPI(t)
	db := newTestDB(t)

	cfg := Config{
		GitHubToken: "gho-test",
		GitHubOrg:   "acme",
		TeamMembers: []string{"alice", "bob"},
		Location:    time.UTC,
	}

	first, err := FetchAndImportMRs(cfg, db)
	if err != nil {
		t.Fatalf("FetchAndImportMRs first run failed: %v", err)
	}
	if first.TotalFetched != 2 || first.Inserted != 2 {
		t.Fatalf("unexpected first fetch result: %+v", first)
	}

	second, err := FetchAndImportMRs(cfg, db)
	if err != nil {
		t.Fatalf("FetchAndImportMRs second run failed: %v", err)
	}
	if second.Inserted != 0 || second.AlreadyTracked != 2 {
		t.Fatalf("unexpected second fetch result (dedup expected): %+v", second)
	}

	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	items, err := GetItemsByDateRange(db, monday, nextMonday)
	if err != nil {
		t.Fatalf("GetItemsByDateRange failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 imported items in week range, got %d", len(items))
	}
}

func TestFunctional_HandleFetchMRs_WithSlackAndGitHub(t *testing.T) {
	withMockGitHubAPI(t)
	db := newTestDB(t)
	api, postCalls := newMockSlackAPI(t)

	cfg := Config{
		GitHubToken:     "gho-test",
		GitHubOrg:       "acme",
		TeamMembers:     []string{"alice", "bob"},
		ManagerSlackIDs: []string{"U_MANAGER"},
		Location:        time.UTC,
	}
	cmd := slack.SlashCommand{
		Command:   "/fetch",
		UserID:    "U_MANAGER",
		UserName:  "manager",
		ChannelID: "C123",
	}

	handleFetchMRs(api, db, cfg, cmd)

	if *postCalls < 2 {
		t.Fatalf("expected at least two ephemeral posts (start + summary), got %d", *postCalls)
	}

	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	items, err := GetItemsByDateRange(db, monday, nextMonday)
	if err != nil {
		t.Fatalf("GetItemsByDateRange failed: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected imported GitHub items after handleFetchMRs")
	}
}
