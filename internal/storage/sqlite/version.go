package sqlite

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
)

// ReportVersion fingerprints Work Items, Placements and Taxonomy. Timestamps have mixed UTC
// offsets: MAX(julianday) orders them (to the ms), MAX(text) catches sub-ms changes.
func ReportVersion(db *sql.DB) (string, error) {
	var raw string
	err := db.QueryRow(`SELECT
		(SELECT COUNT(*) || ':' || COALESCE(MAX(id), 0) || ':' || COALESCE(MAX(julianday(updated_at)), 0) || ':' || COALESCE(MAX(updated_at), '') FROM work_items) || '|' ||
		(SELECT COALESCE(MAX(id), 0) FROM placement_history) || '|' ||
		(SELECT COUNT(*) || ':' || COALESCE(MAX(julianday(updated_at)), 0) || ':' || COALESCE(MAX(updated_at), '') FROM taxonomy_nodes)`).Scan(&raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8]), nil
}
