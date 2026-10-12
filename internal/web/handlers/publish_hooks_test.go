package handlers

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/storage/sqlite"
	"reportbot/internal/web"
)

func (e *testEnv) withHooks(h web.PublishHooks) {
	e.handler = NewServer(e.cfg, e.db, func() { e.woke.Add(1) }, h).Handler
}

func (e *testEnv) waitJob(body string) string {
	e.t.Helper()
	m := regexp.MustCompile(`/generate/([^/]+)/status`).FindStringSubmatch(body)
	if m == nil {
		return body
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := e.do(http.MethodGet, "/generate/"+m[1]+"/status", manager, nil).Body.String()
		if !strings.Contains(status, "spinner") || time.Now().After(deadline) {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGenerate_FallbackRunsOnlyAfterConfirmation(t *testing.T) {
	e := newEnv(t)
	proposed := make(chan struct{})
	called := 0
	e.withHooks(web.PublishHooks{
		Fallback: func(items []domain.WorkItem) int {
			called++
			for _, it := range items {
				if _, err := e.db.Exec(`UPDATE work_items SET section_id = ?, placed_by = 'fallback' WHERE id = ?`, e.sec["Ops"], it.ID); err != nil {
					t.Error(err)
				}
			}
			return len(items)
		},
		AfterPublish: func() { close(proposed) },
	})
	body := e.do(http.MethodPost, "/generate?week="+testWeek, manager, nil).Body.String()
	if !strings.Contains(body, "Undetermined") || !strings.Contains(body, "confirm=1") || called != 0 {
		t.Fatalf("the warning comes first and runs no fallback (called=%d): %s", called, body)
	}
	if it := e.item("bob"); it.SectionID != 0 {
		t.Fatalf("cancelling leaves Undetermined items untouched: %+v", it)
	}

	status := e.waitJob(e.do(http.MethodPost, "/generate?week="+testWeek+"&confirm=1", manager, nil).Body.String())
	if !strings.Contains(status, "flash-success") || called != 1 {
		t.Fatalf("after confirming, the fallback runs and the report publishes (called=%d): %s", called, status)
	}
	if it := e.item("bob"); it.PlacedBy != domain.PlacedByFallback {
		t.Fatalf("the fallback files the item before publishing: %+v", it)
	}
	select {
	case <-proposed:
	case <-time.After(2 * time.Second):
		t.Fatal("Structure Proposals are generated after publishing")
	}
}

func TestGenerate_WarnsWhenUndeterminedRemain(t *testing.T) {
	e := newEnv(t)
	e.withHooks(web.PublishHooks{Fallback: func([]domain.WorkItem) int { return 0 }})
	body := e.do(http.MethodPost, "/generate?week="+testWeek, manager, nil).Body.String()
	if !strings.Contains(body, "Undetermined") || !strings.Contains(body, "confirm=1") {
		t.Fatalf("still-Undetermined items need confirmation: %s", body)
	}
	if _, ok, _ := sqlite.GetPublishedReport(e.db, testWeek); ok {
		t.Fatal("nothing is published before confirmation")
	}
}

func TestWebEditsAndDeletesAreRecorded(t *testing.T) {
	e := newEnv(t)
	form := url.Values{"description": {"Fix ingest lag on shard 3"}, "status": {"blocked"}, "section_id": {fmt.Sprint(e.sec["Ingest"])}}
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], ""), alice, form), http.StatusOK)
	edits, err := sqlite.GetWorkItemEdits(e.db, e.items["alice"])
	if err != nil || len(edits) != 1 || edits[0].ActorID != alice.id || edits[0].Via != "web" ||
		edits[0].Before.Description != "Fix ingest lag" || edits[0].After.Status != domain.StatusBlocked {
		t.Fatalf("web edit: %+v err=%v", edits, err)
	}
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], "/delete"), alice, nil), http.StatusOK)
	edits, _ = sqlite.GetWorkItemEdits(e.db, e.items["alice"])
	if len(edits) != 2 || edits[1].Kind != sqlite.EditDelete || edits[1].ActorID != alice.id {
		t.Fatalf("web delete: %+v", edits)
	}
}
