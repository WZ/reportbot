package sqlite

import (
	"database/sql"
	"time"
)

const (
	JobFile       = "file"
	JobReclassify = "reclassify"
)

type Job struct {
	ID         int64
	Kind       string
	WorkItemID int64
	Payload    string
	Attempts   int
}

func enqueueJob(q querier, kind string, itemID int64, payload string, runAfter time.Time) (int64, error) {
	if payload == "" {
		payload = "{}"
	}
	res, err := q.Exec(
		`INSERT INTO jobs (kind, work_item_id, payload, state, attempts, run_after, created_at, updated_at)
		 VALUES (?, ?, ?, 'queued', 0, ?, ?, ?)`,
		kind, nullID(itemID), payload, runAfter, time.Now(), time.Now(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func EnqueueJob(db *sql.DB, kind string, itemID int64, payload string) (int64, error) {
	return enqueueJob(db, kind, itemID, payload, time.Now())
}

// EnqueueReclassify queues one re-classification unless one is already waiting.
func EnqueueReclassify(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind = ? AND state = 'queued'`, JobReclassify).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := enqueueJob(tx, JobReclassify, 0, "{}", time.Now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClaimJob marks the oldest due queued job running and returns it; nil when none is due.
func ClaimJob(db *sql.DB, now time.Time) (*Job, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var j Job
	var itemID sql.NullInt64
	err = tx.QueryRow(
		`SELECT id, kind, work_item_id, payload, attempts FROM jobs
		 WHERE state = 'queued' AND julianday(run_after) <= julianday(?)
		 ORDER BY run_after, id LIMIT 1`, now,
	).Scan(&j.ID, &j.Kind, &itemID, &j.Payload, &j.Attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.WorkItemID = itemID.Int64
	if _, err := tx.Exec(`UPDATE jobs SET state = 'running', attempts = attempts + 1, updated_at = ? WHERE id = ?`, time.Now(), j.ID); err != nil {
		return nil, err
	}
	j.Attempts++
	return &j, tx.Commit()
}

func CompleteJob(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE jobs SET state = 'done', last_error = '', updated_at = ? WHERE id = ?`, time.Now(), id)
	return err
}

// RetryJob puts a failed job back in the queue to run after runAfter.
func RetryJob(db *sql.DB, id int64, errMsg string, runAfter time.Time) error {
	_, err := db.Exec(`UPDATE jobs SET state = 'queued', last_error = ?, run_after = ?, updated_at = ? WHERE id = ?`,
		errMsg, runAfter, time.Now(), id)
	return err
}

func KillJob(db *sql.DB, id int64, errMsg string) error {
	_, err := db.Exec(`UPDATE jobs SET state = 'dead', last_error = ?, updated_at = ? WHERE id = ?`, errMsg, time.Now(), id)
	return err
}

// ResetRunningJobs re-queues jobs left running by a crash; call once at startup.
func ResetRunningJobs(db *sql.DB) (int64, error) {
	res, err := db.Exec(`UPDATE jobs SET state = 'queued', updated_at = ? WHERE state = 'running'`, time.Now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PendingReclassify reports whether a re-classification is queued or running.
func PendingReclassify(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind = ? AND state IN ('queued','running')`, JobReclassify).Scan(&n)
	return n > 0, err
}
