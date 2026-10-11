package cutover

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/filing"
	"reportbot/internal/storage/sqlite"
)

// Migration versions recorded in schema_migrations, one per idempotent step.
const (
	versionSeed      = 101
	versionImport    = 102
	versionPromote   = 103
	versionCodeChg   = 104
	versionDropV1    = 105
	importSource     = "import"
	reportDateLayout = "20060102"
)

type Options struct {
	LatestReport string // path to the latest published team report (.md)
	HistoryDir   string // folder of published reports, NAME_YYYYMMDD.md; defaults to LatestReport's folder
	DropV1       bool   // drop V1 tables and columns, after a fresh backup
	DryRun       bool   // parse and count, write nothing
	// AuthorID maps an author name to a Slack user ID through the author_ids config; "" when unmapped.
	AuthorID func(name string) string
	Now      time.Time
	Location *time.Location
}

// Summary counts what each step did.
type Summary struct {
	Sections         int
	OpenLines        int
	ImportedNew      int
	ImportedExisting int
	Promoted         int
	Unmatched        int
	Live             int
	ClosedOld        int // open in V1 but missing from the latest report: closed as done
	CodeChanges      int
	Statuses         map[string]int // imported and promoted rows by mapped Status
	DryRun           bool
	Backup           string
	Dropped          bool
	Skipped          []string
	Bindings         []Binding
}

// Binding is how one author name of the imported lines was bound, for review before --drop-v1.
type Binding struct {
	Author string
	ID     string // "" when author_ids has no entry
}

func (s Summary) String() string {
	b := &strings.Builder{}
	if s.DryRun {
		b.WriteString("DRY RUN on a copy of the database; nothing was written.\n")
	}
	fmt.Fprintf(b, "sections seeded: %d\nopen lines in latest report: %d (new Work Items: %d, matched existing rows: %d)\n",
		s.Sections, s.OpenLines, s.ImportedNew, s.ImportedExisting)
	fmt.Fprintf(b, "existing rows promoted with a person Placement: %d\nexisting rows kept as history (no Placement): %d\n", s.Promoted, s.Unmatched)
	fmt.Fprintf(b, "rows reported after the latest report, queued for filing: %d\n", s.Live)
	fmt.Fprintf(b, "old open rows closed as done: %d\n", s.ClosedOld)
	fmt.Fprintf(b, "code changes recorded: %d\n", s.CodeChanges)
	b.WriteString("statuses mapped:")
	for _, st := range domain.Statuses {
		fmt.Fprintf(b, " %s=%d", st, s.Statuses[st])
	}
	b.WriteString("\n")
	if s.Backup != "" {
		fmt.Fprintf(b, "backup: %s\n", s.Backup)
	}
	fmt.Fprintf(b, "V1 tables/columns dropped: %v\n", s.Dropped)
	for _, sk := range s.Skipped {
		fmt.Fprintf(b, "skipped: %s\n", sk)
	}
	if len(s.Bindings) > 0 {
		b.WriteString("author bindings (review before --drop-v1):\n")
		for _, bd := range s.Bindings {
			if bd.ID != "" {
				fmt.Fprintf(b, "  %s -> %s\n", bd.Author, bd.ID)
			} else {
				fmt.Fprintf(b, "  %s -> unmapped\n", bd.Author)
			}
		}
	}
	return b.String()
}

type reportFile struct {
	path string
	date time.Time
	rep  Report
}

var reportDateRe = regexp.MustCompile(`_(\d{8})\.md$`)

func loadHistory(dir string, loc *time.Location) ([]reportFile, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, err
	}
	var out []reportFile
	for _, p := range paths {
		m := reportDateRe.FindStringSubmatch(p)
		if m == nil {
			continue
		}
		d, err := time.ParseInLocation(reportDateLayout, m[1], loc)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, reportFile{path: p, date: d, rep: Parse(string(b))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].date.Before(out[j].date) })
	return out, nil
}

func seedAreas(r Report) []sqlite.SeedArea {
	var areas []sqlite.SeedArea
	for _, a := range r.Areas {
		sa := sqlite.SeedArea{Name: a.Name}
		for _, c := range a.Categories {
			sa.Categories = append(sa.Categories, sqlite.SeedCategory{Name: c.Name, Sections: c.Sections})
		}
		areas = append(areas, sa)
	}
	return areas
}

func importKey(l Line) string {
	h := sha1.Sum([]byte(strings.Join([]string{l.Area, l.Category, l.Section, l.Author, NormalizeDescription(l.Description)}, "|")))
	return "import:" + hex.EncodeToString(h[:])
}

// sectionFor resolves a line's Section by name path in the seeded Taxonomy.
func sectionFor(tax domain.Taxonomy, l Line) int64 {
	if l.Undetermined {
		return 0
	}
	for _, s := range tax.Sections() {
		if s.Area == l.Area && s.Category == l.Category && s.Name == l.Section {
			return s.ID
		}
	}
	return 0
}

// ReportAuthors lists the authors of a report's lines, for suggesting author_ids.
func ReportAuthors(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range Parse(string(b)).Lines {
		out = append(out, l.Author)
	}
	return out, nil
}

// Run performs the cut-over steps that have not run yet.
func Run(db *sql.DB, dbPath string, o Options) (Summary, error) {
	var sum Summary
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	latestBytes, err := os.ReadFile(o.LatestReport)
	if err != nil {
		return sum, fmt.Errorf("reading latest report: %w", err)
	}
	latest := Parse(string(latestBytes))
	m := reportDateRe.FindStringSubmatch(o.LatestReport)
	if m == nil {
		return sum, fmt.Errorf("latest report name must end in _YYYYMMDD.md: %s", o.LatestReport)
	}
	latestDate, err := time.ParseInLocation(reportDateLayout, m[1], o.Location)
	if err != nil {
		return sum, err
	}
	latestWeekStart, latestWeekEnd := domain.CurrentWeekRangeAt(latestDate)
	if o.HistoryDir == "" {
		o.HistoryDir = filepath.Dir(o.LatestReport)
	}
	history, err := loadHistory(o.HistoryDir, o.Location)
	if err != nil {
		return sum, err
	}
	for _, a := range latest.Areas {
		for _, c := range a.Categories {
			sum.Sections += max(len(c.Sections), 1)
		}
	}
	var open []Line
	for _, l := range latest.Lines {
		if st, _ := LineStatus(l.StatusText); st != domain.StatusDone {
			open = append(open, l)
		}
	}
	sum.OpenLines = len(open)
	bound := map[string]string{}
	for _, l := range open {
		if _, done := bound[l.Author]; done {
			continue
		}
		bd := Binding{Author: l.Author}
		if o.AuthorID != nil {
			bd.ID = o.AuthorID(l.Author)
		}
		bound[l.Author] = bd.ID
		sum.Bindings = append(sum.Bindings, bd)
	}
	if o.DryRun {
		return dryRun(db, dbPath, o)
	}
	sum.Statuses = map[string]int{}

	step := func(version int, name string, fn func(tx *sql.Tx) error) error {
		done, err := sqlite.MigrationApplied(db, version)
		if err != nil {
			return err
		}
		if done {
			sum.Skipped = append(sum.Skipped, name+" (already done)")
			return nil
		}
		return sqlite.RunOnce(db, version, fn)
	}

	if err := step(versionSeed, "seed taxonomy", func(tx *sql.Tx) error {
		_, err := sqlite.SeedTaxonomyTx(tx, seedAreas(latest))
		return err
	}); err != nil {
		return sum, fmt.Errorf("seed: %w", err)
	}
	tax, err := sqlite.LoadTaxonomy(db)
	if err != nil {
		return sum, err
	}

	// Existing rows by normalized description, newest first, excluding imported ones.
	rows, err := existingRows(db)
	if err != nil {
		return sum, err
	}
	byNorm := map[string][]int64{}
	for _, r := range rows {
		if !r.imported {
			byNorm[r.norm] = append(byNorm[r.norm], r.id)
		}
	}
	claimed := map[int64]bool{}

	if err := step(versionImport, "import open lines", func(tx *sql.Tx) error {
		for _, l := range open {
			status, note := LineStatus(l.StatusText)
			sum.Statuses[status]++
			section := sectionFor(tax, l)
			placedBy := domain.PlacedByPerson
			if section == 0 {
				placedBy = ""
			}
			// The author's Slack user ID decides who may edit the item; names are never trusted.
			authorID := bound[l.Author]
			if ids := byNorm[NormalizeDescription(l.Description)]; len(ids) > 0 && !claimed[ids[0]] {
				id := ids[0]
				claimed[id] = true
				if _, err := tx.Exec(`UPDATE work_items SET status = ?, status_note = ?, ticket_ids = CASE WHEN ? <> '' THEN ? ELSE ticket_ids END,
					author_id = CASE WHEN COALESCE(author_id, '') = '' THEN ? ELSE author_id END,
					done_at = NULL, section_id = ?, placed_by = ?, filing_state = 'filed', undetermined_reason = '', import_key = ?, updated_at = ? WHERE id = ?`,
					status, note, l.TicketIDs, l.TicketIDs, authorID, nullID(section), placedBy, importKey(l), o.Now, id); err != nil {
					return err
				}
				if err := recordImport(tx, id, section, placedBy, o.Now); err != nil {
					return err
				}
				sum.ImportedExisting++
				continue
			}
			res, err := tx.Exec(`INSERT INTO work_items (description, author, author_id, source, status, status_note, ticket_ids, reported_at, created_at,
					section_id, placed_by, filing_state, undetermined_reason, import_key, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'filed', '', ?, ?)
				ON CONFLICT DO NOTHING`,
				l.Description, l.Author, authorID, importSource, status, note, l.TicketIDs, o.Now, o.Now,
				nullID(section), placedBy, importKey(l), o.Now)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				id, _ := res.LastInsertId()
				if err := recordImport(tx, id, section, placedBy, o.Now); err != nil {
					return err
				}
				sum.ImportedNew++
			}
		}
		return nil
	}); err != nil {
		return sum, fmt.Errorf("import: %w", err)
	}

	if err := step(versionPromote, "promote existing rows", func(tx *sql.Tx) error {
		rows, err := existingRows(tx)
		if err != nil {
			return err
		}
		// The latest report each normalized line appears in decides its Section.
		type seenLine struct {
			line Line
			date time.Time
		}
		lastSeen := map[string]seenLine{}
		for _, h := range history {
			for _, l := range h.rep.Lines {
				lastSeen[NormalizeDescription(l.Description)] = seenLine{l, h.date}
			}
		}
		for _, r := range rows {
			if r.imported {
				continue
			}
			status, note := LineStatus(r.status)
			if !r.reportedAt.Before(latestWeekEnd) {
				// Reported after the latest published report: still live, so the models file it.
				if _, err := tx.Exec(`UPDATE work_items SET status = ?, status_note = ?, done_at = CASE WHEN ? = 'done' THEN reported_at END,
						filing_state = 'pending', updated_at = ? WHERE id = ?`, status, note, status, o.Now, r.id); err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO jobs (kind, work_item_id, payload, state, attempts, run_after, created_at, updated_at)
						VALUES ('file', ?, ?, 'queued', 0, ?, ?, ?)`, r.id, filing.Payload{Kind: filing.KindRetry}.JSON(), o.Now, o.Now, o.Now); err != nil {
					return err
				}
				sum.Live++
				sum.Statuses[status]++
				continue
			}
			// Off the latest report's open lines: done as of the last report showing it.
			seen, ok := lastSeen[r.norm]
			doneAt := r.reportedAt
			section := int64(0)
			if ok {
				if seen.date.After(doneAt) {
					doneAt = seen.date
				}
				section = sectionFor(tax, seen.line)
			}
			placedBy := ""
			if section != 0 {
				placedBy = domain.PlacedByPerson
				sum.Promoted++
			} else {
				sum.Unmatched++
			}
			if status != domain.StatusDone {
				// Open in V1 but not an open line of the latest report: close it before that week,
				// so it never shows on a V2 Weekly Report; the V1 wording stays as the note.
				status, note = domain.StatusDone, strings.TrimSpace(r.status)
				if !ok || !seen.date.Equal(latestDate) {
					doneAt = latestWeekStart.Add(-time.Second)
				}
				sum.ClosedOld++
			}
			doneAtValue := doneAt
			sum.Statuses[status]++
			if _, err := tx.Exec(`UPDATE work_items SET status = ?, status_note = ?, done_at = ?, section_id = ?, placed_by = ?,
					filing_state = 'filed', undetermined_reason = '', updated_at = ? WHERE id = ?`,
				status, note, doneAtValue, nullID(section), placedBy, o.Now, r.id); err != nil {
				return err
			}
			if err := recordImport(tx, r.id, section, placedBy, o.Now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return sum, fmt.Errorf("promote: %w", err)
	}

	if err := step(versionCodeChg, "record code changes", func(tx *sql.Tx) error {
		has, err := hasColumn(tx, "work_items", "source_ref")
		if err != nil || !has {
			return err
		}
		res, err := tx.Exec(`INSERT OR IGNORE INTO code_changes (source, source_ref, title, author, author_id, ticket_ids, state, work_item_id, link_kind, fetched_at)
			SELECT source, source_ref, description, author, COALESCE(author_id, ''), COALESCE(ticket_ids, ''),
				CASE WHEN status = 'done' THEN 'merged' ELSE 'open' END, id, 'own', ?
			FROM work_items WHERE source IN ('gitlab','github') AND source_ref <> ''`, o.Now)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		sum.CodeChanges = int(n)
		return nil
	}); err != nil {
		return sum, fmt.Errorf("code changes: %w", err)
	}

	if o.DropV1 {
		backup, err := dropV1(db, dbPath, o.Now)
		sum.Backup = backup
		if err != nil {
			return sum, fmt.Errorf("drop V1: %w", err)
		}
		sum.Dropped = true
	}
	return sum, nil
}

func recordImport(tx *sql.Tx, id, section int64, placedBy string, now time.Time) error {
	if section == 0 {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO placement_history (work_item_id, to_section, placed_by, reason, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, section, placedBy, sqlite.ReasonImport, now)
	return err
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

type existingRow struct {
	id         int64
	norm       string
	status     string
	reportedAt time.Time
	imported   bool
}

func existingRows(q interface {
	Query(string, ...any) (*sql.Rows, error)
}) ([]existingRow, error) {
	rs, err := q.Query(`SELECT id, description, status, reported_at, import_key IS NOT NULL OR source = 'import' FROM work_items ORDER BY reported_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []existingRow
	for rs.Next() {
		var r existingRow
		var desc string
		if err := rs.Scan(&r.id, &desc, &r.status, &r.reportedAt, &r.imported); err != nil {
			return nil, err
		}
		r.norm = NormalizeDescription(desc)
		out = append(out, r)
	}
	return out, rs.Err()
}

func hasColumn(q interface {
	QueryRow(string, ...any) *sql.Row
}, table, column string) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	return n > 0, err
}

// dropV1 writes a timestamped backup with VACUUM INTO and drops V1 tables and columns
// only after the backup exists and is non-empty.
func dropV1(db *sql.DB, dbPath string, now time.Time) (string, error) {
	for _, v := range []int{versionSeed, versionImport, versionPromote, versionCodeChg} {
		ok, err := sqlite.MigrationApplied(db, v)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("refusing to drop: import step %d has not run", v)
		}
	}
	if done, err := sqlite.MigrationApplied(db, versionDropV1); err != nil || done {
		return "", err
	}
	backup := fmt.Sprintf("%s.%s.backup", dbPath, now.Format("20060102-150405"))
	if _, err := os.Stat(backup); err == nil {
		return "", fmt.Errorf("refusing to overwrite existing backup %s", backup)
	}
	if _, err := db.Exec(`VACUUM INTO ?`, backup); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	if fi, err := os.Stat(backup); err != nil || fi.Size() == 0 {
		return backup, fmt.Errorf("refusing to drop: backup %s missing or empty", backup)
	}
	return backup, sqlite.RunOnce(db, versionDropV1, func(tx *sql.Tx) error {
		stmts := []string{
			`DROP INDEX IF EXISTS idx_work_items_unique_source_ref`,
			`DROP TABLE IF EXISTS classification_history`,
			`DROP TABLE IF EXISTS classification_corrections`,
		}
		for _, col := range []string{"source_ref", "category"} {
			if ok, err := hasColumn(tx, "work_items", col); err != nil {
				return err
			} else if ok {
				stmts = append(stmts, `ALTER TABLE work_items DROP COLUMN `+col)
			}
		}
		for _, s := range stmts {
			if _, err := tx.Exec(s); err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		return nil
	})
}

// dryRun runs every import step on a temporary copy of the database and reports its counts.
func dryRun(db *sql.DB, dbPath string, o Options) (Summary, error) {
	dir, err := os.MkdirTemp("", "migrate-v2-dry-run")
	if err != nil {
		return Summary{}, err
	}
	defer os.RemoveAll(dir)
	copyPath := filepath.Join(dir, "copy.db")
	if _, err := db.Exec(`VACUUM INTO ?`, copyPath); err != nil {
		return Summary{}, fmt.Errorf("copying the database: %w", err)
	}
	cp, err := sqlite.InitDB(copyPath)
	if err != nil {
		return Summary{}, err
	}
	defer cp.Close()
	o.DryRun, o.DropV1 = false, false
	sum, err := Run(cp, copyPath, o)
	sum.DryRun = true
	return sum, err
}
