package learning

import (
	"strings"
	"testing"

	"reportbot/internal/storage/sqlite"
)

// results makes n replay results for truth Section sec, the first correct ones right.
func results(start int64, sec int64, n, correct int) []ReplayResult {
	out := make([]ReplayResult, n)
	for i := range out {
		pick := sec
		if i >= correct {
			pick = 999
		}
		out[i] = ReplayResult{ItemID: start + int64(i), Truth: sec, Pick: pick}
	}
	return out
}

func TestGateAppliesOnTwoPointGainWithoutSectionLoss(t *testing.T) {
	old := append(results(0, 1, 50, 30), results(100, 2, 50, 30)...)
	cand := append(results(0, 1, 50, 32), results(100, 2, 50, 30)...)
	g := Gate(old, cand)
	if !g.Apply || g.Items != 100 || g.OldCorrect != 60 || g.NewCorrect != 62 {
		t.Fatalf("+2 points on 100 Work Items must apply: %+v", g)
	}
	if d := g.PerSection[1]; d.Items != 50 || d.OldCorrect != 30 || d.NewCorrect != 32 {
		t.Fatalf("per-Section numbers: %+v", d)
	}
}

func TestGateDiscardsSmallGain(t *testing.T) {
	old := append(results(0, 1, 50, 30), results(100, 2, 50, 30)...)
	cand := append(results(0, 1, 50, 31), results(100, 2, 50, 30)...)
	g := Gate(old, cand)
	if g.Apply || !strings.Contains(strings.Join(g.Reasons, ";"), "below") {
		t.Fatalf("+1 point must be discarded with a reason: %+v", g)
	}
}

func TestGateDiscardsWhenASectionWithFiveItemsLoses(t *testing.T) {
	old := append(append(results(0, 1, 50, 30), results(100, 2, 45, 20)...), results(200, 3, 5, 4)...)
	cand := append(append(results(0, 1, 50, 40), results(100, 2, 45, 20)...), results(200, 3, 5, 3)...)
	g := Gate(old, cand)
	if g.Apply || !strings.Contains(strings.Join(g.Reasons, ";"), "Section 3") {
		t.Fatalf("a Section with 5 replay items losing accuracy must block: %+v", g)
	}
	// Fewer than 5 replay items in the losing Section does not block.
	old = append(append(results(0, 1, 50, 30), results(100, 2, 46, 20)...), results(200, 3, 4, 4)...)
	cand = append(append(results(0, 1, 50, 40), results(100, 2, 46, 20)...), results(200, 3, 4, 3)...)
	if g := Gate(old, cand); !g.Apply {
		t.Fatalf("a Section with 4 replay items does not block: %+v", g)
	}
}

func TestGateNeedsEnoughReplayItems(t *testing.T) {
	g := Gate(results(0, 1, 59, 10), results(0, 1, 59, 50))
	if g.Apply || !strings.Contains(strings.Join(g.Reasons, ";"), "60") {
		t.Fatalf("fewer than 60 replay items must not apply: %+v", g)
	}
}

func TestGatePairsByWorkItem(t *testing.T) {
	old := results(0, 1, 70, 35)
	cand := results(0, 1, 70, 50)
	cand = append(cand, ReplayResult{ItemID: 5000, Truth: 1, Pick: 1}) // no old answer: not compared
	if g := Gate(old, cand); g.Items != 70 {
		t.Fatalf("only Work Items replayed both ways count: %+v", g)
	}
}

func TestDistinctPeopleCountsSlackIDsOnly(t *testing.T) {
	sig := func(person, author string) sqlite.LearningSignal {
		return sqlite.LearningSignal{Person: person, Author: author}
	}
	if n := DistinctPeople([]sqlite.LearningSignal{sig("", "Name One"), sig("", "Name Two")}); n != 0 {
		t.Fatalf("names without IDs are no people, got %d", n)
	}
	if n := DistinctPeople([]sqlite.LearningSignal{sig("U1", "A"), sig("U1", "B"), sig("U1", "C")}); n != 1 {
		t.Fatalf("one ID is one person, got %d", n)
	}
	if n := DistinctPeople([]sqlite.LearningSignal{sig("U1", "A"), sig("U2", "A"), sig("", "Z")}); n != 2 {
		t.Fatalf("two IDs are two people, got %d", n)
	}
}
