package sqlite

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"reportbot/internal/domain"
)

const workItemColumns = `id, description, author, author_id, source, status, status_note, ticket_ids,
	reported_at, created_at, done_at, section_id, placed_by, placement_confidence, filing_state, undetermined_reason`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanWorkItem(r rowScanner) (WorkItem, error) {
	var item WorkItem
	var authorID, ticketIDs sql.NullString
	var createdAt, doneAt sql.NullTime
	var sectionID sql.NullInt64
	var confidence sql.NullFloat64
	err := r.Scan(&item.ID, &item.Description, &item.Author, &authorID, &item.Source, &item.Status,
		&item.StatusNote, &ticketIDs, &item.ReportedAt, &createdAt, &doneAt, &sectionID, &item.PlacedBy,
		&confidence, &item.FilingState, &item.UndeterminedReason)
	if err != nil {
		return item, err
	}
	item.AuthorID = authorID.String
	item.TicketIDs = ticketIDs.String
	item.CreatedAt = createdAt.Time
	item.DoneAt = doneAt.Time
	item.SectionID = sectionID.Int64
	if confidence.Valid {
		c := confidence.Float64
		item.Confidence = &c
	}
	return item, nil
}

func queryWorkItems(q querier, query string, args ...any) ([]WorkItem, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []WorkItem
	for rows.Next() {
		item, err := scanWorkItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// doneAtFor returns when an item with this status became done.
func doneAtFor(status string, at time.Time) time.Time {
	if status == domain.StatusDone {
		return at
	}
	return time.Time{}
}

func insertWorkItem(q querier, item WorkItem) (int64, error) {
	if item.FilingState == "" {
		item.FilingState = domain.FilingPending
	}
	if item.DoneAt.IsZero() {
		item.DoneAt = doneAtFor(item.Status, item.ReportedAt)
	}
	res, err := q.Exec(
		`INSERT INTO work_items (description, author, author_id, source, status, status_note, ticket_ids,
			reported_at, created_at, done_at, section_id, placed_by, placement_confidence, filing_state,
			undetermined_reason, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.Description, item.Author, item.AuthorID, item.Source, item.Status, item.StatusNote, item.TicketIDs,
		item.ReportedAt, time.Now(), nullTime(item.DoneAt), nullID(item.SectionID), item.PlacedBy,
		nullFloat(item.Confidence), item.FilingState, item.UndeterminedReason, time.Now(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// InsertWorkItemsForFiling saves Work Items as pending and queues one filing job each,
// in one transaction, so a saved item is never left without a job.
func InsertWorkItemsForFiling(db *sql.DB, items []WorkItem, payload string) ([]int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		item.FilingState = domain.FilingPending
		id, err := insertWorkItem(tx, item)
		if err != nil {
			return nil, err
		}
		if _, err := enqueueJob(tx, JobFile, id, payload, time.Now()); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, tx.Commit()
}

// InsertWorkItem saves an item as given, without queuing filing.
func InsertWorkItem(db *sql.DB, item WorkItem) (int64, error) {
	return insertWorkItem(db, item)
}

func GetWorkItemByID(db *sql.DB, id int64) (WorkItem, error) {
	return scanWorkItem(db.QueryRow(`SELECT `+workItemColumns+` FROM work_items WHERE id = ?`, id))
}

// Stored timestamps carry mixed UTC offsets, so queries compare instants with julianday().

// GetWeekItems returns the Work Items on the Weekly Report for [monday, nextMonday):
// open items reported before the week ends, and items that became done during it.
func GetWeekItems(db *sql.DB, monday, nextMonday time.Time) ([]WorkItem, error) {
	return queryWorkItems(db,
		`SELECT `+workItemColumns+` FROM work_items
		 WHERE julianday(reported_at) < julianday(?)
		   AND (done_at IS NULL OR (julianday(done_at) >= julianday(?) AND julianday(done_at) < julianday(?)))
		 ORDER BY reported_at, id`,
		nextMonday, monday, nextMonday,
	)
}

// GetItemsByDateRange returns items reported in [from, to).
func GetItemsByDateRange(db *sql.DB, from, to time.Time) ([]WorkItem, error) {
	return queryWorkItems(db,
		`SELECT `+workItemColumns+` FROM work_items WHERE julianday(reported_at) >= julianday(?) AND julianday(reported_at) < julianday(?) ORDER BY author, reported_at, id`,
		from, to,
	)
}

func GetOpenItems(db *sql.DB) ([]WorkItem, error) {
	return queryWorkItems(db,
		`SELECT `+workItemColumns+` FROM work_items WHERE done_at IS NULL AND status <> 'done' ORDER BY reported_at DESC, id DESC`,
	)
}

// GetItemsNeedingRetry returns items whose filing failed, or that have been pending
// longer than staleBefore without a live job.
func GetItemsNeedingRetry(db *sql.DB, staleBefore time.Time) ([]WorkItem, error) {
	return queryWorkItems(db,
		`SELECT `+workItemColumns+` FROM work_items w
		 WHERE (w.filing_state = 'failed' OR (w.filing_state = 'pending' AND julianday(w.created_at) < julianday(?)))
		   AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.work_item_id = w.id AND j.state IN ('queued','running'))
		 ORDER BY w.id`,
		staleBefore,
	)
}

var ticketIDRe = regexp.MustCompile(`^[0-9]+$`)

// ValidTicketID reports whether s has the ticket format (digits only).
func ValidTicketID(s string) bool { return ticketIDRe.MatchString(s) }

// FindOpenItemsByTicket returns Open Work Items whose ticket list contains ticket exactly.
// A match auto-links Code Changes, so the ticket is validated and compared with instr(), not LIKE.
func FindOpenItemsByTicket(db *sql.DB, ticket string) ([]WorkItem, error) {
	ticket = strings.TrimSpace(ticket)
	if !ValidTicketID(ticket) {
		return nil, nil
	}
	return queryWorkItems(db,
		`SELECT `+workItemColumns+` FROM work_items
		 WHERE done_at IS NULL AND status <> 'done'
		   AND instr(',' || replace(ticket_ids, ' ', '') || ',', ',' || ? || ',') > 0
		 ORDER BY reported_at DESC, id DESC`,
		ticket,
	)
}

func GetSlackAuthorIDsByDateRange(db *sql.DB, from, to time.Time) (map[string]bool, error) {
	rows, err := db.Query(
		`SELECT DISTINCT author_id FROM work_items
		 WHERE julianday(reported_at) >= julianday(?) AND julianday(reported_at) < julianday(?) AND source = 'slack' AND author_id <> ''`,
		from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	authorIDs := make(map[string]bool)
	for rows.Next() {
		var authorID string
		if err := rows.Scan(&authorID); err != nil {
			return nil, err
		}
		authorIDs[authorID] = true
	}
	return authorIDs, rows.Err()
}

// UpdateWorkItemFields edits text, Status, Status Note and tickets, keeping done_at consistent,
// and records the edit.
func UpdateWorkItemFields(db *sql.DB, id int64, description, status, statusNote, ticketIDs string, by Actor) error {
	return inTx(db, func(tx *sql.Tx) error {
		return updateFields(tx, id, description, status, statusNote, ticketIDs, EditFields, 0, by)
	})
}

func updateFields(q querier, id int64, description, status, statusNote, ticketIDs, kind string, related int64, by Actor) error {
	if !domain.IsStatus(status) {
		return fmt.Errorf("invalid status %q", status)
	}
	before, err := snapshotOf(q, id)
	if err != nil {
		return err
	}
	if _, err := q.Exec(
		`UPDATE work_items
		 SET description = ?, status = ?, status_note = ?, ticket_ids = ?, updated_at = ?,
		     done_at = CASE WHEN ? = 'done' THEN COALESCE(done_at, ?) ELSE NULL END
		 WHERE id = ?`,
		description, status, statusNote, ticketIDs, time.Now(), status, time.Now(), id,
	); err != nil {
		return err
	}
	after := ItemSnapshot{Description: description, Status: status, StatusNote: statusNote, TicketIDs: ticketIDs}
	return recordEdit(q, id, kind, &before, &after, related, by)
}

// SetWorkItemStatus changes Status; done sets done_at, leaving done clears it. The edit is recorded.
func SetWorkItemStatus(db *sql.DB, id int64, status string, by Actor) error {
	if !domain.IsStatus(status) {
		return fmt.Errorf("invalid status %q", status)
	}
	return inTx(db, func(tx *sql.Tx) error {
		before, err := snapshotOf(tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE work_items SET status = ?, updated_at = ?,
			     done_at = CASE WHEN ? = 'done' THEN COALESCE(done_at, ?) ELSE NULL END
			 WHERE id = ?`,
			status, time.Now(), status, time.Now(), id,
		); err != nil {
			return err
		}
		after := before
		after.Status = status
		return recordEdit(tx, id, EditStatus, &before, &after, 0, by)
	})
}

func inTx(db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// SetFilingPending marks an item as waiting for the models and queues a filing job.
func SetFilingPending(db *sql.DB, id int64, payload string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE work_items SET filing_state = 'pending', updated_at = ? WHERE id = ?`, time.Now(), id); err != nil {
		return err
	}
	if _, err := enqueueJob(tx, JobFile, id, payload, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteWorkItemByID deletes a Work Item and records what was deleted.
func DeleteWorkItemByID(db *sql.DB, id int64, by Actor) error {
	return inTx(db, func(tx *sql.Tx) error { return deleteItem(tx, id, by) })
}

func deleteItem(q querier, id int64, by Actor) error {
	before, err := snapshotOf(q, id)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err := removeItemRows(q, id); err != nil {
		return err
	}
	if err == sql.ErrNoRows {
		return nil
	}
	return recordEdit(q, id, EditDelete, &before, nil, 0, by)
}

func removeItemRows(q querier, id int64) error {
	for _, stmt := range []string{
		`DELETE FROM placement_history WHERE work_item_id = ?`,
		`DELETE FROM jobs WHERE work_item_id = ?`,
		`UPDATE code_changes SET work_item_id = NULL WHERE work_item_id = ?`,
		`DELETE FROM work_items WHERE id = ?`,
	} {
		if _, err := q.Exec(stmt, id); err != nil {
			return err
		}
	}
	return nil
}

// ApplyUpdate applies a confirmed Update: the existing item takes the new report's text,
// Status, Status Note and tickets, and the new report row is deleted; one edit is recorded.
func ApplyUpdate(db *sql.DB, existingID, newID int64, by Actor) (WorkItem, error) {
	newItem, err := GetWorkItemByID(db, newID)
	if err != nil {
		return WorkItem{}, err
	}
	existing, err := GetWorkItemByID(db, existingID)
	if err != nil {
		return WorkItem{}, err
	}
	tickets := mergeTickets(existing.TicketIDs, newItem.TicketIDs)
	err = inTx(db, func(tx *sql.Tx) error {
		if err := updateFields(tx, existingID, newItem.Description, newItem.Status, newItem.StatusNote, tickets, EditUpdateMerge, newID, by); err != nil {
			return err
		}
		return removeItemRows(tx, newID)
	})
	if err != nil {
		return WorkItem{}, err
	}
	return GetWorkItemByID(db, existingID)
}

func mergeTickets(a, b string) string {
	seen := map[string]bool{}
	var out []string
	for _, t := range strings.Split(a+","+b, ",") {
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}
