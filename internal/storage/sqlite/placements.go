package sqlite

import (
	"database/sql"
	"time"

	"reportbot/internal/domain"
)

// Placement reasons recorded in placement_history.
const (
	ReasonArrival       = "arrival"
	ReasonButton        = "button"
	ReasonEdit          = "edit"
	ReasonWebMove       = "web_move"
	ReasonReclassify    = "reclassify"
	ReasonTaxonomyMerge = "taxonomy_merge"
	ReasonUndo          = "undo"
	ReasonImport        = "import"
)

// Judgment is one raw model answer kept for re-tuning thresholds.
type Judgment struct {
	WorkItemID   int64
	Kind         string // "section_pick" | "section_check" | "status" | "ticket" | "update_candidate" | "link_candidate"
	Subject      string
	AnswerJSON   string
	Model        string
	InputTokens  int64
	OutputTokens int64
	LatencyMS    int64
}

// Placement is a decision to put a Work Item in a Section (0 = Undetermined).
type Placement struct {
	SectionID    int64
	PlacedBy     string
	Confidence   *float64
	LLMSectionID int64 // the LLM's pick behind this decision, 0 if none
	FilingState  string
	Reason       string // Undetermined reason, empty when filed in a Section
	HistoryNote  string // placement_history.reason
	ActorID      string
}

// FilingOutcome is everything one filing pass decided for a Work Item.
type FilingOutcome struct {
	Placement  Placement
	Status     string // empty = keep
	StatusNote string
	TicketIDs  string // empty = keep
	Judgments  []Judgment
}

type PlacementEvent struct {
	ID           int64
	WorkItemID   int64
	FromSection  int64
	FromPlacedBy string
	ToSection    int64
	PlacedBy     string
	ModelSection int64
	Confidence   *float64
	Reason       string
	ActorID      string
	CreatedAt    time.Time
}

func insertJudgments(q querier, js []Judgment) error {
	for _, j := range js {
		if _, err := q.Exec(
			`INSERT INTO model_judgments (work_item_id, kind, subject, answer_json, model, input_tokens, output_tokens, latency_ms, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			j.WorkItemID, j.Kind, j.Subject, j.AnswerJSON, j.Model, j.InputTokens, j.OutputTokens, j.LatencyMS, time.Now(),
		); err != nil {
			return err
		}
	}
	return nil
}

func InsertJudgments(db *sql.DB, js []Judgment) error { return insertJudgments(db, js) }

// applyPlacement updates the item and records history when the Section or decider changes.
func applyPlacement(q querier, itemID int64, p Placement) error {
	var fromSection sql.NullInt64
	var fromBy string
	var llmSection sql.NullInt64
	if err := q.QueryRow(`SELECT section_id, placed_by, llm_section_id FROM work_items WHERE id = ?`, itemID).Scan(&fromSection, &fromBy, &llmSection); err != nil {
		return err
	}
	modelSection := p.LLMSectionID
	if modelSection == 0 {
		modelSection = llmSection.Int64
	}
	if _, err := q.Exec(
		`UPDATE work_items SET section_id = ?, placed_by = ?, placement_confidence = ?, llm_section_id = ?,
			filing_state = ?, undetermined_reason = ?, updated_at = ?
		 WHERE id = ?`,
		nullID(p.SectionID), p.PlacedBy, nullFloat(p.Confidence), nullID(modelSection),
		p.FilingState, p.Reason, time.Now(), itemID,
	); err != nil {
		return err
	}
	if fromSection.Int64 == p.SectionID && fromBy == p.PlacedBy {
		return nil
	}
	_, err := q.Exec(
		`INSERT INTO placement_history (work_item_id, from_section, from_placed_by, to_section, placed_by, model_section, confidence, reason, actor_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		itemID, nullID(fromSection.Int64), fromBy, nullID(p.SectionID), p.PlacedBy, nullID(modelSection),
		nullFloat(p.Confidence), p.HistoryNote, p.ActorID, time.Now(),
	)
	return err
}

// ApplyFiling writes a filing pass's Placement, Status, tickets and raw judgments atomically.
func ApplyFiling(db *sql.DB, itemID int64, o FilingOutcome) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applyPlacement(tx, itemID, o.Placement); err != nil {
		return err
	}
	if o.Placement.FilingState != domain.FilingFailed {
		if _, err := tx.Exec(`UPDATE work_items SET failure_notified_at = NULL WHERE id = ?`, itemID); err != nil {
			return err
		}
	}
	if o.Status != "" {
		if _, err := tx.Exec(
			`UPDATE work_items SET status = ?, status_note = ?,
				done_at = CASE WHEN ? = 'done' THEN COALESCE(done_at, reported_at) ELSE NULL END
			 WHERE id = ?`,
			o.Status, o.StatusNote, o.Status, itemID,
		); err != nil {
			return err
		}
	}
	if o.TicketIDs != "" {
		if _, err := tx.Exec(`UPDATE work_items SET ticket_ids = ? WHERE id = ?`, o.TicketIDs, itemID); err != nil {
			return err
		}
	}
	for i := range o.Judgments {
		o.Judgments[i].WorkItemID = itemID
	}
	if err := insertJudgments(tx, o.Judgments); err != nil {
		return err
	}
	return tx.Commit()
}

// SetPlacement records a Placement outside a filing pass (a person's choice, a re-classification, an undo).
func SetPlacement(db *sql.DB, itemID int64, p Placement) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if p.FilingState == "" {
		p.FilingState = domain.FilingFiled
	}
	if err := applyPlacement(tx, itemID, p); err != nil {
		return err
	}
	return tx.Commit()
}

func GetPlacementEvent(db *sql.DB, id int64) (PlacementEvent, error) {
	var e PlacementEvent
	var from, to, model sql.NullInt64
	var conf sql.NullFloat64
	err := db.QueryRow(
		`SELECT id, work_item_id, from_section, from_placed_by, to_section, placed_by, model_section, confidence, reason, actor_id, created_at
		 FROM placement_history WHERE id = ?`, id,
	).Scan(&e.ID, &e.WorkItemID, &from, &e.FromPlacedBy, &to, &e.PlacedBy, &model, &conf, &e.Reason, &e.ActorID, &e.CreatedAt)
	e.FromSection, e.ToSection, e.ModelSection = from.Int64, to.Int64, model.Int64
	if conf.Valid {
		c := conf.Float64
		e.Confidence = &c
	}
	return e, err
}

// LatestPlacementEventID returns the newest history row for an item.
func LatestPlacementEventID(db *sql.DB, itemID int64) (int64, error) {
	var id int64
	err := db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM placement_history WHERE work_item_id = ?`, itemID).Scan(&id)
	return id, err
}

// Correction is a person putting a Work Item somewhere other than the LLM's pick.
type Correction struct {
	WorkItemID  int64
	Description string
	FromSection int64 // the LLM's pick
	ToSection   int64
	CreatedAt   time.Time
}

// GetRecentCorrections returns person Placements that disagree with the LLM's pick, newest first.
func GetRecentCorrections(db *sql.DB, since time.Time, limit int) ([]Correction, error) {
	rows, err := db.Query(
		`SELECT h.work_item_id, w.description, h.model_section, h.to_section, h.created_at
		 FROM placement_history h JOIN work_items w ON w.id = h.work_item_id
		 WHERE h.placed_by = 'person' AND h.reason <> 'import' AND h.reason <> 'undo'
		   AND h.model_section IS NOT NULL AND h.to_section IS NOT NULL AND h.model_section <> h.to_section
		   AND julianday(h.created_at) >= julianday(?)
		 ORDER BY h.id DESC LIMIT ?`,
		since, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Correction
	for rows.Next() {
		var c Correction
		if err := rows.Scan(&c.WorkItemID, &c.Description, &c.FromSection, &c.ToSection, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetPlacedItems returns Work Items with a Placement, newest first, for the similar-item index.
func GetPlacedItems(db *sql.DB, limit int) ([]domain.PlacedItem, error) {
	rows, err := db.Query(
		`SELECT id, description, author, section_id, placed_by = 'person', reported_at
		 FROM work_items WHERE section_id IS NOT NULL
		 ORDER BY reported_at DESC, id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PlacedItem
	for rows.Next() {
		var p domain.PlacedItem
		if err := rows.Scan(&p.WorkItemID, &p.Description, &p.Author, &p.SectionID, &p.ByPerson, &p.ReportedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// WeekStats summarises filing for /stats.
type WeekStats struct {
	ItemsThisWeek     int
	FiledByModel      int // check passed, filed without asking
	Asked             int // check failed: the reporter was asked
	FailedFiling      int
	Undetermined      int
	PersonCorrections map[int64]int // LLM-picked Section -> corrections away from it
}

// GetWeekStats counts this week's arrivals by outcome and the Undetermined items on the report.
func GetWeekStats(db *sql.DB, monday, nextMonday time.Time) (WeekStats, error) {
	s := WeekStats{PersonCorrections: map[int64]int{}}
	err := db.QueryRow(
		`SELECT COUNT(*),
		    COALESCE(SUM(EXISTS (SELECT 1 FROM placement_history h WHERE h.work_item_id = w.id AND h.reason IN ('arrival','reclassify') AND h.placed_by = 'model' AND h.to_section IS NOT NULL)), 0),
		    COALESCE(SUM(EXISTS (SELECT 1 FROM model_judgments j WHERE j.work_item_id = w.id AND j.kind = 'section_check')
		         AND NOT EXISTS (SELECT 1 FROM placement_history h WHERE h.work_item_id = w.id AND h.placed_by = 'model' AND h.to_section IS NOT NULL)), 0),
		    COALESCE(SUM(w.filing_state = 'failed'), 0)
		 FROM work_items w
		 WHERE julianday(w.reported_at) >= julianday(?) AND julianday(w.reported_at) < julianday(?) AND w.source <> 'import'`,
		monday, nextMonday,
	).Scan(&s.ItemsThisWeek, &s.FiledByModel, &s.Asked, &s.FailedFiling)
	if err != nil {
		return s, err
	}
	week, err := GetWeekItems(db, monday, nextMonday)
	if err != nil {
		return s, err
	}
	for _, it := range week {
		if it.SectionID == 0 {
			s.Undetermined++
		}
	}
	corr, err := GetRecentCorrections(db, monday, 1000)
	if err != nil {
		return s, err
	}
	for _, c := range corr {
		if !c.CreatedAt.Before(nextMonday) {
			continue
		}
		s.PersonCorrections[c.FromSection]++
	}
	return s, nil
}

// MarkFailureNotified records that the author was told filing failed; it reports
// false when they already were and the item has not been filed since.
func MarkFailureNotified(db *sql.DB, itemID int64) (bool, error) {
	res, err := db.Exec(`UPDATE work_items SET failure_notified_at = ? WHERE id = ? AND failure_notified_at IS NULL`, time.Now(), itemID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// HasBeenFiled reports whether the LLM has ever picked a Section for the item.
func HasBeenFiled(db *sql.DB, itemID int64) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM model_judgments WHERE work_item_id = ? AND kind = 'section_pick'`, itemID).Scan(&n)
	return n > 0, err
}
