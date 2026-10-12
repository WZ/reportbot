package handlers

import (
	"net/http"
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

func TestGenerate_FallbackFilesBeforePublishingThenProposes(t *testing.T) {
	e := newEnv(t)
	proposed := make(chan struct{})
	e.withHooks(web.PublishHooks{
		Fallback: func(items []domain.WorkItem) int {
			for _, it := range items {
				if _, err := e.db.Exec(`UPDATE work_items SET section_id = ?, placed_by = 'fallback' WHERE id = ?`, e.sec["Ops"], it.ID); err != nil {
					t.Error(err)
				}
			}
			return len(items)
		},
		AfterPublish: func() { close(proposed) },
	})
	status := e.waitJob(e.do(http.MethodPost, "/generate?week="+testWeek, manager, nil).Body.String())
	if !strings.Contains(status, "flash-success") {
		t.Fatalf("with nothing left Undetermined after the fallback, publish proceeds: %s", status)
	}
	if it := e.item("bob"); it.PlacedBy != domain.PlacedByFallback {
		t.Fatalf("the fallback files the item first: %+v", it)
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
