// Package learning runs the continuous learning loops: Learned Notes behind a replay gate,
// Structure Proposals, and the Second Opinion on unsure Placements.
package learning

import (
	"fmt"
	"sort"
)

// Replay gate rules.
const (
	MinReplayItems     = 60
	MinGainPoints      = 2 // overall accuracy must rise by at least this many percentage points
	MinSectionItems    = 5 // a Section with this many replay items may not lose accuracy
	MinSignalPeople    = 2 // a Section is tuned only on signals from this many distinct people
	ReplayWindowWeeks  = 8
	maxSectionsPerRun  = 3
	examplesPerSection = 4
)

// ReplayResult is the decision model's Section pick for one person-decided Work Item.
type ReplayResult struct {
	ItemID int64
	Truth  int64 // the Section the person chose
	Pick   int64 // 0 when the model picked no active Section
}

// SectionDelta compares one Section's replay accuracy, by the person's Section.
type SectionDelta struct {
	Items                  int
	OldCorrect, NewCorrect int
}

// GateResult is the replay gate's verdict on candidate notes.
type GateResult struct {
	Items                  int
	OldCorrect, NewCorrect int
	PerSection             map[int64]SectionDelta
	Apply                  bool
	Reasons                []string
}

func (g GateResult) OldAccuracy() float64 { return ratio(g.OldCorrect, g.Items) }
func (g GateResult) NewAccuracy() float64 { return ratio(g.NewCorrect, g.Items) }

// GainPoints is the change in overall accuracy in percentage points.
func (g GateResult) GainPoints() float64 { return 100 * (g.NewAccuracy() - g.OldAccuracy()) }

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// Gate compares replays with the current (old) and candidate notes, paired by Work Item.
// It applies only on a gain of MinGainPoints overall with no Section of MinSectionItems losing.
func Gate(old, cand []ReplayResult) GateResult {
	g := GateResult{PerSection: map[int64]SectionDelta{}}
	byItem := make(map[int64]ReplayResult, len(old))
	for _, r := range old {
		byItem[r.ItemID] = r
	}
	for _, n := range cand {
		o, ok := byItem[n.ItemID]
		if !ok {
			continue
		}
		d := g.PerSection[n.Truth]
		d.Items++
		g.Items++
		if o.Pick == o.Truth {
			d.OldCorrect++
			g.OldCorrect++
		}
		if n.Pick == n.Truth {
			d.NewCorrect++
			g.NewCorrect++
		}
		g.PerSection[n.Truth] = d
	}
	if g.Items < MinReplayItems {
		g.Reasons = append(g.Reasons, fmt.Sprintf("only %d replay Work Items (need %d)", g.Items, MinReplayItems))
	}
	if (g.NewCorrect-g.OldCorrect)*100 < MinGainPoints*g.Items {
		g.Reasons = append(g.Reasons, fmt.Sprintf("accuracy %+.1f pts is below +%d", g.GainPoints(), MinGainPoints))
	}
	ids := make([]int64, 0, len(g.PerSection))
	for id := range g.PerSection {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if d := g.PerSection[id]; d.Items >= MinSectionItems && d.NewCorrect < d.OldCorrect {
			g.Reasons = append(g.Reasons, fmt.Sprintf("Section %d loses accuracy (%d → %d of %d)", id, d.OldCorrect, d.NewCorrect, d.Items))
		}
	}
	g.Apply = len(g.Reasons) == 0
	return g
}
