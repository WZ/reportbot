package sqlite

import (
	"database/sql"
	"time"

	"reportbot/internal/domain"
)

const codeChangeColumns = `id, source, source_ref, title, author, author_id, ticket_ids, state, merged_at, work_item_id, link_kind, suggested_for`

func scanCodeChange(r rowScanner) (domain.CodeChange, error) {
	var c domain.CodeChange
	var mergedAt sql.NullTime
	var itemID, suggested sql.NullInt64
	err := r.Scan(&c.ID, &c.Source, &c.SourceRef, &c.Title, &c.Author, &c.AuthorID, &c.TicketIDs, &c.State,
		&mergedAt, &itemID, &c.LinkKind, &suggested)
	c.MergedAt, c.WorkItemID, c.SuggestedFor = mergedAt.Time, itemID.Int64, suggested.Int64
	return c, err
}

// GetCodeChangeByRef returns the tracked Code Change, or ok=false when it is new.
func GetCodeChangeByRef(db *sql.DB, source, ref string) (domain.CodeChange, bool, error) {
	c, err := scanCodeChange(db.QueryRow(`SELECT `+codeChangeColumns+` FROM code_changes WHERE source = ? AND source_ref = ?`, source, ref))
	if err == sql.ErrNoRows {
		return c, false, nil
	}
	return c, err == nil, err
}

func GetCodeChange(db *sql.DB, id int64) (domain.CodeChange, error) {
	return scanCodeChange(db.QueryRow(`SELECT `+codeChangeColumns+` FROM code_changes WHERE id = ?`, id))
}

// GetCodeChangesForItem returns the Code Changes that are, or are linked to, a Work Item.
func GetCodeChangesForItem(db *sql.DB, itemID int64) ([]domain.CodeChange, error) {
	rows, err := db.Query(`SELECT `+codeChangeColumns+` FROM code_changes WHERE work_item_id = ? ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CodeChange
	for rows.Next() {
		c, err := scanCodeChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func insertCodeChange(q querier, c domain.CodeChange) (int64, error) {
	res, err := q.Exec(
		`INSERT INTO code_changes (source, source_ref, title, author, author_id, ticket_ids, state, merged_at, work_item_id, link_kind, suggested_for, fetched_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Source, c.SourceRef, c.Title, c.Author, c.AuthorID, c.TicketIDs, c.State, nullTime(c.MergedAt),
		nullID(c.WorkItemID), c.LinkKind, nullID(c.SuggestedFor), time.Now(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// LinkNewCodeChange records a Code Change linked to an existing Work Item.
func LinkNewCodeChange(db *sql.DB, c domain.CodeChange) (int64, error) {
	c.LinkKind = domain.LinkLinked
	return insertCodeChange(db, c)
}

// InsertCodeChangeWithItem records a Code Change as its own Work Item and queues filing.
func InsertCodeChangeWithItem(db *sql.DB, c domain.CodeChange, item WorkItem, payload string) (int64, int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	item.FilingState = domain.FilingPending
	itemID, err := insertWorkItem(tx, item)
	if err != nil {
		return 0, 0, err
	}
	c.WorkItemID = itemID
	c.LinkKind = domain.LinkOwn
	ccID, err := insertCodeChange(tx, c)
	if err != nil {
		return 0, 0, err
	}
	if _, err := enqueueJob(tx, JobFile, itemID, payload, time.Now()); err != nil {
		return 0, 0, err
	}
	return ccID, itemID, tx.Commit()
}

func UpdateCodeChangeState(db *sql.DB, id int64, state string, mergedAt time.Time) error {
	_, err := db.Exec(`UPDATE code_changes SET state = ?, merged_at = ?, fetched_at = ? WHERE id = ?`, state, nullTime(mergedAt), time.Now(), id)
	return err
}

func SetCodeChangeSuggestion(db *sql.DB, id, suggestedFor int64) error {
	_, err := db.Exec(`UPDATE code_changes SET suggested_for = ?, link_kind = CASE WHEN ? IS NULL THEN link_kind ELSE 'suggested' END WHERE id = ?`,
		nullID(suggestedFor), nullID(suggestedFor), id)
	return err
}

// ConfirmCodeChangeLink links a Code Change to target and deletes the Work Item it had of its own.
func ConfirmCodeChangeLink(db *sql.DB, id, target int64, by Actor) error {
	c, err := GetCodeChange(db, id)
	if err != nil {
		return err
	}
	own := c.WorkItemID
	if _, err := db.Exec(`UPDATE code_changes SET work_item_id = ?, link_kind = 'linked', suggested_for = NULL WHERE id = ?`, target, id); err != nil {
		return err
	}
	if own != 0 && own != target {
		return DeleteWorkItemByID(db, own, by)
	}
	return nil
}

// RejectCodeChangeLink keeps the Code Change as its own Work Item.
func RejectCodeChangeLink(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE code_changes SET link_kind = 'own', suggested_for = NULL WHERE id = ?`, id)
	return err
}
