package sqlite

import (
	"database/sql"
	"encoding/json"
	"sort"
	"time"

	"reportbot/internal/domain"
)

// migrateLearning adds Structure Proposals. learned_notes, learned_notes_history, learning_runs
// and replay_answers are unused; migrations here are additive, so they are not dropped.
func migrateLearning(tx *sql.Tx) error {
	_, err := tx.Exec(`
	ALTER TABLE taxonomy_nodes ADD COLUMN learned_notes TEXT NOT NULL DEFAULT '';

	CREATE TABLE learned_notes_history (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		section_id INTEGER NOT NULL,
		run_id     INTEGER,
		old_notes  TEXT NOT NULL,
		new_notes  TEXT NOT NULL,
		action     TEXT NOT NULL,
		actor_id   TEXT NOT NULL DEFAULT '',
		undone_at  DATETIME,
		created_at DATETIME NOT NULL
	);
	CREATE INDEX idx_lnh_section ON learned_notes_history(section_id, id);

	CREATE TABLE learning_runs (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		trigger      TEXT NOT NULL,
		started_at   DATETIME NOT NULL,
		signals      INTEGER NOT NULL DEFAULT 0,
		sections     TEXT NOT NULL DEFAULT '[]',
		candidates   TEXT NOT NULL DEFAULT '',
		replay_items INTEGER NOT NULL DEFAULT 0,
		old_accuracy REAL,
		new_accuracy REAL,
		decision     TEXT NOT NULL,
		reason       TEXT NOT NULL DEFAULT '',
		finished_at  DATETIME NOT NULL
	);

	CREATE TABLE replay_answers (
		request_hash TEXT PRIMARY KEY,
		answer_json  TEXT NOT NULL,
		model        TEXT NOT NULL,
		created_at   DATETIME NOT NULL
	);

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

// Kinds of learning signal.
const (
	SignalCorrection = "correction" // a person moved a model-filed Work Item elsewhere
	SignalMisranked  = "misranked"  // a person filed an unsure Work Item somewhere other than the model's #1
)

// LearningSignal is one person decision that disagrees with the decision model.
type LearningSignal struct {
	EventID       int64
	WorkItemID    int64
	Description   string
	Kind          string
	ModelSection  int64
	ChosenSection int64
	OutsideTop3   bool   // the chosen Section was not among the model's three most likely
	Author        string // the Work Item's author name
	CreatedAt     time.Time
}

// GetLearningSignals returns corrections and mis-ranked picks recorded after since, oldest first.
func GetLearningSignals(db *sql.DB, since time.Time) ([]LearningSignal, error) {
	rows, err := db.Query(
		`SELECT h.id, h.work_item_id, w.description, h.from_section, h.from_placed_by, h.to_section, h.model_section, h.created_at,
		    w.author,
		    COALESCE((SELECT j.answer_json FROM model_judgments j WHERE j.work_item_id = h.work_item_id AND j.kind = 'section_choice'
		              AND julianday(j.created_at) <= julianday(h.created_at) ORDER BY j.id DESC LIMIT 1), '')
		 FROM placement_history h JOIN work_items w ON w.id = h.work_item_id
		 WHERE h.placed_by = 'person' AND h.reason NOT IN ('import', 'undo', 'taxonomy_merge')
		   AND h.to_section IS NOT NULL AND julianday(h.created_at) > julianday(?)
		 ORDER BY h.id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LearningSignal
	for rows.Next() {
		var s LearningSignal
		var from, model sql.NullInt64
		var fromBy, answer string
		if err := rows.Scan(&s.EventID, &s.WorkItemID, &s.Description, &from, &fromBy, &s.ChosenSection, &model, &s.CreatedAt, &s.Author, &answer); err != nil {
			return nil, err
		}
		switch {
		case from.Valid && fromBy == domain.PlacedByModel && from.Int64 != s.ChosenSection:
			s.Kind, s.ModelSection = SignalCorrection, from.Int64
		case !from.Valid && model.Valid && model.Int64 != s.ChosenSection:
			s.Kind, s.ModelSection = SignalMisranked, model.Int64
			s.OutsideTop3 = !inTop3(answer, s.ChosenSection)
		default:
			continue
		}
		out = append(out, s)
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

// BlockedProposalKeys are changes not to propose again: pending ones, and ones decided since.
func BlockedProposalKeys(db *sql.DB, decidedSince time.Time) (map[string]bool, error) {
	rows, err := db.Query(`SELECT dedupe_key FROM structure_proposals WHERE state = ? OR julianday(decided_at) >= julianday(?)`,
		ProposalPending, decidedSince)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
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
