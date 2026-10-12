package sqlite

import (
	"database/sql"
	"encoding/json"
	"sort"
	"time"

	"reportbot/internal/domain"
)

// migrateStructureProposals stores Structure Proposals and their decisions.
func migrateStructureProposals(tx *sql.Tx) error {
	_, err := tx.Exec(`
	CREATE TABLE structure_proposals (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		kind         TEXT NOT NULL,
		dedupe_key   TEXT NOT NULL,
		payload_json TEXT NOT NULL,
		affected     INTEGER NOT NULL DEFAULT 0,
		state        TEXT NOT NULL DEFAULT 'proposed',
		decided_by   TEXT NOT NULL DEFAULT '',
		decided_at   DATETIME,
		created_at   DATETIME NOT NULL
	);
	CREATE INDEX idx_sp_key ON structure_proposals(dedupe_key, state);
	`)
	return err
}

// OtherPick is a person filing an unsure Work Item outside the decision model's three most
// likely Sections: evidence that a Section may be missing.
type OtherPick struct {
	WorkItemID  int64
	Description string
	Author      string
}

// GetOtherPicks returns person Placements after since that left Undetermined or a Fallback
// Placement for a Section outside the model's top 3, oldest first.
func GetOtherPicks(db *sql.DB, since time.Time) ([]OtherPick, error) {
	rows, err := db.Query(
		`SELECT h.work_item_id, w.description, w.author, h.to_section,
		    COALESCE((SELECT j.answer_json FROM model_judgments j WHERE j.work_item_id = h.work_item_id AND j.kind = 'section_choice'
		              AND julianday(j.created_at) <= julianday(h.created_at) ORDER BY j.id DESC LIMIT 1), '')
		 FROM placement_history h JOIN work_items w ON w.id = h.work_item_id
		 WHERE h.placed_by = ? AND h.reason NOT IN (?, ?, ?)
		   AND h.to_section IS NOT NULL AND (h.from_section IS NULL OR h.from_placed_by = ?)
		   AND julianday(h.created_at) > julianday(?)
		 ORDER BY h.id`,
		domain.PlacedByPerson, ReasonImport, ReasonUndo, ReasonTaxonomyMerge, domain.PlacedByFallback, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OtherPick
	for rows.Next() {
		var p OtherPick
		var chosen int64
		var answer string
		if err := rows.Scan(&p.WorkItemID, &p.Description, &p.Author, &chosen, &answer); err != nil {
			return nil, err
		}
		if answer != "" && !inTop3(answer, chosen) {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func inTop3(answerJSON string, sectionID int64) bool {
	for _, id := range TopSections(answerJSON, 3) {
		if id == sectionID {
			return true
		}
	}
	return false
}

// TopSections reads a stored Section Choice answer and returns its n most likely Section IDs.
func TopSections(answerJSON string, n int) []int64 {
	var a struct {
		Probabilities map[string]float64 `json:"probabilities"`
	}
	if json.Unmarshal([]byte(answerJSON), &a) != nil {
		return nil
	}
	keys := make([]string, 0, len(a.Probabilities))
	for k := range a.Probabilities {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if a.Probabilities[keys[i]] != a.Probabilities[keys[j]] {
			return a.Probabilities[keys[i]] > a.Probabilities[keys[j]]
		}
		return keys[i] < keys[j]
	})
	var out []int64
	for _, k := range keys {
		if id, ok := domain.ParseSectionKey(k); ok && len(out) < n {
			out = append(out, id)
		}
	}
	return out
}

// Structure Proposal states.
const (
	ProposalPending  = "proposed"
	ProposalApproved = "approved"
	ProposalRejected = "rejected"
	ProposalStale    = "stale"   // no longer valid when a manager tried to approve it
	ProposalExpired  = "expired" // nobody decided in time
)

// Proposal is a stored Structure Proposal; Key identifies the same change across weeks.
type Proposal struct {
	ID          int64
	Kind        string
	Key         string
	PayloadJSON string
	Affected    int
	State       string
	DecidedBy   string
}

func InsertProposal(db *sql.DB, p Proposal) (int64, error) {
	res, err := db.Exec(`INSERT INTO structure_proposals (kind, dedupe_key, payload_json, affected, state, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		p.Kind, p.Key, p.PayloadJSON, p.Affected, ProposalPending, time.Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func GetProposal(db *sql.DB, id int64) (Proposal, error) {
	var p Proposal
	err := db.QueryRow(`SELECT id, kind, dedupe_key, payload_json, affected, state, decided_by FROM structure_proposals WHERE id = ?`, id).
		Scan(&p.ID, &p.Kind, &p.Key, &p.PayloadJSON, &p.Affected, &p.State, &p.DecidedBy)
	return p, err
}

// DecideProposal moves a pending proposal to state; it reports false when it was already decided.
func DecideProposal(db *sql.DB, id int64, state, actorID string) (bool, error) {
	res, err := db.Exec(`UPDATE structure_proposals SET state = ?, decided_by = ?, decided_at = ? WHERE id = ? AND state = ?`,
		state, actorID, time.Now(), id, ProposalPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReopenProposal returns an approved proposal to pending when applying it failed.
func ReopenProposal(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE structure_proposals SET state = ?, decided_by = '', decided_at = NULL WHERE id = ?`, ProposalPending, id)
	return err
}

// BlockedProposals are changes not to propose again: pending ones, and ones approved or
// rejected since decidedSince. Stale and expired proposals do not block.
func BlockedProposals(db *sql.DB, decidedSince time.Time) ([]Proposal, error) {
	rows, err := db.Query(
		`SELECT id, kind, dedupe_key, payload_json, affected, state, decided_by FROM structure_proposals
		 WHERE state = ? OR (state IN (?, ?) AND julianday(decided_at) >= julianday(?))`,
		ProposalPending, ProposalApproved, ProposalRejected, decidedSince)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		var p Proposal
		if err := rows.Scan(&p.ID, &p.Kind, &p.Key, &p.PayloadJSON, &p.Affected, &p.State, &p.DecidedBy); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ExpireProposals marks pending proposals created before createdBefore as expired.
func ExpireProposals(db *sql.DB, createdBefore time.Time) (int64, error) {
	res, err := db.Exec(`UPDATE structure_proposals SET state = ?, decided_at = ? WHERE state = ? AND julianday(created_at) < julianday(?)`,
		ProposalExpired, time.Now(), ProposalPending, createdBefore)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SectionActivity is how much a Section is used, for Structure Proposals.
type SectionActivity struct {
	Recent          int  // Work Items reported in the recent window
	Recent8w        int  // Work Items reported in the retire window
	Open            int  // Open Work Items filed there
	CreatedBefore8w bool // the Section existed for the whole retire window
}

// GetSectionActivity counts Work Items per active node reported since recent and since retire.
func GetSectionActivity(db *sql.DB, recent, retire time.Time) (map[int64]SectionActivity, error) {
	rows, err := db.Query(
		`SELECT t.id, julianday(t.created_at) < julianday(?),
		    (SELECT COUNT(*) FROM work_items w WHERE w.section_id = t.id AND julianday(w.reported_at) >= julianday(?)),
		    (SELECT COUNT(*) FROM work_items w WHERE w.section_id = t.id AND julianday(w.reported_at) >= julianday(?)),
		    (SELECT COUNT(*) FROM work_items w WHERE w.section_id = t.id AND w.status <> 'done')
		 FROM taxonomy_nodes t WHERE t.retired_at IS NULL`, retire, recent, retire)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]SectionActivity{}
	for rows.Next() {
		var id int64
		var a SectionActivity
		if err := rows.Scan(&id, &a.CreatedBefore8w, &a.Recent, &a.Recent8w, &a.Open); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, rows.Err()
}
