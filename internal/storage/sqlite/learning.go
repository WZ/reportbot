package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"reportbot/internal/domain"
)

// ReasonSecondOpinion is the placement_history reason for an assistant Placement.
const ReasonSecondOpinion = "second_opinion"

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
	OutsideTop3   bool // the chosen Section was not among the model's three most likely
	Person        string // who decided: the actor, else the Work Item's author
	Author        string // the Work Item's author name
	CreatedAt     time.Time
}

// GetLearningSignals returns corrections and mis-ranked picks recorded after since, oldest first.
func GetLearningSignals(db *sql.DB, since time.Time) ([]LearningSignal, error) {
	rows, err := db.Query(
		`SELECT h.id, h.work_item_id, w.description, h.from_section, h.from_placed_by, h.to_section, h.model_section, h.created_at,
		    COALESCE(NULLIF(h.actor_id, ''), NULLIF(w.author_id, ''), w.author), w.author,
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
		if err := rows.Scan(&s.EventID, &s.WorkItemID, &s.Description, &from, &fromBy, &s.ChosenSection, &model, &s.CreatedAt, &s.Person, &s.Author, &answer); err != nil {
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

// GetPersonDecidedItems returns Work Items a person placed in a Section, reported since, newest first.
func GetPersonDecidedItems(db *sql.DB, since time.Time) ([]WorkItem, error) {
	return queryWorkItems(db,
		`SELECT `+workItemColumns+` FROM work_items
		 WHERE placed_by = 'person' AND section_id IS NOT NULL AND julianday(reported_at) >= julianday(?)
		 ORDER BY reported_at DESC, id DESC`, since)
}

// Learning run decisions.
const (
	RunApplied   = "applied"
	RunDiscarded = "discarded"
	RunSkipped   = "skipped" // nothing to learn, or too few person decisions to replay
	RunFailed    = "failed"
)

// LearningRun is the audit record of one Learned Notes run.
type LearningRun struct {
	ID          int64
	Trigger     string // "nightly" | "retrospect"
	StartedAt   time.Time
	Signals     int
	Sections    []int64 // Sections whose notes were drafted; their signals are consumed unless the run failed
	Candidates  string  // JSON of the drafted notes
	ReplayItems int
	OldAccuracy *float64
	NewAccuracy *float64
	Decision    string
	Reason      string
}

func InsertLearningRun(db *sql.DB, r LearningRun) (int64, error) {
	secs, _ := json.Marshal(nonNilIDs(r.Sections))
	res, err := db.Exec(
		`INSERT INTO learning_runs (trigger, started_at, signals, sections, candidates, replay_items, old_accuracy, new_accuracy, decision, reason, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Trigger, r.StartedAt, r.Signals, string(secs), r.Candidates, r.ReplayItems, nullFloat(r.OldAccuracy), nullFloat(r.NewAccuracy),
		r.Decision, r.Reason, time.Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func nonNilIDs(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// SectionWatermarks returns, per Section, when a run last considered it; its signals before then are used up.
func SectionWatermarks(db *sql.DB) (map[int64]time.Time, error) {
	rows, err := db.Query(`SELECT started_at, sections FROM learning_runs WHERE decision IN (?, ?) ORDER BY id`, RunApplied, RunDiscarded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var at time.Time
		var secs string
		if err := rows.Scan(&at, &secs); err != nil {
			return nil, err
		}
		var ids []int64
		_ = json.Unmarshal([]byte(secs), &ids)
		for _, id := range ids {
			if at.After(out[id]) {
				out[id] = at
			}
		}
	}
	return out, rows.Err()
}

// NoteChange replaces a Section's Learned Notes; Old must still be current when applied.
type NoteChange struct {
	SectionID int64
	Old, New  string
}

// Learned Notes history actions.
const (
	NotesApplied = "applied"
	NotesUndone  = "undone"
	NotesEdited  = "edited"
)

var ErrNothingToUndo = errors.New("nothing to undo")

func insertNoteHistory(q querier, sectionID, runID int64, oldNotes, newNotes, action, actor string) (int64, error) {
	res, err := q.Exec(
		`INSERT INTO learned_notes_history (section_id, run_id, old_notes, new_notes, action, actor_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sectionID, nullID(runID), oldNotes, newNotes, action, actor, time.Now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ApplyLearnedNotes writes a run's notes atomically and returns their history IDs (for Undo).
func ApplyLearnedNotes(db *sql.DB, runID int64, changes []NoteChange) ([]int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var ids []int64
	for _, c := range changes {
		res, err := tx.Exec(`UPDATE taxonomy_nodes SET learned_notes = ?, updated_at = ? WHERE id = ? AND learned_notes = ?`,
			c.New, time.Now(), c.SectionID, c.Old)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, fmt.Errorf("learned notes of Section %d changed since they were drafted", c.SectionID)
		}
		id, err := insertNoteHistory(tx, c.SectionID, runID, c.Old, c.New, NotesApplied, "")
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, tx.Commit()
}

// NoteHistory is one change to a Section's Learned Notes.
type NoteHistory struct {
	ID        int64
	SectionID int64
	RunID     int64
	Old, New  string
	Action    string
	Undone    bool
}

func GetNoteHistory(db *sql.DB, id int64) (NoteHistory, error) {
	var h NoteHistory
	var run sql.NullInt64
	var undone sql.NullTime
	err := db.QueryRow(`SELECT id, section_id, run_id, old_notes, new_notes, action, undone_at FROM learned_notes_history WHERE id = ?`, id).
		Scan(&h.ID, &h.SectionID, &run, &h.Old, &h.New, &h.Action, &undone)
	h.RunID, h.Undone = run.Int64, undone.Valid
	return h, err
}

// UndoLearnedNotes restores the notes an applied change replaced, if they are still current.
func UndoLearnedNotes(db *sql.DB, historyID int64, actorID string) (NoteHistory, error) {
	tx, err := db.Begin()
	if err != nil {
		return NoteHistory{}, err
	}
	defer tx.Rollback()
	var h NoteHistory
	var undone sql.NullTime
	if err := tx.QueryRow(`SELECT id, section_id, old_notes, new_notes, action, undone_at FROM learned_notes_history WHERE id = ?`, historyID).
		Scan(&h.ID, &h.SectionID, &h.Old, &h.New, &h.Action, &undone); err != nil {
		return h, err
	}
	if h.Action != NotesApplied || undone.Valid {
		return h, ErrNothingToUndo
	}
	res, err := tx.Exec(`UPDATE taxonomy_nodes SET learned_notes = ?, updated_at = ? WHERE id = ? AND learned_notes = ?`, h.Old, time.Now(), h.SectionID, h.New)
	if err != nil {
		return h, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return h, ErrNothingToUndo
	}
	if _, err := tx.Exec(`UPDATE learned_notes_history SET undone_at = ? WHERE id = ?`, time.Now(), h.ID); err != nil {
		return h, err
	}
	if _, err := insertNoteHistory(tx, h.SectionID, 0, h.New, h.Old, NotesUndone, actorID); err != nil {
		return h, err
	}
	return h, tx.Commit()
}

// SetLearnedNotes is a manager editing or clearing a Section's Learned Notes.
func SetLearnedNotes(db *sql.DB, sectionID int64, notes, actorID string) error {
	notes = strings.TrimSpace(notes)
	if len([]rune(notes)) > domain.MaxLearnedNotes {
		return fmt.Errorf("learned notes are limited to %d characters", domain.MaxLearnedNotes)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	if err := tx.QueryRow(`SELECT learned_notes FROM taxonomy_nodes WHERE id = ?`, sectionID).Scan(&old); err != nil {
		return err
	}
	if old == notes {
		return nil
	}
	if _, err := tx.Exec(`UPDATE taxonomy_nodes SET learned_notes = ?, updated_at = ? WHERE id = ?`, notes, time.Now(), sectionID); err != nil {
		return err
	}
	if _, err := insertNoteHistory(tx, sectionID, 0, old, notes, NotesEdited, actorID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetReplayAnswer returns a cached decision-model response for an identical replay request.
func GetReplayAnswer(db *sql.DB, hash string) (string, bool, error) {
	var answer string
	err := db.QueryRow(`SELECT answer_json FROM replay_answers WHERE request_hash = ?`, hash).Scan(&answer)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return answer, err == nil, err
}

func PutReplayAnswer(db *sql.DB, hash, answerJSON, model string) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO replay_answers (request_hash, answer_json, model, created_at) VALUES (?, ?, ?, ?)`,
		hash, answerJSON, model, time.Now())
	return err
}

// GetSecondOpinionCandidates returns Undetermined Work Items the decision model was unsure
// about, whose reporter was asked before askedBefore and has not picked a Section.
func GetSecondOpinionCandidates(db *sql.DB, askedBefore time.Time) ([]WorkItem, error) {
	return queryWorkItems(db,
		`SELECT `+workItemColumns+` FROM work_items w
		 WHERE w.section_id IS NULL AND w.filing_state = 'asking' AND w.undetermined_reason = 'low_confidence'
		   AND (SELECT MAX(julianday(j.created_at)) FROM model_judgments j WHERE j.work_item_id = w.id AND j.kind = 'section_choice') <= julianday(?)
		 ORDER BY w.id`, askedBefore)
}

// LatestSectionChoice returns the raw answer of the item's most recent Section Choice.
func LatestSectionChoice(db *sql.DB, itemID int64) (string, error) {
	var answer string
	err := db.QueryRow(`SELECT answer_json FROM model_judgments WHERE work_item_id = ? AND kind = 'section_choice' ORDER BY id DESC LIMIT 1`, itemID).Scan(&answer)
	return answer, err
}

// PlaceByAssistant files a still-Undetermined Work Item under the assistant's Section; it
// reports false when a person or the model placed it first.
func PlaceByAssistant(db *sql.DB, itemID, sectionID int64) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var waiting bool
	if err := tx.QueryRow(`SELECT section_id IS NULL AND filing_state = 'asking' FROM work_items WHERE id = ?`, itemID).Scan(&waiting); err != nil {
		return false, err
	}
	if !waiting {
		return false, nil
	}
	if err := applyPlacement(tx, itemID, Placement{SectionID: sectionID, PlacedBy: domain.PlacedByAssistant,
		FilingState: domain.FilingFiled, HistoryNote: ReasonSecondOpinion}); err != nil {
		return false, err
	}
	return true, tx.Commit()
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
