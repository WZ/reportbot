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
		c.Author != "Dev A" {
		t.Fatalf("correction: %+v", c)
	}
	if m := byItem[misranked]; m.Kind != SignalMisranked || m.ModelSection != a || m.ChosenSection != infra || !m.OutsideTop3 {
		t.Fatalf("mis-ranked pick: %+v", m)
	}
	if later, _ := GetLearningSignals(db, now.Add(time.Hour)); len(later) != 0 {
		t.Fatalf("signals before since must be skipped: %+v", later)
	}
}

func TestFallbackCandidatesAndPlacement(t *testing.T) {
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
	if got, _ := GetFallbackCandidates(db, time.Now().Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("items asked less than the wait ago are not due: %+v", got)
	}
	got, err := GetFallbackCandidates(db, time.Now().Add(time.Hour))
	if err != nil || len(got) != 1 || got[0].ID != waiting {
		t.Fatalf("want only the unanswered item, got %+v err=%v", got, err)
	}
	ans, err := LatestSectionChoice(db, waiting)
	if err != nil || ans == "" {
		t.Fatalf("latest section choice: %q %v", ans, err)
	}

	ok, err := PlaceByFallback(db, waiting, a)
	if err != nil || !ok {
		t.Fatalf("fallback placement: %v %v", ok, err)
	}
	it, _ := GetWorkItemByID(db, waiting)
	if it.SectionID != a || it.PlacedBy != domain.PlacedByFallback || it.FilingState != domain.FilingFiled {
		t.Fatalf("fallback placement not stored: %+v", it)
	}
	if ok, _ := PlaceByFallback(db, answered, a); ok {
		t.Fatal("an item a person already filed must not be placed by the fallback")
	}

	monday, next := domain.CurrentWeekRangeAt(time.Now())
	st, err := GetWeekStats(db, monday, next)
	if err != nil || st.FiledByFallback != 1 {
		t.Fatalf("fallback placements must be counted apart: %+v %v", st, err)
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
