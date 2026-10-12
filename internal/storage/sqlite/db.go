package sqlite

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"reportbot/internal/domain"

	_ "github.com/mattn/go-sqlite3"
)

type WorkItem = domain.WorkItem

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

type migration struct {
	version int
	apply   func(tx *sql.Tx) error
}

// migrations run in order at startup; each is recorded in schema_migrations.
// They are additive only: destructive steps live in the cut-over command.
var migrations = []migration{
	{1, migrateBaseWorkItems},
	{2, migrateTaxonomy},
	{3, migratePlacement},
	{4, migrateSupportTables},
	{5, migrateFailureNotified},
	{6, migrateModelSectionColumn},
	{7, migrateLearning},
	{8, migrateEditHistory},
}

func InitDB(path string) (*sql.DB, error) {
	// Immediate transactions take the write lock up front, so busy_timeout applies
	// instead of a read-then-write transaction failing with SQLITE_BUSY.
	dsn := path
	if strings.Contains(dsn, "?") {
		dsn += "&_txlock=immediate"
	} else {
		dsn += "?_txlock=immediate"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("enabling WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("setting busy timeout: %w", err)
	}
	if err := Migrate(db); err != nil {
		return nil, err
	}
	return db, nil
}

func Migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL
	)`); err != nil {
		return err
	}
	for _, m := range migrations {
		if err := RunOnce(db, m.version, m.apply); err != nil {
			return fmt.Errorf("migration %d: %w", m.version, err)
		}
	}
	return nil
}

// RunOnce applies fn in a transaction unless version is already recorded.
func RunOnce(db *sql.DB, version int, fn func(tx *sql.Tx) error) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func MigrationApplied(db *sql.DB, version int) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n)
	return n > 0, err
}

func columnExists(q querier, table, column string) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	return n > 0, err
}

func migrateBaseWorkItems(tx *sql.Tx) error {
	if _, err := tx.Exec(`
	CREATE TABLE IF NOT EXISTS work_items (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		description TEXT NOT NULL,
		author      TEXT NOT NULL,
		author_id   TEXT DEFAULT '',
		source      TEXT NOT NULL DEFAULT 'slack',
		status      TEXT NOT NULL DEFAULT 'in progress',
		ticket_ids  TEXT DEFAULT '',
		reported_at DATETIME NOT NULL,
		created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_work_items_reported_at ON work_items(reported_at);
	CREATE INDEX IF NOT EXISTS idx_work_items_author ON work_items(author);
	`); err != nil {
		return err
	}
	// Databases created before author_id existed.
	ok, err := columnExists(tx, "work_items", "author_id")
	if err != nil || ok {
		return err
	}
	_, err = tx.Exec(`ALTER TABLE work_items ADD COLUMN author_id TEXT DEFAULT ''`)
	return err
}

func migrateTaxonomy(tx *sql.Tx) error {
	_, err := tx.Exec(`
	CREATE TABLE taxonomy_nodes (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		parent_id   INTEGER REFERENCES taxonomy_nodes(id),
		level       TEXT NOT NULL CHECK (level IN ('area','category','section')),
		name        TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		sort_order  INTEGER NOT NULL,
		retired_at  DATETIME,
		created_at  DATETIME NOT NULL,
		updated_at  DATETIME NOT NULL
	);
	CREATE INDEX idx_tax_parent ON taxonomy_nodes(parent_id, sort_order);
	`)
	return err
}

func migratePlacement(tx *sql.Tx) error {
	stmts := []string{
		`ALTER TABLE work_items ADD COLUMN status_note TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE work_items ADD COLUMN done_at DATETIME`,
		`ALTER TABLE work_items ADD COLUMN section_id INTEGER REFERENCES taxonomy_nodes(id)`,
		`ALTER TABLE work_items ADD COLUMN placed_by TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE work_items ADD COLUMN placement_confidence REAL`,
		`ALTER TABLE work_items ADD COLUMN llm_section_id INTEGER`,
		`ALTER TABLE work_items ADD COLUMN filing_state TEXT NOT NULL DEFAULT 'filed'`,
		`ALTER TABLE work_items ADD COLUMN undetermined_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE work_items ADD COLUMN updated_at DATETIME`,
		`ALTER TABLE work_items ADD COLUMN import_key TEXT`,
		`CREATE UNIQUE INDEX idx_work_items_import_key ON work_items(import_key) WHERE import_key IS NOT NULL`,
		`CREATE INDEX idx_work_items_open ON work_items(done_at, section_id)`,
		`CREATE TABLE placement_history (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			work_item_id   INTEGER NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
			from_section   INTEGER,
			from_placed_by TEXT NOT NULL DEFAULT '',
			to_section     INTEGER,
			placed_by      TEXT NOT NULL,
			model_section  INTEGER,
			confidence     REAL,
			reason         TEXT NOT NULL,
			actor_id       TEXT NOT NULL DEFAULT '',
			created_at     DATETIME NOT NULL
		)`,
		`CREATE INDEX idx_ph_item ON placement_history(work_item_id, id)`,
		`CREATE INDEX idx_ph_date ON placement_history(created_at)`,
		`CREATE TABLE model_judgments (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			work_item_id  INTEGER NOT NULL,
			kind          TEXT NOT NULL,
			subject       TEXT NOT NULL DEFAULT '',
			answer_json   TEXT NOT NULL,
			model         TEXT NOT NULL,
			input_tokens  INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			latency_ms    INTEGER NOT NULL DEFAULT 0,
			created_at    DATETIME NOT NULL
		)`,
		`CREATE INDEX idx_mj_item ON model_judgments(work_item_id, kind)`,
		`CREATE INDEX idx_mj_date ON model_judgments(created_at)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

func migrateSupportTables(tx *sql.Tx) error {
	_, err := tx.Exec(`
	CREATE TABLE code_changes (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		source        TEXT NOT NULL,
		source_ref    TEXT NOT NULL,
		title         TEXT NOT NULL,
		author        TEXT NOT NULL,
		author_id     TEXT NOT NULL DEFAULT '',
		ticket_ids    TEXT NOT NULL DEFAULT '',
		state         TEXT NOT NULL,
		merged_at     DATETIME,
		work_item_id  INTEGER REFERENCES work_items(id) ON DELETE SET NULL,
		link_kind     TEXT NOT NULL,
		suggested_for INTEGER,
		fetched_at    DATETIME NOT NULL,
		UNIQUE (source, source_ref)
	);
	CREATE INDEX idx_cc_item ON code_changes(work_item_id);

	CREATE TABLE jobs (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		kind         TEXT NOT NULL,
		work_item_id INTEGER,
		payload      TEXT NOT NULL DEFAULT '{}',
		state        TEXT NOT NULL DEFAULT 'queued',
		attempts     INTEGER NOT NULL DEFAULT 0,
		run_after    DATETIME NOT NULL,
		last_error   TEXT NOT NULL DEFAULT '',
		created_at   DATETIME NOT NULL,
		updated_at   DATETIME NOT NULL
	);
	CREATE INDEX idx_jobs_due ON jobs(state, run_after);
	CREATE INDEX idx_jobs_item ON jobs(work_item_id, state);

	CREATE TABLE published_reports (
		week_start         TEXT PRIMARY KEY,
		team_md            TEXT NOT NULL,
		boss_md            TEXT NOT NULL,
		undetermined_count INTEGER NOT NULL,
		published_by       TEXT NOT NULL,
		published_at       DATETIME NOT NULL
	);
	`)
	return err
}

func migrateFailureNotified(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE work_items ADD COLUMN failure_notified_at DATETIME`)
	return err
}

// The decision model, not an LLM, picks Sections.
func migrateModelSectionColumn(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE work_items RENAME COLUMN llm_section_id TO model_section_id`)
	return err
}
