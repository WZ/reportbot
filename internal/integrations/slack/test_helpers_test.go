package slackbot

import (
	"database/sql"
	"path/filepath"
	sqlitedb "reportbot/internal/storage/sqlite"
	"testing"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "slack-test.db")
	db, err := sqlitedb.InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func resetUserCacheForTest(t *testing.T) {
	t.Helper()
	userCache.Lock()
	userCache.users = nil
	userCache.fetchedAt = userCache.fetchedAt.AddDate(-10, 0, 0)
	userCache.Unlock()
	t.Cleanup(func() {
		userCache.Lock()
		userCache.users = nil
		userCache.fetchedAt = userCache.fetchedAt.AddDate(-10, 0, 0)
		userCache.Unlock()
	})
}
