package slackbot

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

func newMockSlackAPIWithUsers(t *testing.T) *slack.Client {
	t.Helper()

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
		case "users.list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"members": []map[string]any{
					{
						"id":        "U_BOB",
						"name":      "bob",
						"real_name": "Bob Real",
						"profile": map[string]any{
							"display_name": "Bob Display",
						},
					},
				},
			})
		case "chat.postEphemeral":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(server.Close)

	return slack.New("xoxb-test", slack.OptionAPIURL(server.URL+"/api/"))
}

func newMockSlackAPIWithManagerNotify(t *testing.T) (*slack.Client, *int, *string) {
	t.Helper()

	managerMsgCalls := 0
	lastManagerMsg := ""

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/")
		switch path {
		case "users.info":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"user": map[string]any{
					"id":        "U_MEMBER",
					"name":      "member",
					"real_name": "Member Real",
					"profile": map[string]any{
						"display_name": "Member Display",
					},
				},
			})
		case "conversations.open":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"channel": map[string]any{
					"id": "D_MANAGER",
				},
			})
		case "chat.postMessage":
			_ = r.ParseForm()
			managerMsgCalls++
			lastManagerMsg = r.Form.Get("text")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "D_MANAGER", "ts": "1.23"})
		case "chat.postEphemeral":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(server.Close)

	return slack.New("xoxb-test", slack.OptionAPIURL(server.URL+"/api/")), &managerMsgCalls, &lastManagerMsg
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

func TestFunctional_HandleReport_RejectsUnresolvedDelegatedAuthor(t *testing.T) {
	db := newTestDB(t)
	api, postCalls := newMockSlackAPI(t)

	cfg := Config{
		Location:        time.UTC,
		ManagerSlackIDs: []string{"U_MANAGER"},
		TeamMembers:     []string{"Alice Real", "Bob Real"},
	}
	cmd := slack.SlashCommand{
		Command:   "/report",
		Text:      "{Charlie} Ship release checklist (done)",
		UserID:    "U_MANAGER",
		UserName:  "manager",
		ChannelID: "C123",
	}

	handleReport(api, db, cfg, cmd)

	from := time.Now().UTC().Add(-1 * time.Hour)
	to := time.Now().UTC().Add(1 * time.Hour)
	items, err := GetItemsByDateRange(db, from, to)
	if err != nil {
		t.Fatalf("GetItemsByDateRange failed: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no inserted items for unresolved delegated author, got %d", len(items))
	}
	if *postCalls == 0 {
		t.Fatal("expected chat.postEphemeral to be called for validation feedback")
	}
}

func TestFunctional_HandleReport_RejectsAmbiguousDelegatedAuthor(t *testing.T) {
	db := newTestDB(t)
	api, postCalls := newMockSlackAPI(t)

	cfg := Config{
		Location:        time.UTC,
		ManagerSlackIDs: []string{"U_MANAGER"},
		// Two team members whose names are likely to both fuzzily match the same delegated input.
		TeamMembers: []string{"Alice Real", "Alicia Real"},
	}
	cmd := slack.SlashCommand{
		Command: "/report",
		// Delegated author name intended to ambiguously match multiple team members.
		Text:      "{Ali} Ship release checklist (done)",
		UserID:    "U_MANAGER",
		UserName:  "manager",
		ChannelID: "C123",
	}

	handleReport(api, db, cfg, cmd)

	from := time.Now().UTC().Add(-1 * time.Hour)
	to := time.Now().UTC().Add(1 * time.Hour)
	items, err := GetItemsByDateRange(db, from, to)
	if err != nil {
		t.Fatalf("GetItemsByDateRange failed: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no inserted items for ambiguous delegated author, got %d", len(items))
	}
	if *postCalls == 0 {
		t.Fatal("expected chat.postEphemeral to be called for ambiguous match validation feedback")
	}
}

func TestFunctional_HandleReport_DelegatedWithSlackIDResolution(t *testing.T) {
	db := newTestDB(t)
	api := newMockSlackAPIWithUsers(t)

	cfg := Config{
		Location:        time.UTC,
		ManagerSlackIDs: []string{"U_MANAGER"},
		TeamMembers:     []string{"Bob Real"},
	}
	cmd := slack.SlashCommand{
		Command:   "/report",
		Text:      "{Bob} Implement feature X (done)",
		UserID:    "U_MANAGER",
		UserName:  "manager",
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
	if items[0].Author != "Bob Real" {
		t.Errorf("expected author 'Bob Real', got %q", items[0].Author)
	}
	if items[0].AuthorID != "U_BOB" {
		t.Errorf("expected AuthorID 'U_BOB' (delegated member's resolved Slack ID), got %q", items[0].AuthorID)
	}
}

func TestFunctional_HandleReport_DelegatedWithoutSlackIDResolution(t *testing.T) {
	db := newTestDB(t)
	// Use mock API that doesn't support users.list, so Slack ID resolution fails
	api, _ := newMockSlackAPI(t)

	cfg := Config{
		Location:        time.UTC,
		ManagerSlackIDs: []string{"U_MANAGER"},
		TeamMembers:     []string{"Charlie Real"},
	}
	cmd := slack.SlashCommand{
		Command:   "/report",
		Text:      "{Charlie} Implement feature Y (done)",
		UserID:    "U_MANAGER",
		UserName:  "manager",
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
	if items[0].Author != "Charlie Real" {
		t.Errorf("expected author 'Charlie Real', got %q", items[0].Author)
	}
	if items[0].AuthorID != "" {
		t.Errorf("expected empty AuthorID when Slack ID resolution fails, got %q (must not be manager's ID)", items[0].AuthorID)
	}
}

func TestFunctional_HandleReport_NotifiesManagersForMemberReport(t *testing.T) {
	db := newTestDB(t)
	api, managerCalls, lastManagerMsg := newMockSlackAPIWithManagerNotify(t)

	cfg := Config{
		Location:        time.UTC,
		ManagerSlackIDs: []string{"U_MANAGER"},
	}
	cmd := slack.SlashCommand{
		Command:   "/report",
		Text:      "Ship release checklist (done)",
		UserID:    "U_MEMBER",
		UserName:  "member-raw",
		ChannelID: "C_TEST",
	}

	handleReport(api, db, cfg, cmd)

	if *managerCalls != 1 {
		t.Fatalf("expected 1 manager notification, got %d", *managerCalls)
	}
	if !strings.Contains(*lastManagerMsg, "New /report from *Member Display*") {
		t.Fatalf("unexpected manager notification text: %q", *lastManagerMsg)
	}
	if !strings.Contains(*lastManagerMsg, "• Ship release checklist (done)") {
		t.Fatalf("manager notification missing reported item: %q", *lastManagerMsg)
	}
}

func TestFunctional_HandleReport_DoesNotNotifyWhenReporterIsManager(t *testing.T) {
	db := newTestDB(t)
	api, managerCalls, _ := newMockSlackAPIWithManagerNotify(t)

	cfg := Config{
		Location:        time.UTC,
		ManagerSlackIDs: []string{"U_MANAGER"},
	}
	cmd := slack.SlashCommand{
		Command:   "/report",
		Text:      "Review roadmap (in progress)",
		UserID:    "U_MANAGER",
		UserName:  "manager-raw",
		ChannelID: "C_TEST",
	}

	handleReport(api, db, cfg, cmd)

	if *managerCalls != 0 {
		t.Fatalf("expected no manager notification when manager reports, got %d", *managerCalls)
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
