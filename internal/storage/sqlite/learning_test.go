package sqlite

import (
	"database/sql"
	"testing"
	"time"

	"reportbot/internal/domain"
)

func insertAsked(t *testing.T, db *sql.DB, desc string, at time.Time) int64 {
	t.Helper()
	id, err := InsertWorkItem(db, WorkItem{Description: desc, Author: "Dev A", AuthorID: "U1", Source: "slack",
		Status: domain.StatusInProgress, ReportedAt: at, FilingState: domain.FilingPending})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func choiceJSON(probs string) string {
	return `{"type":"choice","choice":"x","probabilities":` + probs + `,"confidence":0.5}`
}

func TestLearningSignalsCorrectionsAndMisrankedPicks(t *testing.T) {
	db := newTestDB(t)
	ids := seedSmallTaxonomy(t, db)
	a, b, infra := ids[SeedKey("Product A", "Release", "7.6.3")], ids[SeedKey("Product A", "Release", "7.4.7")], ids[SeedKey("Product A", "Infrastructure", "")]
	now := time.Now()
	conf := 0.9

	// A model-filed Work Item a person moved elsewhere: a correction.
	corrected := insertAsked(t, db, "moved by a person", now)
	if err := ApplyFiling(db, corrected, FilingOutcome{Placement: Placement{SectionID: a, PlacedBy: domain.PlacedByModel, Confidence: &conf,
		ModelSectionID: a, FilingState: domain.FilingFiled, HistoryNote: ReasonArrival}}); err != nil {
		t.Fatal(err)
	}
	if err := SetPlacement(db, corrected, Placement{SectionID: b, PlacedBy: domain.PlacedByPerson, HistoryNote: ReasonWebMove, ActorID: "U7"}); err != nil {
		t.Fatal(err)
	}

	// "Where does this go?" answered with a Section other than the model's #1, outside its top 3.
	misranked := insertAsked(t, db, "picked another", now)
	low := 0.4
	if err := ApplyFiling(db, misranked, FilingOutcome{
		Placement: Placement{PlacedBy: domain.PlacedByModel, Confidence: &low, ModelSectionID: a, FilingState: domain.FilingAsking, Reason: domain.ReasonLowConfidence, HistoryNote: ReasonArrival},
		Judgments: []Judgment{{Kind: "section_choice", Subject: "section", Model: "m",
			AnswerJSON: choiceJSON(`{"` + domain.SectionKey(a) + `":0.4,"` + domain.SectionKey(b) + `":0.35}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := SetPlacement(db, misranked, Placement{SectionID: infra, PlacedBy: domain.PlacedByPerson, Confidence: &low, HistoryNote: ReasonButton}); err != nil {
		t.Fatal(err)
	}

	// A pick that agrees with the model's #1 and an Undo are not signals.
	agreed := insertAsked(t, db, "agreed", now)
	if err := ApplyFiling(db, agreed, FilingOutcome{Placement: Placement{PlacedBy: domain.PlacedByModel, ModelSectionID: a,
		FilingState: domain.FilingAsking, HistoryNote: ReasonArrival}}); err != nil {
		t.Fatal(err)
	}
	if err := SetPlacement(db, agreed, Placement{SectionID: a, PlacedBy: domain.PlacedByPerson, HistoryNote: ReasonButton}); err != nil {
		t.Fatal(err)
	}
	if err := SetPlacement(db, corrected, Placement{SectionID: a, PlacedBy: domain.PlacedByPerson, HistoryNote: ReasonUndo}); err != nil {
		t.Fatal(err)
	}

	sigs, err := GetLearningSignals(db, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 2 {
		t.Fatalf("want 2 signals, got %+v", sigs)
	}
	byItem := map[int64]LearningSignal{}
	for _, s := range sigs {
		byItem[s.WorkItemID] = s
	}
	if c := byItem[corrected]; c.Kind != SignalCorrection || c.ModelSection != a || c.ChosenSection != b || c.Description != "moved by a person" ||
		c.Person != "U7" || c.Author != "Dev A" {
		t.Fatalf("correction: %+v", c)
	}
	if m := byItem[misranked]; m.Kind != SignalMisranked || m.ModelSection != a || m.ChosenSection != infra || !m.OutsideTop3 || m.Person != "U1" {
		t.Fatalf("mis-ranked pick: %+v", m)
	}
	if later, _ := GetLearningSignals(db, now.Add(time.Hour)); len(later) != 0 {
		t.Fatalf("signals before since must be skipped: %+v", later)
	}
}

// People are counted only by Slack user ID: a free-text author name never identifies a person.
func TestLearningSignalPeopleAreSlackIDsOnly(t *testing.T) {
	db := newTestDB(t)
	ids := seedSmallTaxonomy(t, db)
	a, b := ids[SeedKey("Product A", "Release", "7.6.3")], ids[SeedKey("Product A", "Release", "7.4.7")]
	now := time.Now()
	misrank := func(author, authorID string) {
		id, err := InsertWorkItem(db, WorkItem{Description: "x", Author: author, AuthorID: authorID, Source: "slack",
			Status: domain.StatusInProgress, ReportedAt: now, FilingState: domain.FilingPending})
		if err != nil {
			t.Fatal(err)
		}
		if err := ApplyFiling(db, id, FilingOutcome{Placement: Placement{PlacedBy: domain.PlacedByModel, ModelSectionID: a,
			FilingState: domain.FilingAsking, HistoryNote: ReasonArrival}}); err != nil {
			t.Fatal(err)
		}
		if err := SetPlacement(db, id, Placement{SectionID: b, PlacedBy: domain.PlacedByPerson, HistoryNote: ReasonButton, ActorID: "UMGR"}); err != nil {
			t.Fatal(err)
		}
	}
	misrank("Name One", "")
	misrank("Name Two", "")
	sigs, _ := GetLearningSignals(db, now.Add(-time.Hour))
	if len(sigs) != 2 || sigs[0].Person != "" || sigs[1].Person != "" {
		t.Fatalf("signals without a Slack ID still count, with no person: %+v", sigs)
	}
	misrank("Name Three", "U5")
	sigs, _ = GetLearningSignals(db, now.Add(-time.Hour))
	if sigs[2].Person != "U5" {
		t.Fatalf("a mis-ranked pick belongs to the Work Item's author ID, got %q", sigs[2].Person)
	}
}

func TestPersonDecidedItemsForReplay(t *testing.T) {
	db := newTestDB(t)
	ids := seedSmallTaxonomy(t, db)
	a := ids[SeedKey("Product A", "Release", "7.6.3")]
	now := time.Now()
	person := insertAsked(t, db, "person", now)
	model := insertAsked(t, db, "model", now)
	old := insertAsked(t, db, "old", now.AddDate(0, 0, -70))
	for _, id := range []int64{person, old} {
		if err := SetPlacement(db, id, Placement{SectionID: a, PlacedBy: domain.PlacedByPerson, HistoryNote: ReasonButton}); err != nil {
			t.Fatal(err)
		}
	}
	if err := SetPlacement(db, model, Placement{SectionID: a, PlacedBy: domain.PlacedByModel, HistoryNote: ReasonArrival}); err != nil {
		t.Fatal(err)
	}
	items, err := GetPersonDecidedItems(db, now.AddDate(0, 0, -56))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != person {
		t.Fatalf("want only the recent person-decided item, got %+v", items)
	}
}

func TestLearnedNotesApplyAndUndo(t *testing.T) {
	db := newTestDB(t)
	ids := seedSmallTaxonomy(t, db)
	a := ids[SeedKey("Product A", "Release", "7.6.3")]
	runID, err := InsertLearningRun(db, LearningRun{Trigger: "nightly", StartedAt: time.Now(), Decision: RunApplied})
	if err != nil {
		t.Fatal(err)
	}
	hist, err := ApplyLearnedNotes(db, runID, []NoteChange{{SectionID: a, Old: "", New: "Release work. Not infrastructure."}})
	if err != nil || len(hist) != 1 {
		t.Fatalf("apply: %v %v", hist, err)
	}
	tax, _ := LoadTaxonomy(db)
	if s, _ := tax.Section(a); s.LearnedNotes != "Release work. Not infrastructure." || s.Description != "" {
		t.Fatalf("notes must be stored apart from the description: %+v", s)
	}
	if _, err := ApplyLearnedNotes(db, runID, []NoteChange{{SectionID: a, Old: "stale", New: "x"}}); err == nil {
		t.Fatal("applying over notes that changed since drafting must fail")
	}

	h, err := UndoLearnedNotes(db, hist[0], "U9")
	if err != nil || h.SectionID != a {
		t.Fatalf("undo: %+v %v", h, err)
	}
	tax, _ = LoadTaxonomy(db)
	if s, _ := tax.Section(a); s.LearnedNotes != "" {
		t.Fatalf("undo must restore the previous notes: %+v", s)
	}
	if _, err := UndoLearnedNotes(db, hist[0], "U9"); err != ErrNothingToUndo {
		t.Fatalf("a second undo must be refused, got %v", err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM learned_notes_history WHERE action = 'undone' AND actor_id = 'U9'`).Scan(&n)
	if n != 1 {
		t.Fatalf("undo must be recorded once, got %d", n)
	}

	if err := SetLearnedNotes(db, a, "Edited by a manager.", "U2"); err != nil {
		t.Fatal(err)
	}
	if _, err := UndoLearnedNotes(db, hist[0], "U9"); err != ErrNothingToUndo {
		t.Fatalf("an undone change cannot be undone again after a manager edit, got %v", err)
	}

	// An old Undo never overwrites a later run's notes.
	run2, _ := InsertLearningRun(db, LearningRun{Trigger: "nightly", StartedAt: time.Now(), Decision: RunApplied})
	first, err := ApplyLearnedNotes(db, run2, []NoteChange{{SectionID: a, Old: "Edited by a manager.", New: "First."}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyLearnedNotes(db, run2, []NoteChange{{SectionID: a, Old: "First.", New: "Second."}}); err != nil {
		t.Fatal(err)
	}
	if _, err := UndoLearnedNotes(db, first[0], "U9"); err != ErrNotesChanged || err.Error() != "the learned notes changed since; nothing to undo" {
		t.Fatalf("an Undo of superseded notes must be refused, got %v", err)
	}
	if tax, _ := LoadTaxonomy(db); func() string { s, _ := tax.Section(a); return s.LearnedNotes }() != "Second." {
		t.Fatal("the later notes must stay")
	}
	if err := SetLearnedNotes(db, a, string(make([]rune, domain.MaxLearnedNotes+1)), "U2"); err == nil {
		t.Fatal("notes over the cap must be rejected")
	}
}

func TestLearningRunsWatermarkPerSection(t *testing.T) {
	db := newTestDB(t)
	t0 := time.Now().Add(-48 * time.Hour)
	if _, err := InsertLearningRun(db, LearningRun{Trigger: "nightly", StartedAt: t0, Sections: []int64{3, 4}, Decision: RunDiscarded}); err != nil {
		t.Fatal(err)
	}
	if _, err := InsertLearningRun(db, LearningRun{Trigger: "nightly", StartedAt: t0.Add(time.Hour), Sections: []int64{4}, Decision: RunFailed}); err != nil {
		t.Fatal(err)
	}
	marks, err := SectionWatermarks(db)
	if err != nil {
		t.Fatal(err)
	}
	if !marks[3].Equal(t0) || !marks[4].Equal(t0) {
		t.Fatalf("a failed run must not consume signals: %v", marks)
	}
}

func TestReplayCache(t *testing.T) {
	db := newTestDB(t)
	if _, ok, err := GetReplayAnswer(db, "h1"); ok || err != nil {
		t.Fatalf("empty cache: %v %v", ok, err)
	}
	if err := PutReplayAnswer(db, "h1", `{"answers":{}}`, "m"); err != nil {
		t.Fatal(err)
	}
	if err := PutReplayAnswer(db, "h1", `{"answers":{}}`, "m"); err != nil {
		t.Fatalf("a repeated put must be harmless: %v", err)
	}
	if got, ok, _ := GetReplayAnswer(db, "h1"); !ok || got != `{"answers":{}}` {
		t.Fatalf("cache read: %q %v", got, ok)
	}
}

func TestSecondOpinionCandidatesAndAssistantPlacement(t *testing.T) {
	db := newTestDB(t)
	ids := seedSmallTaxonomy(t, db)
	a := ids[SeedKey("Product A", "Release", "7.6.3")]
	low := 0.4
	asked := func(desc string) int64 {
		id := insertAsked(t, db, desc, time.Now())
		if err := ApplyFiling(db, id, FilingOutcome{
			Placement: Placement{PlacedBy: domain.PlacedByModel, Confidence: &low, ModelSectionID: a, FilingState: domain.FilingAsking, Reason: domain.ReasonLowConfidence, HistoryNote: ReasonArrival},
			Judgments: []Judgment{{Kind: "section_choice", Subject: "section", Model: "m", AnswerJSON: choiceJSON(`{"` + domain.SectionKey(a) + `":0.4}`)}},
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	waiting := asked("waiting")
	answered := asked("answered")
	if err := SetPlacement(db, answered, Placement{SectionID: a, PlacedBy: domain.PlacedByPerson, HistoryNote: ReasonButton}); err != nil {
		t.Fatal(err)
	}
	if got, _ := GetSecondOpinionCandidates(db, time.Now().Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("items asked less than the wait ago are not due: %+v", got)
	}
	got, err := GetSecondOpinionCandidates(db, time.Now().Add(time.Hour))
	if err != nil || len(got) != 1 || got[0].ID != waiting {
		t.Fatalf("want only the unanswered item, got %+v err=%v", got, err)
	}
	ans, err := LatestSectionChoice(db, waiting)
	if err != nil || ans == "" {
		t.Fatalf("latest section choice: %q %v", ans, err)
	}

	ok, err := PlaceByAssistant(db, waiting, a)
	if err != nil || !ok {
		t.Fatalf("assistant placement: %v %v", ok, err)
	}
	it, _ := GetWorkItemByID(db, waiting)
	if it.SectionID != a || it.PlacedBy != domain.PlacedByAssistant || it.FilingState != domain.FilingFiled {
		t.Fatalf("assistant placement not stored: %+v", it)
	}
	if ok, _ := PlaceByAssistant(db, answered, a); ok {
		t.Fatal("an item a person already filed must not be placed by the assistant")
	}

	monday, next := domain.CurrentWeekRangeAt(time.Now())
	st, err := GetWeekStats(db, monday, next)
	if err != nil || st.FiledByAssistant != 1 {
		t.Fatalf("assistant placements must be counted apart: %+v %v", st, err)
	}
}

func TestStructureProposalsDecideOnceAndCooldown(t *testing.T) {
	db := newTestDB(t)
	id, err := InsertProposal(db, Proposal{Kind: "retire", Key: "retire:7", PayloadJSON: `{}`, Affected: 0})
	if err != nil {
		t.Fatal(err)
	}
	won, err := DecideProposal(db, id, ProposalRejected, "U1")
	if err != nil || !won {
		t.Fatalf("first decision must win: %v %v", won, err)
	}
	if won, _ := DecideProposal(db, id, ProposalApproved, "U2"); won {
		t.Fatal("a proposal is decided at most once")
	}
	p, err := GetProposal(db, id)
	if err != nil || p.State != ProposalRejected || p.DecidedBy != "U1" {
		t.Fatalf("stored decision: %+v %v", p, err)
	}
	pending, _ := InsertProposal(db, Proposal{Kind: "new", Key: "new:1:infra", PayloadJSON: `{}`})
	_ = pending
	keys, err := BlockedProposalKeys(db, time.Now().AddDate(0, 0, -56))
	if err != nil || !keys["retire:7"] || !keys["new:1:infra"] {
		t.Fatalf("rejected and pending proposals are blocked: %v %v", keys, err)
	}
	if keys, _ := BlockedProposalKeys(db, time.Now().Add(time.Hour)); keys["retire:7"] || !keys["new:1:infra"] {
		t.Fatalf("a rejection expires after the cooldown, a pending proposal does not: %v", keys)
	}
	if err := ReopenProposal(db, pending); err != nil {
		t.Fatal(err)
	}
}

func TestSectionActivity(t *testing.T) {
	db := newTestDB(t)
	ids := seedSmallTaxonomy(t, db)
	a := ids[SeedKey("Product A", "Release", "7.6.3")]
	it := insertAsked(t, db, "recent", time.Now())
	if err := SetPlacement(db, it, Placement{SectionID: a, PlacedBy: domain.PlacedByPerson, HistoryNote: ReasonButton}); err != nil {
		t.Fatal(err)
	}
	act, err := GetSectionActivity(db, time.Now().AddDate(0, 0, -28), time.Now().AddDate(0, 0, -56))
	if err != nil {
		t.Fatal(err)
	}
	if got := act[a]; got.Recent != 1 || got.Recent8w != 1 || got.Open != 1 {
		t.Fatalf("activity for a: %+v", got)
	}
	if got := act[ids[SeedKey("Product A", "Release", "7.4.7")]]; got.Recent8w != 0 || got.Open != 0 || got.CreatedBefore8w {
		t.Fatalf("a new empty Section is not old enough to retire: %+v", got)
	}
}
