package main

import (
	"database/sql"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func InitDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}

	schema := `
	CREATE TABLE IF NOT EXISTS work_items (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		description TEXT NOT NULL,
		author      TEXT NOT NULL,
		source      TEXT NOT NULL DEFAULT 'slack',
		source_ref  TEXT DEFAULT '',
		category    TEXT DEFAULT '',
		status      TEXT DEFAULT 'done',
		ticket_ids  TEXT DEFAULT '',
		reported_at DATETIME NOT NULL,
		created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_work_items_reported_at ON work_items(reported_at);
	CREATE INDEX IF NOT EXISTS idx_work_items_author ON work_items(author);

	CREATE TABLE IF NOT EXISTS classification_history (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		work_item_id     INTEGER NOT NULL,
		section_id       TEXT NOT NULL,
		section_label    TEXT DEFAULT '',
		confidence       REAL NOT NULL,
		normalized_status TEXT DEFAULT '',
		ticket_ids       TEXT DEFAULT '',
		duplicate_of     TEXT DEFAULT '',
		llm_provider     TEXT DEFAULT '',
		llm_model        TEXT DEFAULT '',
		classified_at    DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_ch_work_item ON classification_history(work_item_id);
	CREATE INDEX IF NOT EXISTS idx_ch_date ON classification_history(classified_at);

	CREATE TABLE IF NOT EXISTS classification_corrections (
		id                   INTEGER PRIMARY KEY AUTOINCREMENT,
		work_item_id         INTEGER NOT NULL,
		original_section_id  TEXT NOT NULL,
		original_label       TEXT DEFAULT '',
		corrected_section_id TEXT NOT NULL,
		corrected_label      TEXT DEFAULT '',
		description          TEXT DEFAULT '',
		corrected_by         TEXT DEFAULT '',
		corrected_at         DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_cc_date ON classification_corrections(corrected_at);
	`
	_, err = db.Exec(schema)
	if err != nil {
		return nil, err
	}

	// Migration: add author_id column if missing.
	var colCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('work_items') WHERE name = 'author_id'`).Scan(&colCount)
	if colCount == 0 {
		_, _ = db.Exec(`ALTER TABLE work_items ADD COLUMN author_id TEXT DEFAULT ''`)
	}

	return db, nil
}

func InsertWorkItem(db *sql.DB, item WorkItem) error {
	_, err := db.Exec(
		`INSERT INTO work_items (description, author, author_id, source, source_ref, category, status, ticket_ids, reported_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.Description, item.Author, item.AuthorID, item.Source, item.SourceRef,
		item.Category, item.Status, item.TicketIDs, item.ReportedAt,
	)
	return err
}

func InsertWorkItems(db *sql.DB, items []WorkItem) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO work_items (description, author, author_id, source, source_ref, category, status, ticket_ids, reported_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	inserted := 0
	for _, item := range items {
		_, err := stmt.Exec(
			item.Description, item.Author, item.AuthorID, item.Source, item.SourceRef,
			item.Category, item.Status, item.TicketIDs, item.ReportedAt,
		)
		if err != nil {
			return inserted, err
		}
		inserted++
	}

	return inserted, tx.Commit()
}

func SourceRefExists(db *sql.DB, sourceRef string) (bool, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM work_items WHERE source_ref = ?", sourceRef).Scan(&count)
	return count > 0, err
}

func GetItemsByDateRange(db *sql.DB, from, to time.Time) ([]WorkItem, error) {
	rows, err := db.Query(
		`SELECT id, description, author, author_id, source, source_ref, category, status, ticket_ids, reported_at, created_at
		 FROM work_items WHERE reported_at >= ? AND reported_at < ? ORDER BY category, author, reported_at, id`,
		from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []WorkItem
	for rows.Next() {
		var item WorkItem
		err := rows.Scan(
			&item.ID, &item.Description, &item.Author, &item.AuthorID, &item.Source,
			&item.SourceRef, &item.Category, &item.Status, &item.TicketIDs,
			&item.ReportedAt, &item.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func GetWorkItemByID(db *sql.DB, id int64) (WorkItem, error) {
	var item WorkItem
	err := db.QueryRow(
		`SELECT id, description, author, author_id, source, source_ref, category, status, ticket_ids, reported_at, created_at
		 FROM work_items WHERE id = ?`,
		id,
	).Scan(
		&item.ID, &item.Description, &item.Author, &item.AuthorID, &item.Source,
		&item.SourceRef, &item.Category, &item.Status, &item.TicketIDs,
		&item.ReportedAt, &item.CreatedAt,
	)
	return item, err
}

func UpdateWorkItemTextAndStatus(db *sql.DB, id int64, description, status string) error {
	_, err := db.Exec(
		`UPDATE work_items
		 SET description = ?, status = ?
		 WHERE id = ?`,
		description, status, id,
	)
	return err
}

func DeleteWorkItemByID(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM work_items WHERE id = ?`, id)
	return err
}

func GetPendingSlackItemsByAuthorAndDateRange(db *sql.DB, author string, from, to time.Time) ([]WorkItem, error) {
	rows, err := db.Query(
		`SELECT id, description, author, author_id, source, source_ref, category, status, ticket_ids, reported_at, created_at
		 FROM work_items
		 WHERE author = ? AND source = 'slack' AND reported_at >= ? AND reported_at < ?
		   AND lower(trim(status)) <> 'done'
		 ORDER BY reported_at DESC, id DESC`,
		author, from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []WorkItem
	for rows.Next() {
		var item WorkItem
		err := rows.Scan(
			&item.ID, &item.Description, &item.Author, &item.AuthorID, &item.Source,
			&item.SourceRef, &item.Category, &item.Status, &item.TicketIDs,
			&item.ReportedAt, &item.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func GetSlackItemsByAuthorAndDateRange(db *sql.DB, author string, from, to time.Time) ([]WorkItem, error) {
	rows, err := db.Query(
		`SELECT id, description, author, author_id, source, source_ref, category, status, ticket_ids, reported_at, created_at
		 FROM work_items
		 WHERE author = ? AND source = 'slack' AND reported_at >= ? AND reported_at < ?
		 ORDER BY reported_at DESC, id DESC`,
		author, from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []WorkItem
	for rows.Next() {
		var item WorkItem
		err := rows.Scan(
			&item.ID, &item.Description, &item.Author, &item.AuthorID, &item.Source,
			&item.SourceRef, &item.Category, &item.Status, &item.TicketIDs,
			&item.ReportedAt, &item.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func GetSlackAuthorsByDateRange(db *sql.DB, from, to time.Time) (map[string]bool, error) {
	rows, err := db.Query(
		`SELECT DISTINCT author FROM work_items
		 WHERE reported_at >= ? AND reported_at < ? AND source = 'slack'`,
		from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	authors := make(map[string]bool)
	for rows.Next() {
		var author string
		if err := rows.Scan(&author); err != nil {
			return nil, err
		}
		if author != "" {
			authors[author] = true
		}
	}
	return authors, rows.Err()
}

func UpdateCategories(db *sql.DB, categorized map[int64]string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("UPDATE work_items SET category = ? WHERE id = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for id, category := range categorized {
		if _, err := stmt.Exec(category, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func UpdateTicketIDs(db *sql.DB, ticketMap map[int64]string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("UPDATE work_items SET ticket_ids = ? WHERE id = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for id, tickets := range ticketMap {
		if _, err := stmt.Exec(tickets, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func UpdateWorkItemCategory(db *sql.DB, id int64, category string) error {
	_, err := db.Exec("UPDATE work_items SET category = ? WHERE id = ?", category, id)
	return err
}

// --- Classification History ---

type ClassificationRecord struct {
	ID               int64
	WorkItemID       int64
	SectionID        string
	SectionLabel     string
	Confidence       float64
	NormalizedStatus string
	TicketIDs        string
	DuplicateOf      string
	LLMProvider      string
	LLMModel         string
	ClassifiedAt     time.Time
}

func InsertClassificationHistory(db *sql.DB, records []ClassificationRecord) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO classification_history
		 (work_item_id, section_id, section_label, confidence, normalized_status, ticket_ids, duplicate_of, llm_provider, llm_model)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range records {
		if _, err := stmt.Exec(
			r.WorkItemID, r.SectionID, r.SectionLabel, r.Confidence,
			r.NormalizedStatus, r.TicketIDs, r.DuplicateOf,
			r.LLMProvider, r.LLMModel,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func GetLatestClassification(db *sql.DB, workItemID int64) (ClassificationRecord, error) {
	var r ClassificationRecord
	err := db.QueryRow(
		`SELECT id, work_item_id, section_id, section_label, confidence,
		        normalized_status, ticket_ids, duplicate_of, llm_provider, llm_model, classified_at
		 FROM classification_history
		 WHERE work_item_id = ?
		 ORDER BY classified_at DESC LIMIT 1`,
		workItemID,
	).Scan(
		&r.ID, &r.WorkItemID, &r.SectionID, &r.SectionLabel, &r.Confidence,
		&r.NormalizedStatus, &r.TicketIDs, &r.DuplicateOf,
		&r.LLMProvider, &r.LLMModel, &r.ClassifiedAt,
	)
	return r, err
}

// --- Classification Corrections ---

type ClassificationCorrection struct {
	ID                 int64
	WorkItemID         int64
	OriginalSectionID  string
	OriginalLabel      string
	CorrectedSectionID string
	CorrectedLabel     string
	Description        string
	CorrectedBy        string
	CorrectedAt        time.Time
}

func InsertClassificationCorrection(db *sql.DB, c ClassificationCorrection) error {
	_, err := db.Exec(
		`INSERT INTO classification_corrections
		 (work_item_id, original_section_id, original_label, corrected_section_id, corrected_label, description, corrected_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.WorkItemID, c.OriginalSectionID, c.OriginalLabel,
		c.CorrectedSectionID, c.CorrectedLabel, c.Description, c.CorrectedBy,
	)
	return err
}

func GetRecentCorrections(db *sql.DB, since time.Time, limit int) ([]ClassificationCorrection, error) {
	rows, err := db.Query(
		`SELECT id, work_item_id, original_section_id, original_label,
		        corrected_section_id, corrected_label, description, corrected_by, corrected_at
		 FROM classification_corrections
		 WHERE corrected_at >= ?
		 ORDER BY corrected_at DESC
		 LIMIT ?`,
		since, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ClassificationCorrection
	for rows.Next() {
		var c ClassificationCorrection
		if err := rows.Scan(
			&c.ID, &c.WorkItemID, &c.OriginalSectionID, &c.OriginalLabel,
			&c.CorrectedSectionID, &c.CorrectedLabel, &c.Description,
			&c.CorrectedBy, &c.CorrectedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func GetClassifiedItemsWithSections(db *sql.DB, since time.Time, limit int) ([]historicalItem, error) {
	rows, err := db.Query(
		`SELECT w.description, ch.section_id, ch.section_label
		 FROM classification_history ch
		 JOIN work_items w ON w.id = ch.work_item_id
		 WHERE ch.confidence >= 0.70 AND ch.classified_at >= ?
		 ORDER BY ch.classified_at DESC, ch.id DESC
		 LIMIT ?`,
		since, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []historicalItem
	for rows.Next() {
		var h historicalItem
		if err := rows.Scan(&h.Description, &h.SectionID, &h.SectionLabel); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func CountCorrectionsByPhrase(db *sql.DB, description, correctedSectionID string) (int, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM classification_corrections
		 WHERE LOWER(TRIM(description)) = LOWER(TRIM(?))
		   AND corrected_section_id = ?`,
		description, correctedSectionID,
	).Scan(&count)
	return count, err
}

// --- Classification Stats ---

type ClassificationStats struct {
	TotalClassifications int
	TotalCorrections     int
	AvgConfidence        float64
	BucketBelow50        int
	Bucket50to70         int
	Bucket70to90         int
	Bucket90Plus         int
}

func GetClassificationStats(db *sql.DB, since time.Time) (ClassificationStats, error) {
	var s ClassificationStats
	err := db.QueryRow(
		`SELECT COUNT(*), COALESCE(AVG(confidence), 0),
		        COALESCE(SUM(CASE WHEN confidence < 0.50 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN confidence >= 0.50 AND confidence < 0.70 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN confidence >= 0.70 AND confidence < 0.90 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN confidence >= 0.90 THEN 1 ELSE 0 END), 0)
		 FROM classification_history WHERE classified_at >= ?`,
		since,
	).Scan(&s.TotalClassifications, &s.AvgConfidence,
		&s.BucketBelow50, &s.Bucket50to70, &s.Bucket70to90, &s.Bucket90Plus)
	if err != nil {
		return s, err
	}

	err = db.QueryRow(
		`SELECT COUNT(*) FROM classification_corrections WHERE corrected_at >= ?`,
		since,
	).Scan(&s.TotalCorrections)
	return s, err
}

type SectionCorrectionStat struct {
	OriginalSectionID string
	OriginalLabel     string
	CorrectionCount   int
}

func GetCorrectionsBySection(db *sql.DB, since time.Time) ([]SectionCorrectionStat, error) {
	rows, err := db.Query(
		`SELECT original_section_id, COALESCE(MAX(original_label), ''), COUNT(*) as cnt
		 FROM classification_corrections
		 WHERE corrected_at >= ?
		 GROUP BY original_section_id
		 ORDER BY cnt DESC
		 LIMIT 10`,
		since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SectionCorrectionStat
	for rows.Next() {
		var s SectionCorrectionStat
		if err := rows.Scan(&s.OriginalSectionID, &s.OriginalLabel, &s.CorrectionCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type WeeklyTrend struct {
	WeekStart       string
	Classifications int
	Corrections     int
	AvgConfidence   float64
}

func GetWeeklyClassificationTrend(db *sql.DB, since time.Time) ([]WeeklyTrend, error) {
	rows, err := db.Query(
		`SELECT
		    strftime('%Y-%m-%d', classified_at, 'weekday 0', '-6 days') as week_start,
		    COUNT(*) as classifications,
		    COALESCE(AVG(confidence), 0) as avg_confidence
		 FROM classification_history
		 WHERE classified_at >= ?
		 GROUP BY week_start
		 ORDER BY week_start DESC`,
		since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var trends []WeeklyTrend
	for rows.Next() {
		var t WeeklyTrend
		if err := rows.Scan(&t.WeekStart, &t.Classifications, &t.AvgConfidence); err != nil {
			return nil, err
		}
		trends = append(trends, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load correction counts per week.
	corrRows, err := db.Query(
		`SELECT
		    strftime('%Y-%m-%d', corrected_at, 'weekday 0', '-6 days') as week_start,
		    COUNT(*) as corrections
		 FROM classification_corrections
		 WHERE corrected_at >= ?
		 GROUP BY week_start`,
		since,
	)
	if err != nil {
		return trends, nil // non-fatal
	}
	defer corrRows.Close()

	corrMap := make(map[string]int)
	for corrRows.Next() {
		var ws string
		var cnt int
		if err := corrRows.Scan(&ws, &cnt); err != nil {
			continue
		}
		corrMap[ws] = cnt
	}
	for i := range trends {
		if cnt, ok := corrMap[trends[i].WeekStart]; ok {
			trends[i].Corrections = cnt
		}
	}
	return trends, nil
}
