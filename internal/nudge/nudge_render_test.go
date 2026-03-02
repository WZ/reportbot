package nudge

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	sqlitedb "reportbot/internal/storage/sqlite"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func newRenderTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "nudge-test.db")
	db, err := sqlitedb.InitDB(dbPath)
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newNudgeMockSlackAPI(t *testing.T, users map[string]map[string]string) *slack.Client {
	t.Helper()
	httpClient := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			form, err := requestForm(req)
			if err != nil {
				return nil, err
			}

			payload := map[string]any{"ok": true}
			switch strings.TrimPrefix(req.URL.Path, "/api/") {
			case "users.info":
				userID := form.Get("user")
				profile := users[userID]
				if profile == nil {
					payload = map[string]any{"ok": false, "error": "user_not_found"}
					break
				}
				payload = map[string]any{
					"ok": true,
					"user": map[string]any{
						"id":        userID,
						"name":      profile["name"],
						"real_name": profile["real_name"],
						"profile": map[string]any{
							"display_name": profile["display_name"],
						},
					},
				}
			}
			return jsonResponse(req, payload)
		}),
	}
	return slack.New(
		"xoxb-test",
		slack.OptionAPIURL("https://slack.test/api/"),
		slack.OptionHTTPClient(httpClient),
	)
}

func requestForm(req *http.Request) (url.Values, error) {
	values := req.URL.Query()
	if req.Body == nil {
		return values, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return values, nil
	}
	parsed, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	for key, vals := range parsed {
		for _, val := range vals {
			values.Add(key, val)
		}
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return values, nil
}

func jsonResponse(req *http.Request, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

func TestRenderNudgeForUser_GenericOnlyWhenNoActiveItems(t *testing.T) {
	db := newRenderTestDB(t)
	api := newNudgeMockSlackAPI(t, map[string]map[string]string{
		"U_MEMBER": {
			"name":         "member",
			"real_name":    "Member Real",
			"display_name": "Member Display",
		},
	})

	now := time.Date(2026, 3, 3, 9, 0, 0, 0, time.UTC)
	if err := sqlitedb.InsertWorkItem(db, sqlitedb.WorkItem{
		Description: "Already shipped",
		Author:      "Member Display",
		AuthorID:    "U_MEMBER",
		Source:      "slack",
		Status:      "done",
		ReportedAt:  now,
	}); err != nil {
		t.Fatalf("insert work item: %v", err)
	}

	cfg := Config{Location: time.UTC}
	rendered, err := RenderNudgeForUser(api, db, cfg, "U_MEMBER", "C_REPORT", now, 0, false)
	if err != nil {
		t.Fatalf("RenderNudgeForUser returned error: %v", err)
	}

	if !strings.Contains(rendered.Text, "Friendly reminder to report your work items") {
		t.Fatalf("expected generic reminder text, got %q", rendered.Text)
	}
	if strings.Contains(rendered.Text, "still marked") {
		t.Fatalf("did not expect active-item copy in generic nudge: %q", rendered.Text)
	}
	if len(rendered.Blocks) != 1 {
		t.Fatalf("expected one generic block, got %d", len(rendered.Blocks))
	}
}

func TestRenderNudgeForUser_PaginatesAndUsesAllSources(t *testing.T) {
	db := newRenderTestDB(t)
	api := newNudgeMockSlackAPI(t, map[string]map[string]string{
		"U_MEMBER": {
			"name":         "pat",
			"real_name":    "Pat User",
			"display_name": "Pat",
		},
	})
	now := time.Date(2026, 3, 3, 9, 0, 0, 0, time.UTC)

	insert := func(item sqlitedb.WorkItem) {
		t.Helper()
		if err := sqlitedb.InsertWorkItem(db, item); err != nil {
			t.Fatalf("insert work item: %v", err)
		}
	}

	insert(sqlitedb.WorkItem{
		Description: "Slack task",
		Author:      "Pat",
		AuthorID:    "U_MEMBER",
		Source:      "slack",
		Status:      "in progress",
		ReportedAt:  now,
	})
	insert(sqlitedb.WorkItem{
		Description: "GitLab task",
		Author:      "Pat User",
		Source:      "gitlab",
		Status:      "in progress",
		ReportedAt:  now.Add(1 * time.Minute),
	})
	insert(sqlitedb.WorkItem{
		Description: "GitHub task",
		Author:      "Pat",
		Source:      "github",
		Status:      "resolved in session; root cause analysis in progress",
		ReportedAt:  now.Add(2 * time.Minute),
	})
	// Older in-progress row should be hidden by newer done row with same dedupe key.
	insert(sqlitedb.WorkItem{
		Description: "[7001] Duplicate task",
		Author:      "Pat",
		Source:      "gitlab",
		Status:      "in progress",
		TicketIDs:   "7001",
		ReportedAt:  now.Add(3 * time.Minute),
	})
	insert(sqlitedb.WorkItem{
		Description: "[7001] Duplicate task",
		Author:      "Pat",
		Source:      "gitlab",
		Status:      "done",
		TicketIDs:   "7001",
		ReportedAt:  now.Add(4 * time.Minute),
	})
	for i := 0; i < 9; i++ {
		insert(sqlitedb.WorkItem{
			Description: "Paginated task " + string(rune('A'+i)),
			Author:      "Pat",
			Source:      "slack",
			Status:      "in progress",
			ReportedAt:  now.Add(time.Duration(10+i) * time.Minute),
		})
	}

	cfg := Config{Location: time.UTC}
	firstPage, err := RenderNudgeForUser(api, db, cfg, "U_MEMBER", "", now, 0, false)
	if err != nil {
		t.Fatalf("RenderNudgeForUser first page error: %v", err)
	}
	if !strings.Contains(firstPage.Text, "Showing 1-10 of 12 active items") {
		t.Fatalf("expected first-page summary, got %q", firstPage.Text)
	}
	if !strings.Contains(firstPage.Text, "GitLab task") || !strings.Contains(firstPage.Text, "GitHub task") {
		t.Fatalf("expected all-source active items in rendered text, got %q", firstPage.Text)
	}
	if strings.Contains(firstPage.Text, "Duplicate task") {
		t.Fatalf("expected deduped latest done item to be excluded, got %q", firstPage.Text)
	}

	secondPage, err := RenderNudgeForUser(api, db, cfg, "U_MEMBER", "", now, 99, false)
	if err != nil {
		t.Fatalf("RenderNudgeForUser second page error: %v", err)
	}
	if !strings.Contains(secondPage.Text, "Showing 11-12 of 12 active items") {
		t.Fatalf("expected clamped second-page summary, got %q", secondPage.Text)
	}
}

func TestRenderNudgeForUser_UpdatedNoItems(t *testing.T) {
	db := newRenderTestDB(t)
	api := newNudgeMockSlackAPI(t, map[string]map[string]string{
		"U_MEMBER": {
			"name":         "member",
			"real_name":    "Member Real",
			"display_name": "Member Display",
		},
	})
	now := time.Date(2026, 3, 3, 9, 0, 0, 0, time.UTC)
	cfg := Config{Location: time.UTC}

	rendered, err := RenderNudgeForUser(api, db, cfg, "U_MEMBER", "", now, 0, true)
	if err != nil {
		t.Fatalf("RenderNudgeForUser returned error: %v", err)
	}
	if !strings.Contains(rendered.Text, "Updated. No items are currently marked in progress.") {
		t.Fatalf("expected updated no-items text, got %q", rendered.Text)
	}
}
