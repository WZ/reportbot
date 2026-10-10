package cutover

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/storage/sqlite"
)

const olderReport = `### Product A

#### Engineering

- **Cluster Manager**
  - **Dev A** - Node add orchestration (done)

#### Infrastructure

- **Dev B** - Rack cabling plan (done)
`

const latestReport = `### Product A

#### Release and Support

* **7.6.3**

  * **Dev C** - [1234567] Ship 7.6.3 build (done)

#### Engineering

- **Cluster Manager**
  - **Dev A** - [7654321] Shard rebalance timeout (root cause analysis in progress)
  - Plan outline without an author (in progress)
- **Observability stack (in progress)**
  - **Dev D -** Metrics pipeline (in testing)
- **Dev E** - Category level item next to Sections (blocked)

#### Data

- **Dev G** - Category level item (blocked)

#### Undetermined

- **Dev F** - Unsure where this goes (in progress)
`

func TestParse(t *testing.T) {
	r := Parse(latestReport)
	if len(r.Areas) != 1 || len(r.Areas[0].Categories) != 3 {
		t.Fatalf("unexpected headings: %+v", r.Areas)
	}
	eng := r.Areas[0].Categories[1]
	if eng.Name != "Engineering" || strings.Join(eng.Sections, "|") != "Cluster Manager|Observability stack (in progress)" {
		t.Fatalf("a bold line with nothing but a status suffix is a Section; got %+v", eng)
	}
	if len(r.Lines) != 6 {
		t.Fatalf("expected 6 authored lines (plan outline dropped), got %d: %+v", len(r.Lines), r.Lines)
	}
	l := r.Lines[1]
	if l.Author != "Dev A" || l.Section != "Cluster Manager" || l.TicketIDs != "7654321" ||
		l.Description != "[7654321] Shard rebalance timeout" || l.StatusText != "root cause analysis in progress" {
		t.Fatalf("unexpected line: %+v", l)
	}
	if r.Lines[2].Author != "Dev D" || r.Lines[2].Section != "Observability stack (in progress)" {
		t.Fatalf("'**Author -**' variant: %+v", r.Lines[2])
	}
	if r.Lines[3].Section != "" || r.Lines[3].Category != "Engineering" {
		t.Fatalf("a bold line followed by ' - ' is a Category-level item: %+v", r.Lines[3])
	}
	if !r.Lines[5].Undetermined {
		t.Fatalf("expected Undetermined line: %+v", r.Lines[5])
	}
	if st, note := LineStatus("root cause analysis in progress"); st != domain.StatusInProgress || note == "" {
		t.Fatalf("free text keeps a note: %q %q", st, note)
	}
	if NormalizeDescription("[123] Fix  Thing (done)") != "fix thing" {
		t.Fatal("unexpected normalization")
	}
}

func setupV1(t *testing.T) (string, string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	reports := filepath.Join(dir, "reports")
	os.MkdirAll(reports, 0o755)
	os.WriteFile(filepath.Join(reports, "TEAM_20260911.md"), []byte(olderReport), 0o644)
	os.WriteFile(filepath.Join(reports, "TEAM_20260918.md"), []byte(latestReport), 0o644)

	dbPath := filepath.Join(dir, "v1.db")
	raw, _ := sql.Open("sqlite3", dbPath)
	_, err := raw.Exec(`
		CREATE TABLE work_items (id INTEGER PRIMARY KEY AUTOINCREMENT, description TEXT NOT NULL, author TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'slack', source_ref TEXT DEFAULT '', category TEXT DEFAULT '', status TEXT DEFAULT 'done',
			ticket_ids TEXT DEFAULT '', reported_at DATETIME NOT NULL, created_at DATETIME DEFAULT CURRENT_TIMESTAMP, author_id TEXT DEFAULT '');
		CREATE UNIQUE INDEX idx_work_items_unique_source_ref ON work_items(source, source_ref) WHERE source_ref <> '';
		CREATE TABLE classification_history (id INTEGER PRIMARY KEY, work_item_id INTEGER, section_id TEXT);
		CREATE TABLE classification_corrections (id INTEGER PRIMARY KEY, work_item_id INTEGER);
		INSERT INTO work_items (description, author, source, status, reported_at, category) VALUES
			('[7654321] shard rebalance timeout', 'Dev A', 'slack', 'in progress', '2026-09-15 10:00:00-07:00', 'S1_0'),
			('node add orchestration', 'Dev A', 'slack', 'mr review', '2026-09-08 10:00:00-07:00', ''),
			('something never published', 'Dev B', 'slack', 'done', '2026-09-09 10:00:00-07:00', ''),
			('reported after the latest report', 'Dev B', 'slack', 'in progress', '2026-09-23 10:00:00-07:00', '');
		INSERT INTO work_items (description, author, source, source_ref, status, reported_at) VALUES
			('Rack cabling plan', 'Dev B', 'gitlab', 'https://git.example/mr/9', 'done', '2026-09-10 10:00:00-07:00');
	`)
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, err := sqlite.InitDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return dbPath, filepath.Join(reports, "TEAM_20260918.md"), db
}

func TestRunImportsPromotesAndIsIdempotent(t *testing.T) {
	dbPath, latest, db := setupV1(t)
	loc := time.FixedZone("PDT", -7*3600)
	opts := Options{LatestReport: latest, Location: loc, Now: time.Date(2026, 9, 28, 9, 0, 0, 0, loc),
		AuthorID: config.Config{AuthorIDs: map[string]string{"dev c": "UDEVC", "dev a": "UDEVA", "dev e": "UDEVE"}}.AuthorID}

	sum, err := Run(db, dbPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Sections != 4 || sum.Unmatched != 2 || sum.OpenLines != 5 || sum.ImportedExisting != 1 || sum.ImportedNew != 4 || sum.Live != 1 || sum.CodeChanges != 1 {
		t.Fatalf("unexpected summary:\n%s", sum)
	}

	report := sum.String()
	if !strings.Contains(report, "Dev A -> UDEVA") || !strings.Contains(report, "Dev G -> unmapped") {
		t.Fatalf("expected a binding report:\n%s", report)
	}
	dry, err := Run(db, dbPath, Options{LatestReport: latest, Location: loc, DryRun: true, AuthorID: opts.AuthorID})
	if err != nil || len(dry.Bindings) != len(sum.Bindings) {
		t.Fatalf("--dry-run must include the binding report: %+v err=%v", dry.Bindings, err)
	}

	tax, _ := sqlite.LoadTaxonomy(db)
	var paths []string
	for _, s := range tax.Sections() {
		paths = append(paths, s.Path())
	}
	if strings.Join(paths, "|") != "Product A › Release and Support › 7.6.3|Product A › Engineering › Cluster Manager|Product A › Engineering › Observability stack (in progress)|Product A › Data" {
		t.Fatalf("unexpected seeded Sections: %v", paths)
	}

	// The open line that matches an existing row reuses it, with a person Placement.
	shard, _ := sqlite.GetWorkItemByID(db, 1)
	cm := tax.Sections()[1].ID
	if shard.SectionID != cm || shard.PlacedBy != domain.PlacedByPerson || shard.Status != domain.StatusInProgress ||
		shard.StatusNote != "root cause analysis in progress" || !shard.DoneAt.IsZero() || shard.AuthorID != "UDEVA" {
		t.Fatalf("unexpected matched row: %+v", shard)
	}
	// An older row seen in an older report is promoted as done history in its Section.
	node, _ := sqlite.GetWorkItemByID(db, 2)
	if node.SectionID != cm || node.Status != domain.StatusDone || node.DoneAt.IsZero() || node.StatusNote != "mr review" {
		t.Fatalf("unexpected promoted row: %+v", node)
	}
	// Unmatched rows stay history with no Placement.
	if never, _ := sqlite.GetWorkItemByID(db, 3); never.SectionID != 0 || never.Status != domain.StatusDone {
		t.Fatalf("unexpected unmatched row: %+v", never)
	}
	// Rows after the latest report are queued for the models.
	if live, _ := sqlite.GetWorkItemByID(db, 4); live.FilingState != domain.FilingPending || live.Status != domain.StatusInProgress {
		t.Fatalf("unexpected live row: %+v", live)
	}

	open, _ := sqlite.GetOpenItems(db)
	var openDesc []string
	for _, o := range open {
		openDesc = append(openDesc, o.Description)
	}
	if len(open) != 6 { // 4 imported open lines + matched row + live row
		t.Fatalf("unexpected open items: %v", openDesc)
	}
	byAuthor := map[string]domain.WorkItem{}
	for _, o := range open {
		byAuthor[o.Author] = o
	}
	if g := byAuthor["Dev G"]; g.AuthorID != "" || g.SectionID == 0 || g.Status != domain.StatusBlocked || g.Source != importSource || g.PlacedBy != domain.PlacedByPerson {
		t.Fatalf("unexpected imported Category-level line: %+v", g)
	}
	// A Category with Sections is not fileable, so its own lines wait in Undetermined.
	if e := byAuthor["Dev E"]; e.SectionID != 0 || e.AuthorID != "UDEVE" {
		t.Fatalf("expected Undetermined: %+v", e)
	}

	again, err := Run(db, dbPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Skipped) != 4 {
		t.Fatalf("a second run must skip every step:\n%s", again)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&n)
	if n != 9 {
		t.Fatalf("a second run must not duplicate rows, got %d", n)
	}
}

func TestDropV1RequiresImportAndBackup(t *testing.T) {
	dbPath, latest, db := setupV1(t)
	now := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	if _, err := dropV1(db, dbPath, now); err == nil {
		t.Fatal("dropping before the import must be refused")
	}
	sum, err := Run(db, dbPath, Options{LatestReport: latest, DropV1: true, Now: now, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Dropped || sum.Backup == "" {
		t.Fatalf("expected a backup and a drop:\n%s", sum)
	}
	if fi, err := os.Stat(sum.Backup); err != nil || fi.Size() == 0 {
		t.Fatalf("backup missing: %v", err)
	}
	for _, col := range []string{"category", "source_ref"} {
		if ok, _ := hasColumn(db, "work_items", col); ok {
			t.Fatalf("column %s should be dropped", col)
		}
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('classification_history','classification_corrections')`).Scan(&n)
	if n != 0 {
		t.Fatal("V1 tables should be dropped")
	}
	// The backup still holds the V1 data.
	b, _ := sql.Open("sqlite3", sum.Backup)
	defer b.Close()
	if err := b.QueryRow(`SELECT COUNT(*) FROM classification_history`).Scan(&n); err != nil {
		t.Fatalf("backup should keep V1 tables: %v", err)
	}
	// The bot still works on the dropped schema.
	if _, err := sqlite.InsertWorkItem(db, domain.WorkItem{Description: "after", Author: "A", Source: "slack", Status: domain.StatusDone, ReportedAt: now}); err != nil {
		t.Fatalf("insert after drop: %v", err)
	}
}
