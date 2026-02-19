package slackbot

import (
	"database/sql"
	"path/filepath"
	sqlitedb "reportbot/internal/storage/sqlite"
	"testing"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlitedb.InitDB(dbPath)
	if err != nil {
		t.Fatalf("init test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
