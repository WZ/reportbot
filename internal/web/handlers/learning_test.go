package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/learning"
	"reportbot/internal/storage/sqlite"
	"reportbot/internal/web"
)

type fakeLoops struct {
	mu       sync.Mutex
	sweep    func(items []domain.WorkItem) int
	proposed chan struct{}
}

func (f *fakeLoops) SweepUndetermined(ctx context.Context, items []domain.WorkItem, timeout time.Duration) int {
	return f.sweep(items)
}

func (f *fakeLoops) ProposeStructure(ctx context.Context) ([]learning.StoredProposal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.proposed != nil {
		close(f.proposed)
		f.proposed = nil
	}
	return nil, nil
}

func (e *testEnv) withLoops(l web.Loops) {
	e.handler = NewServer(e.cfg, e.db, func() { e.woke.Add(1) }, l).Handler
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

func TestGenerate_SweepsUndeterminedBeforePublishing(t *testing.T) {
	e := newEnv(t)
	bobID := e.items["bob"]
	if _, err := e.db.Exec(`UPDATE work_items SET filing_state = 'asking' WHERE id = ?`, bobID); err != nil {
		t.Fatal(err)
	}
	f := &fakeLoops{proposed: make(chan struct{}), sweep: func(items []domain.WorkItem) int {
		for _, it := range items {
			sqlite.PlaceByAssistant(e.db, it.ID, e.sec["Ops"])
		}
		return len(items)
	}}
	proposed := f.proposed
	e.withLoops(f)

	status := e.waitJob(e.do(http.MethodPost, "/generate?week="+testWeek, manager, nil).Body.String())
	if !strings.Contains(status, "flash-success") {
		t.Fatalf("with nothing left Undetermined after the sweep, publish proceeds: %s", status)
	}
	if it := e.item("bob"); it.PlacedBy != domain.PlacedByAssistant {
		t.Fatalf("the sweep files the item first: %+v", it)
	}
	select {
	case <-proposed:
	case <-time.After(2 * time.Second):
		t.Fatal("Structure Proposals are generated after publishing")
	}
}

func TestGenerate_WarnsWhenTheSweepLeavesUndetermined(t *testing.T) {
	e := newEnv(t)
	e.withLoops(&fakeLoops{sweep: func([]domain.WorkItem) int { return 0 }})
	status := e.waitJob(e.do(http.MethodPost, "/generate?week="+testWeek, manager, nil).Body.String())
	if !strings.Contains(status, "Undetermined") || !strings.Contains(status, "confirm=1") {
		t.Fatalf("still-Undetermined items need confirmation: %s", status)
	}
	if _, ok, _ := sqlite.GetPublishedReport(e.db, testWeek); ok {
		t.Fatal("nothing is published before confirmation")
	}
}

func TestTaxonomyLearnedNotes(t *testing.T) {
	e := newEnv(t)
	path := fmt.Sprintf("/taxonomy/nodes/%d/notes", e.sec["Ingest"])
	expectCode(t, e.do(http.MethodPost, path, bob, url.Values{"learned_notes": {"x"}}), http.StatusForbidden)

	e.clearReclassify()
	expectCode(t, e.do(http.MethodPost, path, manager, url.Values{"learned_notes": {"Ingest pipeline lag. Not query tuning."}}), http.StatusSeeOther)
	n := e.node(e.sec["Ingest"])
	if n.LearnedNotes != "Ingest pipeline lag. Not query tuning." || n.Description != "" {
		t.Fatalf("notes saved apart from the description: %+v", n)
	}
	e.expectReclassifyQueued("notes")
	if body := e.do(http.MethodGet, "/taxonomy", manager, nil).Body.String(); !strings.Contains(body, "Ingest pipeline lag") || !strings.Contains(body, `name="learned_notes"`) {
		t.Fatalf("the Taxonomy page shows and edits Learned Notes")
	}
	expectCode(t, e.do(http.MethodPost, path, manager, url.Values{"learned_notes": {strings.Repeat("x", domain.MaxLearnedNotes+1)}}), http.StatusBadRequest)
	expectCode(t, e.do(http.MethodPost, path, manager, url.Values{"learned_notes": {""}}), http.StatusSeeOther)
	if e.node(e.sec["Ingest"]).LearnedNotes != "" {
		t.Fatal("managers can clear Learned Notes")
	}
}
