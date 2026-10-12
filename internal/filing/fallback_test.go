package filing

import (
	"strconv"
	"testing"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/storage/sqlite"
)

// askedAgo files an unsure arrival through the decision model, then backdates its Section Choice.
func (e *env) askedAgo(t *testing.T, desc string, ago time.Duration) int64 {
	t.Helper()
	e.pick("upgrade", 0.4)
	id := e.report(t, domain.WorkItem{Description: desc, Author: "Dev A", AuthorID: "U1", Status: domain.StatusInProgress})
	e.drain(t)
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.FilingState != domain.FilingAsking {
		t.Fatalf("setup: expected an unsure item, got %+v", it)
	}
	if _, err := e.db.Exec(`UPDATE model_judgments SET created_at = ? WHERE work_item_id = ?`, time.Now().Add(-ago), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) gatewayCalls() int {
	e.gw.mu.Lock()
	defer e.gw.mu.Unlock()
	return len(e.gw.requests)
}

func TestFallbackFilesUnansweredItemsUnderTheModelsFirstChoiceAfter24Hours(t *testing.T) {
	e := newEnv(t)
	due := e.askedAgo(t, "upgrade path for old nodes", 25*time.Hour)
	fresh := e.askedAgo(t, "upgrade checks", time.Hour)
	answered := e.askedAgo(t, "upgrade docs", 25*time.Hour)
	if err := sqlite.SetPlacement(e.db, answered, sqlite.Placement{SectionID: e.section["infra"], PlacedBy: domain.PlacedByPerson, HistoryNote: sqlite.ReasonButton}); err != nil {
		t.Fatal(err)
	}
	calls := e.gatewayCalls()

	if n := e.queue.Sweep(); n != 0 {
		t.Fatalf("the sweep re-queues nothing here, got %d", n)
	}
	it, _ := sqlite.GetWorkItemByID(e.db, due)
	if it.SectionID != e.section["upgrade"] || it.PlacedBy != domain.PlacedByFallback || it.FilingState != domain.FilingFiled {
		t.Fatalf("an unanswered item goes to the model's #1 Section: %+v", it)
	}
	if f, _ := sqlite.GetWorkItemByID(e.db, fresh); f.SectionID != 0 {
		t.Fatal("an item asked less than 24 hours ago waits for its reporter")
	}
	if a, _ := sqlite.GetWorkItemByID(e.db, answered); a.SectionID != e.section["infra"] || a.PlacedBy != domain.PlacedByPerson {
		t.Fatal("the reporter's choice wins")
	}
	if e.gatewayCalls() != calls {
		t.Fatal("the fallback must not call the decision model")
	}
	if len(e.notes.fallbacks) != 1 || e.notes.fallbacks[0] != itoa(due)+":Upgrade:false" {
		t.Fatalf("the reporter is told once: %v", e.notes.fallbacks)
	}
}

func TestFallbackSkipsARetiredFirstChoice(t *testing.T) {
	e := newEnv(t)
	id := e.askedAgo(t, "upgrade path", 25*time.Hour)
	if err := sqlite.RetireTaxonomyNode(e.db, e.section["upgrade"]); err != nil {
		t.Fatal(err)
	}
	e.queue.Sweep()
	// pick("upgrade", 0.4) ranks infra second.
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.SectionID != e.section["infra"] {
		t.Fatalf("a retired #1 falls through to the next active Section: %+v", it)
	}
}

func TestPublishFallbackFilesEveryUnsureItemWithoutWaiting(t *testing.T) {
	e := newEnv(t)
	id := e.askedAgo(t, "upgrade path", time.Minute)
	items, _ := sqlite.GetOpenItems(e.db)
	calls := e.gatewayCalls()
	if n := FileByFallback(e.db, e.notes, items, true); n != 1 {
		t.Fatalf("publish files every still-unsure item, got %d", n)
	}
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.PlacedBy != domain.PlacedByFallback {
		t.Fatalf("filed by the fallback: %+v", it)
	}
	if e.gatewayCalls() != calls {
		t.Fatal("publishing calls no model")
	}
	if e.notes.fallbacks[0] != itoa(id)+":Upgrade:true" {
		t.Fatalf("the reporter is told it was filed at publish: %v", e.notes.fallbacks)
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }
