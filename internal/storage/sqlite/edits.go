package sqlite

import (
	"database/sql"
	"encoding/json"
	"time"
)

// migrateEditHistory adds the append-only Work Item edit log and the Draft Snapshots.
func migrateEditHistory(tx *sql.Tx) error {
	_, err := tx.Exec(`
	CREATE TABLE work_item_edits (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		work_item_id    INTEGER NOT NULL,
		kind            TEXT NOT NULL,
		before_json     TEXT,
		after_json      TEXT,
		related_item_id INTEGER,
		actor_id        TEXT NOT NULL DEFAULT '',
		via             TEXT NOT NULL DEFAULT '',
		created_at      DATETIME NOT NULL
	);
	CREATE INDEX idx_wie_item ON work_item_edits(work_item_id, id);
	CREATE INDEX idx_wie_date ON work_item_edits(created_at);

	CREATE TABLE draft_snapshots (
		week_start         TEXT PRIMARY KEY,
		team_md            TEXT NOT NULL,
		boss_md            TEXT NOT NULL,
		item_count         INTEGER NOT NULL,
		undetermined_count INTEGER NOT NULL,
		created_at         DATETIME NOT NULL
	);
	`)
	return err
}

// Actor is who changed a Work Item and through which path (a Via constant).
type Actor struct {
	ID  string // Slack user ID; empty for the system
	Via string
}

// Paths a Work Item edit comes through.
const (
	ViaSlack   = "slack"   // slash commands, buttons and modals
	ViaNudge   = "nudge"   // the nudge DM's status buttons
	ViaWeb     = "web"     // the web report editor
	ViaFetch   = "fetch"   // a merged Code Change moving its Work Item to in testing
	ViaCutover = "cutover" // the migrate-v2 import overwriting a V1 row
)

// Kinds of Work Item edit.
const (
	EditFields      = "edit"         // text, Status, Status Note or tickets
	EditStatus      = "status"       // Status only
	EditDelete      = "delete"       // the Work Item was deleted
	EditUpdateMerge = "update_merge" // a confirmed Update replaced the item's text and Status
)

// ItemSnapshot is the editable part of a Work Item before or after an edit.
type ItemSnapshot struct {
	Description string `json:"description"`
	Status      string `json:"status"`
	StatusNote  string `json:"status_note"`
	TicketIDs   string `json:"ticket_ids"`
}

// WorkItemEdit is one recorded change; Before is nil for nothing, After is nil after a delete.
type WorkItemEdit struct {
	ID            int64
	WorkItemID    int64
	Kind          string
	Before, After *ItemSnapshot
	RelatedItemID int64 // the report an Update merge absorbed
	ActorID, Via  string
	CreatedAt     time.Time
}

func snapshotOf(q querier, id int64) (ItemSnapshot, error) {
	var s ItemSnapshot
	var tickets sql.NullString
	err := q.QueryRow(`SELECT description, status, status_note, ticket_ids FROM work_items WHERE id = ?`, id).
		Scan(&s.Description, &s.Status, &s.StatusNote, &tickets)
	s.TicketIDs = tickets.String
	return s, err
}

func snapshotJSON(s *ItemSnapshot) any {
	if s == nil {
		return nil
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// recordEdit appends one edit unless it changed nothing.
func recordEdit(q querier, itemID int64, kind string, before, after *ItemSnapshot, related int64, by Actor) error {
	if before != nil && after != nil && *before == *after {
		return nil
	}
	_, err := q.Exec(
		`INSERT INTO work_item_edits (work_item_id, kind, before_json, after_json, related_item_id, actor_id, via, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		itemID, kind, snapshotJSON(before), snapshotJSON(after), nullID(related), by.ID, by.Via, time.Now())
	return err
}

// SnapshotItemTx reads a Work Item's editable fields inside tx, before a raw-SQL change.
func SnapshotItemTx(tx *sql.Tx, id int64) (ItemSnapshot, error) { return snapshotOf(tx, id) }

// RecordEditTx records a raw-SQL change made inside tx, comparing before with the row now.
func RecordEditTx(tx *sql.Tx, id int64, before ItemSnapshot, by Actor) error {
	after, err := snapshotOf(tx, id)
	if err != nil {
		return err
	}
	return recordEdit(tx, id, EditFields, &before, &after, 0, by)
}

// GetWorkItemEdits returns a Work Item's recorded edits, oldest first.
func GetWorkItemEdits(db *sql.DB, itemID int64) ([]WorkItemEdit, error) {
	rows, err := db.Query(`SELECT id, work_item_id, kind, before_json, after_json, related_item_id, actor_id, via, created_at
		FROM work_item_edits WHERE work_item_id = ? ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkItemEdit
	for rows.Next() {
		var e WorkItemEdit
		var before, after sql.NullString
		var related sql.NullInt64
		if err := rows.Scan(&e.ID, &e.WorkItemID, &e.Kind, &before, &after, &related, &e.ActorID, &e.Via, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.RelatedItemID = related.Int64
		for _, p := range []struct {
			raw sql.NullString
			dst **ItemSnapshot
		}{{before, &e.Before}, {after, &e.After}} {
			if p.raw.Valid {
				var s ItemSnapshot
				if json.Unmarshal([]byte(p.raw.String), &s) == nil {
					*p.dst = &s
				}
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DraftSnapshot is the Weekly Report as it stood at the Friday snapshot, before publishing.
type DraftSnapshot struct {
	WeekStart         string
	TeamMarkdown      string
	BossMarkdown      string
	ItemCount         int
	UndeterminedCount int
	CreatedAt         time.Time
}

// SaveDraftSnapshot stores (or replaces) the week's Draft Snapshot.
func SaveDraftSnapshot(db *sql.DB, d DraftSnapshot) error {
	_, err := db.Exec(
		`INSERT INTO draft_snapshots (week_start, team_md, boss_md, item_count, undetermined_count, created_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(week_start) DO UPDATE SET team_md = excluded.team_md, boss_md = excluded.boss_md,
		     item_count = excluded.item_count, undetermined_count = excluded.undetermined_count, created_at = excluded.created_at`,
		d.WeekStart, d.TeamMarkdown, d.BossMarkdown, d.ItemCount, d.UndeterminedCount, time.Now())
	return err
}

func GetDraftSnapshot(db *sql.DB, weekStart string) (DraftSnapshot, bool, error) {
	d := DraftSnapshot{WeekStart: weekStart}
	err := db.QueryRow(`SELECT team_md, boss_md, item_count, undetermined_count, created_at FROM draft_snapshots WHERE week_start = ?`, weekStart).
		Scan(&d.TeamMarkdown, &d.BossMarkdown, &d.ItemCount, &d.UndeterminedCount, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return d, false, nil
	}
	return d, err == nil, err
}
