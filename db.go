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
	`
	_, err = db.Exec(schema)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func InsertWorkItem(db *sql.DB, item WorkItem) error {
	_, err := db.Exec(
		`INSERT INTO work_items (description, author, source, source_ref, category, status, ticket_ids, reported_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		item.Description, item.Author, item.Source, item.SourceRef,
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
		`INSERT INTO work_items (description, author, source, source_ref, category, status, ticket_ids, reported_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	inserted := 0
	for _, item := range items {
		_, err := stmt.Exec(
			item.Description, item.Author, item.Source, item.SourceRef,
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
		`SELECT id, description, author, source, source_ref, category, status, ticket_ids, reported_at, created_at
		 FROM work_items WHERE reported_at >= ? AND reported_at < ? ORDER BY category, author, reported_at`,
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
			&item.ID, &item.Description, &item.Author, &item.Source,
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

func GetPendingSlackItemsByAuthorAndDateRange(db *sql.DB, author string, from, to time.Time) ([]WorkItem, error) {
	rows, err := db.Query(
		`SELECT id, description, author, source, source_ref, category, status, ticket_ids, reported_at, created_at
		 FROM work_items
		 WHERE author = ? AND source = 'slack' AND reported_at >= ? AND reported_at < ?
		   AND lower(trim(status)) <> 'done'
		 ORDER BY reported_at DESC`,
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
			&item.ID, &item.Description, &item.Author, &item.Source,
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
