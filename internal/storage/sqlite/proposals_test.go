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

// asked files an unsure Work Item whose Section Choice ranked first > second > third.
func asked(t *testing.T, db *sql.DB, desc string, first, second, third int64) int64 {
	t.Helper()
	id := insertAsked(t, db, desc, time.Now())
	low := 0.4
	probs := `{"` + domain.SectionKey(first) + `":0.4,"` + domain.SectionKey(second) + `":0.3,"` + domain.SectionKey(third) + `":0.2}`
	if err := ApplyFiling(db, id, FilingOutcome{
		Placement: Placement{PlacedBy: domain.PlacedByModel, Confidence: &low, ModelSectionID: first, FilingState: domain.FilingAsking,
			Reason: domain.ReasonLowConfidence, HistoryNote: ReasonArrival},
		Judgments: []Judgment{{Kind: "section_choice", Subject: "section", Model: "m", AnswerJSON: choiceJSON(probs)}},
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOtherPicksAreOutsideTheModelsTop3(t *testing.T) {
	db := newTestDB(t)
	ids := seedSmallTaxonomy(t, db)
	a, b, infra := ids[SeedKey("Product A", "Release", "7.6.3")], ids[SeedKey("Product A", "Release", "7.4.7")], ids[SeedKey("Product A", "Infrastructure", "")]
	extra, err := InsertTaxonomyNode(db, ids[SeedKey("Product A", "Release", "")], domain.LevelSection, "7.2.1", "")
	if err != nil {
		t.Fatal(err)
	}
	person := func(id, to int64, why string) {
		t.Helper()
		if err := SetPlacement(db, id, Placement{SectionID: to, PlacedBy: domain.PlacedByPerson, HistoryNote: why, ActorID: "U1"}); err != nil {
			t.Fatal(err)
		}
	}
	since := time.Now().Add(-time.Hour)

	other := asked(t, db, "picked outside the top 3", a, b, infra)
	person(other, extra, ReasonEdit)
	second := asked(t, db, "picked the second choice", a, b, infra)
	person(second, b, ReasonButton)

	// A Fallback Placement a person then moves counts the same way.
	movedOut := asked(t, db, "fallback moved outside the top 3", a, b, infra)
	if ok, err := PlaceByFallback(db, movedOut, a); err != nil || !ok {
		t.Fatal(err)
	}
	person(movedOut, extra, ReasonWebMove)
	movedSecond := asked(t, db, "fallback moved to the second choice", a, b, infra)
	if ok, _ := PlaceByFallback(db, movedSecond, a); !ok {
		t.Fatal("setup")
	}
	person(movedSecond, b, ReasonWebMove)

	// A model-filed Work Item a person moves is not evidence for a missing Section.
	conf := 0.9
	corrected := insertAsked(t, db, "model filed, moved", time.Now())
	if err := ApplyFiling(db, corrected, FilingOutcome{Placement: Placement{SectionID: a, PlacedBy: domain.PlacedByModel, Confidence: &conf,
		ModelSectionID: a, FilingState: domain.FilingFiled, HistoryNote: ReasonArrival}}); err != nil {
		t.Fatal(err)
	}
	person(corrected, extra, ReasonWebMove)

	picks, err := GetOtherPicks(db, since)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, p := range picks {
		got[p.WorkItemID] = true
		if p.Author != "Dev A" || p.Description == "" {
			t.Fatalf("pick fields: %+v", p)
		}
	}
	if len(picks) != 2 || !got[other] || !got[movedOut] {
		t.Fatalf("want the outside-top-3 pick and the moved fallback, got %+v", picks)
	}
	if later, _ := GetOtherPicks(db, time.Now().Add(time.Hour)); len(later) != 0 {
		t.Fatalf("picks before since are skipped: %+v", later)
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
	ans, _, err := LatestSectionChoice(db, waiting)
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

func TestStructureProposalsDecideOnceCooldownAndExpiry(t *testing.T) {
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
	pending, _ := InsertProposal(db, Proposal{Kind: "new", Key: "new:1:infra", PayloadJSON: `{"item_ids":[1,2]}`})
	keys := func(since time.Time) map[string]bool {
		t.Helper()
		ps, err := BlockedProposals(db, since)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, p := range ps {
			out[p.Key] = true
		}
		return out
	}
	if k := keys(time.Now().AddDate(0, 0, -56)); !k["retire:7"] || !k["new:1:infra"] {
		t.Fatalf("rejected and pending proposals are blocked: %v", k)
	}
	if k := keys(time.Now().Add(time.Hour)); k["retire:7"] || !k["new:1:infra"] {
		t.Fatalf("a rejection expires after the cooldown, a pending proposal does not: %v", k)
	}

	// Pending past its age limit expires: no longer blocking, and no longer decidable.
	if n, err := ExpireProposals(db, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("a fresh proposal does not expire: %d %v", n, err)
	}
	if n, err := ExpireProposals(db, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("an old pending proposal expires: %d %v", n, err)
	}
	if p, _ := GetProposal(db, pending); p.State != ProposalExpired {
		t.Fatalf("state: %+v", p)
	}
	if k := keys(time.Now().AddDate(0, 0, -56)); k["new:1:infra"] {
		t.Fatalf("an expired proposal may be proposed again: %v", k)
	}
	if won, _ := DecideProposal(db, pending, ProposalApproved, "U1"); won {
		t.Fatal("an expired proposal cannot be approved")
	}

	// A stale proposal (refused at approval) does not block either.
	stale, _ := InsertProposal(db, Proposal{Kind: "retire", Key: "retire:9", PayloadJSON: `{}`})
	if won, _ := DecideProposal(db, stale, ProposalStale, "U1"); !won {
		t.Fatal("a pending proposal can be marked stale")
	}
	if k := keys(time.Now().AddDate(0, 0, -56)); k["retire:9"] {
		t.Fatalf("a stale proposal may be proposed again: %v", k)
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

func TestSectionChoiceIsStaleAfterATaxonomyChange(t *testing.T) {
	db := newTestDB(t)
	ids := seedSmallTaxonomy(t, db)
	a, b, infra := ids[SeedKey("Product A", "Release", "7.6.3")], ids[SeedKey("Product A", "Release", "7.4.7")], ids[SeedKey("Product A", "Infrastructure", "")]
	id := asked(t, db, "unsure", a, b, infra)
	if _, current, err := LatestSectionChoice(db, id); err != nil || !current {
		t.Fatalf("an answer after the last Taxonomy change is current: %v %v", current, err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := RetireTaxonomyNode(db, a); err != nil {
		t.Fatal(err)
	}
	if _, current, _ := LatestSectionChoice(db, id); current {
		t.Fatal("an answer from before a Taxonomy change is stale until the item is re-asked")
	}
	time.Sleep(5 * time.Millisecond)
	if err := InsertJudgments(db, []Judgment{{WorkItemID: id, Kind: "section_choice", Model: "m", AnswerJSON: choiceJSON(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, current, _ := LatestSectionChoice(db, id); !current {
		t.Fatal("re-asking makes the answer current again")
	}
}
