package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/integrations/llm"
	"reportbot/internal/storage/sqlite"
)

// Structure Proposal rules.
const (
	ProposalCooldownWeeks = 8 // a rejected change is not proposed again for this long
	proposalWindowWeeks   = 4 // recent window for themes and Section shares
	retireWindowWeeks     = 8
	largeShare            = 0.2 // a split candidate holds at least this share of recent Work Items
	largeMinItems         = 10
	maxLargeSections      = 2
	maxItemsPerLarge      = 30
	maxUnplaced           = 80
)

var ErrAlreadyDecided = errors.New("this proposal was already decided")

// StoredProposal is a Structure Proposal saved for managers to approve or reject.
type StoredProposal struct {
	ID       int64
	Affected int // Work Items it would move or re-file
	llm.StructureProposal
}

// ProposeStructure asks the LLM for 0–3 structure changes, drops ones pending or decided within
// the cooldown, stores the rest and sends them to managers.
func (l *Learner) ProposeStructure(ctx context.Context) ([]StoredProposal, error) {
	if !l.Cfg.LLMConfigured() {
		return nil, ErrNoLLM
	}
	now := l.now()
	tax, err := sqlite.LoadTaxonomy(l.DB)
	if err != nil {
		return nil, err
	}
	in, activity, err := l.proposalInput(tax)
	if err != nil {
		return nil, err
	}
	if len(in.Unplaced) < 5 && len(in.Large) == 0 && len(in.Retirable) == 0 {
		log.Printf("learning proposals: nothing to propose from")
		return nil, nil
	}
	system, user := llm.BuildProposalPrompt(in)
	text, err := l.complete(ctx, system, user)
	if err != nil {
		return nil, fmt.Errorf("structure proposals: %w", err)
	}
	parsed, err := llm.ParseProposals(text, in)
	if err != nil {
		return nil, err
	}
	blocked, err := sqlite.BlockedProposalKeys(l.DB, now.AddDate(0, 0, -7*ProposalCooldownWeeks))
	if err != nil {
		return nil, err
	}
	var out []StoredProposal
	for _, p := range FilterBlocked(parsed, blocked) {
		sp := StoredProposal{StructureProposal: p}
		switch p.Kind {
		case llm.ProposeNew:
			sp.Affected = len(p.ItemIDs)
		case llm.ProposeSplit:
			sp.Affected = activity[p.SectionID].Open
		}
		payload, _ := json.Marshal(p)
		if sp.ID, err = sqlite.InsertProposal(l.DB, sqlite.Proposal{Kind: p.Kind, Key: llm.ProposalKey(p), PayloadJSON: string(payload), Affected: sp.Affected}); err != nil {
			return out, err
		}
		out = append(out, sp)
	}
	log.Printf("learning proposals: parsed=%d new=%d", len(parsed), len(out))
	if len(out) > 0 && l.Notify != nil {
		l.Notify.ProposalsReady(out)
	}
	return out, nil
}

// FilterBlocked drops proposals whose change is pending or was decided within the cooldown.
func FilterBlocked(ps []llm.StructureProposal, blocked map[string]bool) []llm.StructureProposal {
	var out []llm.StructureProposal
	for _, p := range ps {
		if !blocked[llm.ProposalKey(p)] {
			out = append(out, p)
		}
	}
	return out
}

func (l *Learner) proposalInput(tax domain.Taxonomy) (llm.ProposalInput, map[int64]sqlite.SectionActivity, error) {
	now := l.now()
	recent := now.AddDate(0, 0, -7*proposalWindowWeeks)
	activity, err := sqlite.GetSectionActivity(l.DB, recent, now.AddDate(0, 0, -7*retireWindowWeeks))
	if err != nil {
		return llm.ProposalInput{}, nil, err
	}
	in := llm.ProposalInput{Window: fmt.Sprintf("%d weeks", proposalWindowWeeks)}
	total := 0
	for _, s := range tax.Sections() {
		a := activity[s.ID]
		total += a.Recent
		in.Sections = append(in.Sections, llm.ProposalSection{ID: s.ID, Path: s.Path(), Description: s.Description, Recent: a.Recent})
		if a.CreatedBefore8w && a.Recent8w == 0 && a.Open == 0 {
			in.Retirable = append(in.Retirable, llm.ProposalSection{ID: s.ID, Path: s.Path()})
		}
	}
	for _, area := range tax.Areas() {
		in.Parents = append(in.Parents, llm.ProposalParent{ID: area.ID, Path: area.Name, Level: area.Level})
		for _, cat := range tax.Children(area.ID) {
			in.Parents = append(in.Parents, llm.ProposalParent{ID: cat.ID, Path: area.Name + domain.PathSeparator + cat.Name, Level: cat.Level})
		}
	}

	// Unplaced: Undetermined Open Work Items, Undetermined ones from the window, and "Other…" picks.
	seen := map[int64]bool{}
	addItem := func(id int64, desc string) {
		if !seen[id] && len(in.Unplaced) < maxUnplaced {
			seen[id] = true
			in.Unplaced = append(in.Unplaced, llm.ProposalItem{ID: id, Description: desc})
		}
	}
	open, err := sqlite.GetOpenItems(l.DB)
	if err != nil {
		return in, nil, err
	}
	for _, it := range open {
		if it.SectionID == 0 && it.FilingState != domain.FilingPending {
			addItem(it.ID, it.Description)
		}
	}
	windowItems, err := sqlite.GetItemsByDateRange(l.DB, recent, now.Add(time.Second))
	if err != nil {
		return in, nil, err
	}
	for _, it := range windowItems {
		if it.SectionID == 0 && it.FilingState != domain.FilingPending {
			addItem(it.ID, it.Description)
		}
	}
	signals, err := sqlite.GetLearningSignals(l.DB, recent)
	if err != nil {
		return in, nil, err
	}
	for _, s := range signals {
		if s.OutsideTop3 {
			addItem(s.WorkItemID, s.Description)
		}
	}

	// Split candidates: the Sections with the largest share of recent Work Items.
	var large []domain.Section
	for _, s := range tax.Sections() {
		if a := activity[s.ID]; a.Recent >= largeMinItems && total > 0 && float64(a.Recent) >= largeShare*float64(total) {
			large = append(large, s)
		}
	}
	sort.SliceStable(large, func(i, j int) bool { return activity[large[i].ID].Recent > activity[large[j].ID].Recent })
	if len(large) > maxLargeSections {
		large = large[:maxLargeSections]
	}
	for _, s := range large {
		li := llm.ProposalSectionItems{SectionID: s.ID, Path: s.Path()}
		for _, it := range windowItems {
			if it.SectionID == s.ID && len(li.Items) < maxItemsPerLarge {
				li.Items = append(li.Items, llm.ProposalItem{ID: it.ID, Description: it.Description})
			}
		}
		in.Large = append(in.Large, li)
	}
	return in, activity, nil
}

// ApproveProposal applies a pending proposal once, through the same Taxonomy edits as the web
// editor, and queues re-filing of Open Work Items.
func (l *Learner) ApproveProposal(id int64, actorID string) (StoredProposal, error) {
	won, err := sqlite.DecideProposal(l.DB, id, sqlite.ProposalApproved, actorID)
	if err != nil {
		return StoredProposal{}, err
	}
	if !won {
		return StoredProposal{}, ErrAlreadyDecided
	}
	sp, err := l.loadProposal(id)
	if err == nil {
		err = l.applyProposal(sp.StructureProposal)
	}
	if err != nil {
		if e := sqlite.ReopenProposal(l.DB, id); e != nil {
			log.Printf("learning proposal %d reopen: %v", id, e)
		}
		return sp, err
	}
	if err := sqlite.EnqueueReclassify(l.DB); err != nil {
		return sp, err
	}
	if l.Wake != nil {
		l.Wake()
	}
	log.Printf("learning proposal %d approved kind=%s by=%s", id, sp.Kind, actorID)
	return sp, nil
}

// RejectProposal stores the rejection; the same change is not proposed again for the cooldown.
func (l *Learner) RejectProposal(id int64, actorID string) error {
	won, err := sqlite.DecideProposal(l.DB, id, sqlite.ProposalRejected, actorID)
	if err != nil {
		return err
	}
	if !won {
		return ErrAlreadyDecided
	}
	log.Printf("learning proposal %d rejected by=%s", id, actorID)
	return nil
}

func (l *Learner) loadProposal(id int64) (StoredProposal, error) {
	p, err := sqlite.GetProposal(l.DB, id)
	if err != nil {
		return StoredProposal{}, err
	}
	sp := StoredProposal{ID: p.ID, Affected: p.Affected}
	err = json.Unmarshal([]byte(p.PayloadJSON), &sp.StructureProposal)
	return sp, err
}

func (l *Learner) applyProposal(p llm.StructureProposal) error {
	tax, err := sqlite.LoadTaxonomy(l.DB)
	if err != nil {
		return err
	}
	switch p.Kind {
	case llm.ProposeNew:
		parent, ok := tax.Node(p.ParentID)
		if !ok || parent.Retired {
			return fmt.Errorf("the parent of the new Section no longer exists")
		}
		level := domain.LevelSection
		if parent.Level == domain.LevelArea {
			level = domain.LevelCategory
		}
		_, err = sqlite.InsertTaxonomyNode(l.DB, p.ParentID, level, p.Name, p.Description)
		return err
	case llm.ProposeSplit:
		if _, ok := tax.Section(p.SectionID); !ok {
			return fmt.Errorf("the Section to split no longer exists")
		}
		parts := make([]sqlite.NewSection, len(p.NewSections))
		for i, s := range p.NewSections {
			parts[i] = sqlite.NewSection{Name: s.Name, Description: s.Description}
		}
		return sqlite.SplitSection(l.DB, p.SectionID, parts)
	case llm.ProposeRetire:
		if _, ok := tax.Section(p.SectionID); !ok {
			return fmt.Errorf("the Section to retire no longer exists")
		}
		return sqlite.RetireTaxonomyNode(l.DB, p.SectionID)
	}
	return fmt.Errorf("unknown proposal kind %q", p.Kind)
}

// RunNowReport is what /retrospect shows: the notes run and the proposals sent.
type RunNowReport struct {
	Notes     NotesReport
	NotesErr  error
	Proposals []StoredProposal
	PropErr   error
}

// RunNow runs Loop 1 and generates Structure Proposals immediately.
func (l *Learner) RunNow(ctx context.Context) (RunNowReport, error) {
	if !l.Cfg.LLMConfigured() {
		return RunNowReport{}, ErrNoLLM
	}
	var r RunNowReport
	r.Notes, r.NotesErr = l.RunNotes(ctx, TriggerRetrospect)
	r.Proposals, r.PropErr = l.ProposeStructure(ctx)
	return r, nil
}
