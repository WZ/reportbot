package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"reportbot/internal/domain"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := InitDB(filepath.Join(t.TempDir(), "reportbot-test.db"))
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedSmallTaxonomy(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	ids, err := SeedTaxonomy(db, []SeedArea{{Name: "Product A", Categories: []SeedCategory{
		{Name: "Release", Sections: []string{"7.6.3", "7.4.7"}},
		{Name: "Infrastructure"},
	}}})
	if err != nil {
		t.Fatalf("SeedTaxonomy: %v", err)
	}
	return ids
}

func TestInitDBUpgradesV1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE work_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT, description TEXT NOT NULL, author TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'slack', source_ref TEXT DEFAULT '', category TEXT DEFAULT '',
			status TEXT DEFAULT 'done', ticket_ids TEXT DEFAULT '', reported_at DATETIME NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO work_items (description, author, category, status, reported_at)
		VALUES ('old item', 'Dev A', 'S1_2', 'mr review', '2026-03-02 10:00:00-08:00');
	`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := InitDB(path)
	if err != nil {
		t.Fatalf("InitDB on a V1 database: %v", err)
	}
	defer db.Close()
	for _, col := range []string{"author_id", "status_note", "done_at", "section_id", "placed_by", "filing_state", "import_key", "category"} {
		if ok, err := columnExists(db, "work_items", col); err != nil || !ok {
			t.Fatalf("expected column %s after migration (err=%v)", col, err)
		}
	}
	item, err := GetWorkItemByID(db, 1)
	if err != nil || item.Description != "old item" || item.SectionID != 0 {
		t.Fatalf("V1 row must survive untouched: %+v err=%v", item, err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrations must be idempotent: %v", err)
	}
}

func TestWeekMembershipAndDoneLifecycle(t *testing.T) {
	db := newTestDB(t)
	loc := time.FixedZone("PDT", -7*3600)
	// Status changes stamp done_at with the current time, so use the current week.
	monday, next := domain.CurrentWeekRangeAt(time.Now().In(loc))

	insert := func(desc, status string, at time.Time) int64 {
		id, err := InsertWorkItem(db, WorkItem{Description: desc, Author: "Dev A", Source: "slack", Status: status, ReportedAt: at, FilingState: domain.FilingFiled})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	openOld := insert("open from last week", domain.StatusInProgress, monday.AddDate(0, 0, -3))
	doneOld := insert("done last week", domain.StatusDone, monday.AddDate(0, 0, -3))
	doneNow := insert("done this week", domain.StatusDone, monday.Add(time.Hour))
	future := insert("next week", domain.StatusInProgress, next.Add(time.Hour))
	// Same instant in UTC: must still count as this week.
	utc := insert("reported in UTC", domain.StatusInProgress, monday.Add(2*time.Hour).UTC())
	// An instant just before next Monday, written in UTC, is still this week.
	edge := insert("edge in UTC", domain.StatusInProgress, next.Add(-time.Minute).UTC())

	ids := func() map[int64]bool {
		items, err := GetWeekItems(db, monday, next)
		if err != nil {
			t.Fatal(err)
		}
		m := map[int64]bool{}
		for _, it := range items {
			m[it.ID] = true
		}
		return m
	}
	got := ids()
	if !got[openOld] || got[doneOld] || !got[doneNow] || got[future] || !got[utc] || !got[edge] {
		t.Fatalf("unexpected week membership: %v", got)
	}

	if err := SetWorkItemStatus(db, openOld, domain.StatusDone); err != nil {
		t.Fatal(err)
	}
	it, _ := GetWorkItemByID(db, openOld)
	if it.DoneAt.IsZero() {
		t.Fatal("done must set done_at")
	}
	if !ids()[openOld] {
		t.Fatal("an item done this week appears once more")
	}
	nextWeek, _ := GetWeekItems(db, next, next.AddDate(0, 0, 7))
	for _, w := range nextWeek {
		if w.ID == openOld {
			t.Fatal("a done item drops off the following week")
		}
	}
	if err := SetWorkItemStatus(db, openOld, domain.StatusInProgress); err != nil {
		t.Fatal(err)
	}
	if it, _ := GetWorkItemByID(db, openOld); !it.DoneAt.IsZero() {
		t.Fatal("leaving done clears done_at")
	}
	if err := SetWorkItemStatus(db, openOld, "mr review"); err == nil {
		t.Fatal("unknown statuses are rejected")
	}
}

func TestApplyFilingHistoryAndCorrections(t *testing.T) {
	db := newTestDB(t)
	ids := seedSmallTaxonomy(t, db)
	s763, infra := ids[SeedKey("Product A", "Release", "7.6.3")], ids[SeedKey("Product A", "Infrastructure", "")]

	newIDs, err := InsertWorkItemsForFiling(db, []WorkItem{{Description: "rack power", Author: "Dev A", Source: "slack", ReportedAt: time.Now()}}, `{"kind":"arrival"}`)
	if err != nil {
		t.Fatal(err)
	}
	id := newIDs[0]
	if it, _ := GetWorkItemByID(db, id); it.FilingState != domain.FilingPending || it.Status != "" {
		t.Fatalf("new items wait for filing: %+v", it)
	}
	job, err := ClaimJob(db, time.Now())
	if err != nil || job == nil || job.WorkItemID != id || job.Attempts != 1 {
		t.Fatalf("expected the filing job: %+v err=%v", job, err)
	}

	p := 0.42
	err = ApplyFiling(db, id, FilingOutcome{
		Placement: Placement{PlacedBy: domain.PlacedByModel, Confidence: &p, LLMSectionID: s763,
			FilingState: domain.FilingAsking, Reason: domain.ReasonLowConfidence, HistoryNote: ReasonArrival},
		Status: domain.StatusBlocked, StatusNote: "waiting on vendor", TicketIDs: "1234567",
		Judgments: []Judgment{{Kind: "section_check", AnswerJSON: `{"noul":0.42}`, Model: "m"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	it, _ := GetWorkItemByID(db, id)
	if it.SectionID != 0 || it.Status != domain.StatusBlocked || it.StatusNote != "waiting on vendor" || it.TicketIDs != "1234567" ||
		it.Confidence == nil || *it.Confidence != 0.42 || it.UndeterminedReason != domain.ReasonLowConfidence {
		t.Fatalf("unexpected filed item: %+v", it)
	}

	// The reporter picks a different Section: that is a correction of the LLM's pick.
	if err := SetPlacement(db, id, Placement{SectionID: infra, PlacedBy: domain.PlacedByPerson, HistoryNote: ReasonButton, ActorID: "U1"}); err != nil {
		t.Fatal(err)
	}
	corr, err := GetRecentCorrections(db, time.Now().Add(-time.Hour), 10)
	if err != nil || len(corr) != 1 || corr[0].FromSection != s763 || corr[0].ToSection != infra {
		t.Fatalf("expected one correction %d->%d, got %+v err=%v", s763, infra, corr, err)
	}
	eventID, _ := LatestPlacementEventID(db, id)
	ev, err := GetPlacementEvent(db, eventID)
	if err != nil || ev.FromPlacedBy != domain.PlacedByModel || ev.ToSection != infra || ev.ActorID != "U1" {
		t.Fatalf("unexpected history event: %+v err=%v", ev, err)
	}
	placed, _ := GetPlacedItems(db, 10)
	if len(placed) != 1 || !placed[0].ByPerson || placed[0].SectionID != infra {
		t.Fatalf("unexpected placed items: %+v", placed)
	}
	stats, err := GetWeekStats(db, time.Now().Add(-24*time.Hour), time.Now().Add(24*time.Hour))
	if err != nil || stats.ItemsThisWeek != 1 || stats.Asked != 1 || stats.FiledByModel != 0 || stats.PersonCorrections[s763] != 1 {
		t.Fatalf("unexpected stats: %+v err=%v", stats, err)
	}
}

func TestJobsRetryKillAndReset(t *testing.T) {
	db := newTestDB(t)
	jobID, _ := EnqueueJob(db, JobFile, 7, "")
	job, _ := ClaimJob(db, time.Now())
	if job == nil || job.ID != jobID {
		t.Fatalf("expected job %d, got %+v", jobID, job)
	}
	if again, _ := ClaimJob(db, time.Now()); again != nil {
		t.Fatal("a running job is not claimed twice")
	}
	if err := RetryJob(db, jobID, "boom", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if early, _ := ClaimJob(db, time.Now()); early != nil {
		t.Fatal("a job is not claimed before run_after")
	}
	if late, _ := ClaimJob(db, time.Now().Add(2*time.Hour)); late == nil || late.Attempts != 2 {
		t.Fatalf("expected second attempt, got %+v", late)
	}
	if n, _ := ResetRunningJobs(db); n != 1 {
		t.Fatalf("expected crash recovery to re-queue 1 job, got %d", n)
	}
	if err := EnqueueReclassify(db); err != nil {
		t.Fatal(err)
	}
	if err := EnqueueReclassify(db); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind = 'reclassify'`).Scan(&n)
	if n != 1 {
		t.Fatalf("re-classification requests coalesce, got %d jobs", n)
	}
}

func TestTaxonomyEditing(t *testing.T) {
	db := newTestDB(t)
	ids := seedSmallTaxonomy(t, db)
	if _, err := SeedTaxonomy(db, nil); err == nil {
		t.Fatal("seeding twice must fail")
	}
	s763, s747 := ids[SeedKey("Product A", "Release", "7.6.3")], ids[SeedKey("Product A", "Release", "7.4.7")]
	release := ids[SeedKey("Product A", "Release", "")]

	tax, _ := LoadTaxonomy(db)
	secs := tax.Sections()
	if len(secs) != 3 || secs[0].Path() != "Product A › Release › 7.6.3" || secs[2].Path() != "Product A › Infrastructure" {
		t.Fatalf("unexpected sections: %+v", secs)
	}
	if _, err := InsertTaxonomyNode(db, s763, domain.LevelSection, "too deep", ""); err == nil {
		t.Fatal("a Section cannot hold Sections")
	}
	newID, err := InsertTaxonomyNode(db, release, domain.LevelSection, "7.8.0", "next release")
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveTaxonomyNode(db, newID, -1); err != nil {
		t.Fatal(err)
	}
	tax, _ = LoadTaxonomy(db)
	if kids := tax.Children(release); kids[1].ID != newID {
		t.Fatalf("expected the new Section moved up one: %+v", kids)
	}
	if err := UpdateTaxonomyNode(db, s747, "7.4.8", "patch"); err != nil {
		t.Fatal(err)
	}

	itemID, _ := InsertWorkItem(db, WorkItem{Description: "x", Author: "A", Source: "slack", Status: domain.StatusInProgress,
		ReportedAt: time.Now(), SectionID: s763, PlacedBy: domain.PlacedByModel, FilingState: domain.FilingFiled})
	moved, err := MergeSections(db, s763, s747, "UMGR")
	if err != nil || moved != 1 {
		t.Fatalf("merge moved %d err=%v", moved, err)
	}
	it, _ := GetWorkItemByID(db, itemID)
	if it.SectionID != s747 || it.PlacedBy != domain.PlacedByPerson {
		t.Fatalf("merge moves items as a person decision: %+v", it)
	}
	tax, _ = LoadTaxonomy(db)
	if _, ok := tax.Section(s763); ok {
		t.Fatal("the merged-away Section is retired")
	}
	if err := RetireTaxonomyNode(db, release); err != nil {
		t.Fatal(err)
	}
	tax, _ = LoadTaxonomy(db)
	if len(tax.Sections()) != 1 {
		t.Fatalf("retiring a Category retires its Sections: %+v", tax.Sections())
	}
	if s, ok := tax.SectionAny(s747); !ok || s.Name != "7.4.8" {
		t.Fatalf("retired Sections still resolve for history: %+v", s)
	}
}

func TestCodeChangesAndUpdates(t *testing.T) {
	db := newTestDB(t)
	existing, _ := InsertWorkItem(db, WorkItem{Description: "shard timeout", Author: "Dev A", Source: "slack",
		Status: domain.StatusInProgress, TicketIDs: "1234567", ReportedAt: time.Now(), FilingState: domain.FilingFiled})

	found, err := FindOpenItemsByTicket(db, "1234567")
	if err != nil || len(found) != 1 || found[0].ID != existing {
		t.Fatalf("exact ticket lookup failed: %+v err=%v", found, err)
	}
	if found, _ := FindOpenItemsByTicket(db, "123456"); len(found) != 0 {
		t.Fatal("ticket lookup must be exact")
	}

	ccID, ownItem, err := InsertCodeChangeWithItem(db, domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/1", Title: "fix timeout",
		Author: "Dev A", State: "open"}, WorkItem{Description: "fix timeout", Author: "Dev A", Source: "gitlab", Status: domain.StatusInProgress, ReportedAt: time.Now()}, `{"kind":"code_change"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := GetCodeChangeByRef(db, "gitlab", "https://git.example/mr/1"); !ok {
		t.Fatal("code change must be tracked by ref")
	}
	if err := SetCodeChangeSuggestion(db, ccID, existing); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmCodeChangeLink(db, ccID, existing); err != nil {
		t.Fatal(err)
	}
	cc, _ := GetCodeChange(db, ccID)
	if cc.WorkItemID != existing || cc.LinkKind != domain.LinkLinked {
		t.Fatalf("expected linked code change: %+v", cc)
	}
	if _, err := GetWorkItemByID(db, ownItem); err != sql.ErrNoRows {
		t.Fatalf("the code change's own Work Item is removed when linked, err=%v", err)
	}

	newID, _ := InsertWorkItem(db, WorkItem{Description: "shard timeout fixed", Author: "Dev A", Source: "slack",
		Status: domain.StatusInTesting, StatusNote: "verifying on lab", TicketIDs: "7654321", ReportedAt: time.Now()})
	merged, err := MergeUpdate(db, existing, newID)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Description != "shard timeout fixed" || merged.Status != domain.StatusInTesting || merged.TicketIDs != "1234567,7654321" {
		t.Fatalf("unexpected merged item: %+v", merged)
	}
	if _, err := GetWorkItemByID(db, newID); err != sql.ErrNoRows {
		t.Fatal("an Update does not become a separate Work Item")
	}
}

func TestPublishedReportReplace(t *testing.T) {
	db := newTestDB(t)
	for _, md := range []string{"first", "second"} {
		if err := SavePublishedReport(db, PublishedReport{WeekStart: "2026-09-07", TeamMarkdown: md, BossMarkdown: md, PublishedBy: "U1"}); err != nil {
			t.Fatal(err)
		}
	}
	r, ok, err := GetPublishedReport(db, "2026-09-07")
	if err != nil || !ok || r.TeamMarkdown != "second" {
		t.Fatalf("re-publishing replaces the week's report: %+v ok=%v err=%v", r, ok, err)
	}
	weeks, _ := ListPublishedWeeks(db)
	if len(weeks) != 1 {
		t.Fatalf("expected one published week, got %v", weeks)
	}
}
