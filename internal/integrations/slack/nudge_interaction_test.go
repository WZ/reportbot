package slackbot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

type nudgeUpdateRecorder struct {
	updateCalls    int
	lastUpdateText string
	ephemeralCalls int
}

func nudgeTestReportedAt() time.Time {
	monday, _ := ReportWeekRange(Config{Location: time.UTC, MondayCutoffTime: "12:00"}, time.Now().UTC())
	return monday.Add(48 * time.Hour).Truncate(time.Second)
}

type mockSlackRoundTripper func(*http.Request) (*http.Response, error)

func (fn mockSlackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func newMockSlackAPIForNudgeInteractions(t *testing.T, rec *nudgeUpdateRecorder) *slack.Client {
	t.Helper()
	httpClient := &http.Client{
		Transport: mockSlackRoundTripper(func(req *http.Request) (*http.Response, error) {
			form, err := mockSlackRequestForm(req)
			if err != nil {
				return nil, err
			}

			payload := map[string]any{"ok": true}
			switch strings.TrimPrefix(req.URL.Path, "/api/") {
			case "users.info":
				userID := form.Get("user")
				display := userID
				if userID == "U_MEMBER" {
					display = "Pat"
				}
				payload = map[string]any{
					"ok": true,
					"user": map[string]any{
						"id":        userID,
						"name":      strings.ToLower(display),
						"real_name": display + " Real",
						"profile": map[string]any{
							"display_name": display,
						},
					},
				}
			case "chat.update":
				rec.updateCalls++
				rec.lastUpdateText = form.Get("text")
				payload = map[string]any{"ok": true, "channel": "D123", "ts": "123.45", "text": rec.lastUpdateText}
			case "chat.postEphemeral":
				rec.ephemeralCalls++
				payload = map[string]any{"ok": true}
			}

			return mockSlackJSONResponse(req, payload)
		}),
	}

	return slack.New(
		"xoxb-test",
		slack.OptionAPIURL("https://slack.test/api/"),
		slack.OptionHTTPClient(httpClient),
	)
}

func mockSlackRequestForm(req *http.Request) (url.Values, error) {
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

func mockSlackJSONResponse(req *http.Request, payload any) (*http.Response, error) {
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

func TestHandleBlockActions_NudgeDoneUpdatesStatusAndRefreshesMessage(t *testing.T) {
	db := newTestDB(t)
	now := nudgeTestReportedAt()
	if err := InsertWorkItem(db, WorkItem{
		Description: "Close stale deployment work",
		Author:      "Pat",
		AuthorID:    "U_MEMBER",
		Source:      "slack",
		Status:      "in progress",
		ReportedAt:  now,
	}); err != nil {
		t.Fatalf("insert work item: %v", err)
	}
	items, err := GetItemsByDateRange(db, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || len(items) != 1 {
		t.Fatalf("load items err=%v len=%d", err, len(items))
	}

	rec := &nudgeUpdateRecorder{}
	api := newMockSlackAPIForNudgeInteractions(t, rec)
	cfg := Config{Location: time.UTC, MondayCutoffTime: "12:00"}
	cb := slack.InteractionCallback{
		User:      slack.User{ID: "U_MEMBER"},
		Container: slack.Container{ChannelID: "D123", MessageTs: "123.45"},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: actionNudgeDone, Value: fmt.Sprintf("done|U_MEMBER|%d|0", items[0].ID)},
			},
		},
	}

	handleBlockActions(api, db, cfg, cb)

	updated, err := GetWorkItemByID(db, items[0].ID)
	if err != nil {
		t.Fatalf("GetWorkItemByID failed: %v", err)
	}
	if updated.Status != "done" {
		t.Fatalf("expected status done, got %q", updated.Status)
	}
	if rec.updateCalls != 1 {
		t.Fatalf("expected one chat.update call, got %d", rec.updateCalls)
	}
	if !strings.Contains(rec.lastUpdateText, "Updated. No items are currently marked in progress.") {
		t.Fatalf("unexpected updated message text: %q", rec.lastUpdateText)
	}
}

func TestHandleBlockActions_NudgeMoreUpdatesStatus(t *testing.T) {
	db := newTestDB(t)
	now := nudgeTestReportedAt()
	if err := InsertWorkItem(db, WorkItem{
		Description: "Validate build pipeline",
		Author:      "Pat",
		AuthorID:    "U_MEMBER",
		Source:      "slack",
		Status:      "in progress",
		ReportedAt:  now,
	}); err != nil {
		t.Fatalf("insert work item: %v", err)
	}
	items, err := GetItemsByDateRange(db, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("GetItemsByDateRange failed: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected at least one work item in date range")
	}

	rec := &nudgeUpdateRecorder{}
	api := newMockSlackAPIForNudgeInteractions(t, rec)
	cfg := Config{Location: time.UTC, MondayCutoffTime: "12:00"}
	cb := slack.InteractionCallback{
		User:      slack.User{ID: "U_MEMBER"},
		Container: slack.Container{ChannelID: "D123", MessageTs: "123.45"},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{
					ActionID: actionNudgeMore,
					SelectedOption: slack.OptionBlockObject{
						Value: fmt.Sprintf("status|U_MEMBER|%d|in testing|0", items[0].ID),
					},
				},
			},
		},
	}

	handleBlockActions(api, db, cfg, cb)

	updated, err := GetWorkItemByID(db, items[0].ID)
	if err != nil {
		t.Fatalf("GetWorkItemByID failed: %v", err)
	}
	if updated.Status != "in testing" {
		t.Fatalf("expected status in testing, got %q", updated.Status)
	}
	if rec.updateCalls != 1 {
		t.Fatalf("expected one chat.update call, got %d", rec.updateCalls)
	}
}

func TestHandleBlockActions_NudgePageNextRefreshesWithoutMutation(t *testing.T) {
	db := newTestDB(t)
	now := nudgeTestReportedAt()
	for i := 0; i < 11; i++ {
		if err := InsertWorkItem(db, WorkItem{
			Description: fmt.Sprintf("Paginated active item %02d", i+1),
			Author:      "Pat",
			AuthorID:    "U_MEMBER",
			Source:      "slack",
			Status:      "in progress",
			ReportedAt:  now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("insert work item %d: %v", i, err)
		}
	}
	items, err := GetItemsByDateRange(db, now.Add(-time.Hour), now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("GetItemsByDateRange failed: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected at least one work item in date range")
	}

	rec := &nudgeUpdateRecorder{}
	api := newMockSlackAPIForNudgeInteractions(t, rec)
	cfg := Config{Location: time.UTC, MondayCutoffTime: "12:00"}
	cb := slack.InteractionCallback{
		User:      slack.User{ID: "U_MEMBER"},
		Container: slack.Container{ChannelID: "D123", MessageTs: "123.45"},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: actionNudgePageNext, Value: "page|U_MEMBER|1"},
			},
		},
	}

	handleBlockActions(api, db, cfg, cb)

	if rec.updateCalls != 1 {
		t.Fatalf("expected one chat.update call, got %d", rec.updateCalls)
	}
	if !strings.Contains(rec.lastUpdateText, "Showing 11-11 of 11 active items") {
		t.Fatalf("expected second-page summary, got %q", rec.lastUpdateText)
	}
	unchanged, err := GetWorkItemByID(db, items[0].ID)
	if err != nil {
		t.Fatalf("GetWorkItemByID failed: %v", err)
	}
	if unchanged.Status != "in progress" {
		t.Fatalf("expected navigation not to mutate status, got %q", unchanged.Status)
	}
}

func TestHandleBlockActions_NudgeDoneClampsPageAfterLastItemRemoved(t *testing.T) {
	db := newTestDB(t)
	now := nudgeTestReportedAt()
	for i := 0; i < 11; i++ {
		if err := InsertWorkItem(db, WorkItem{
			Description: fmt.Sprintf("Paginated active item %02d", i+1),
			Author:      "Pat",
			AuthorID:    "U_MEMBER",
			Source:      "slack",
			Status:      "in progress",
			ReportedAt:  now.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("insert work item %d: %v", i, err)
		}
	}
	items, err := GetItemsByDateRange(db, now.Add(-time.Hour), now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("GetItemsByDateRange failed: %v", err)
	}

	rec := &nudgeUpdateRecorder{}
	api := newMockSlackAPIForNudgeInteractions(t, rec)
	cfg := Config{Location: time.UTC, MondayCutoffTime: "12:00"}
	lastItem := items[len(items)-1]
	cb := slack.InteractionCallback{
		User:      slack.User{ID: "U_MEMBER"},
		Container: slack.Container{ChannelID: "D123", MessageTs: "123.45"},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: actionNudgeDone, Value: fmt.Sprintf("done|U_MEMBER|%d|1", lastItem.ID)},
			},
		},
	}

	handleBlockActions(api, db, cfg, cb)

	updated, err := GetWorkItemByID(db, lastItem.ID)
	if err != nil {
		t.Fatalf("GetWorkItemByID failed: %v", err)
	}
	if updated.Status != "done" {
		t.Fatalf("expected status done, got %q", updated.Status)
	}
	if rec.updateCalls != 1 {
		t.Fatalf("expected one chat.update call, got %d", rec.updateCalls)
	}
	if !strings.Contains(rec.lastUpdateText, "Showing 1-10 of 10 active items") {
		t.Fatalf("expected page clamp to first page, got %q", rec.lastUpdateText)
	}
}

func TestHandleBlockActions_NudgeUnauthorizedUserCannotUpdate(t *testing.T) {
	db := newTestDB(t)
	now := nudgeTestReportedAt()
	if err := InsertWorkItem(db, WorkItem{
		Description: "Protected item",
		Author:      "Pat",
		AuthorID:    "U_MEMBER",
		Source:      "slack",
		Status:      "in progress",
		ReportedAt:  now,
	}); err != nil {
		t.Fatalf("insert work item: %v", err)
	}
	items, err := GetItemsByDateRange(db, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("GetItemsByDateRange failed: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected at least one work item in date range")
	}

	rec := &nudgeUpdateRecorder{}
	api := newMockSlackAPIForNudgeInteractions(t, rec)
	cfg := Config{Location: time.UTC, MondayCutoffTime: "12:00"}
	cb := slack.InteractionCallback{
		User:      slack.User{ID: "U_OTHER"},
		Container: slack.Container{ChannelID: "D123", MessageTs: "123.45"},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: actionNudgeDone, Value: fmt.Sprintf("done|U_MEMBER|%d|0", items[0].ID)},
			},
		},
	}

	handleBlockActions(api, db, cfg, cb)

	unchanged, err := GetWorkItemByID(db, items[0].ID)
	if err != nil {
		t.Fatalf("GetWorkItemByID failed: %v", err)
	}
	if unchanged.Status != "in progress" {
		t.Fatalf("expected unauthorized action not to mutate status, got %q", unchanged.Status)
	}
	if rec.updateCalls != 0 {
		t.Fatalf("expected no chat.update call, got %d", rec.updateCalls)
	}
	if rec.ephemeralCalls != 1 {
		t.Fatalf("expected one ephemeral denial message, got %d", rec.ephemeralCalls)
	}
}
