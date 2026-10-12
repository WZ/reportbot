package proposals

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
	ProposalCooldownWeeks = 8   // a rejected change is not proposed again for this long
	ProposalExpiryWeeks   = 2   // an undecided proposal expires after this long and may be proposed again
	proposalWindowWeeks   = 4   // recent window for themes and Section shares
	RetireWindowWeeks     = 8   // a Section unused this long, with no Open Work Items, may be retired
	largeShare            = 0.2 // a split candidate holds at least this share of recent Work Items
	largeMinItems         = 10
	maxLargeSections      = 2
	maxItemsPerLarge      = 30
	maxUnplaced           = 80
)

var (
	ErrAlreadyDecided = errors.New("this proposal was already decided or has expired")
	ErrProposalStale  = errors.New("this proposal no longer holds")
)

// sameEvidence is the share of a new Section's evidence that, shared with a blocked one under
// the same parent, makes it the same change however it is worded.
const sameEvidence = 0.5

// StoredProposal is a Structure Proposal saved for managers to approve or reject.
type StoredProposal struct {
	ID       int64
	Affected int // Work Items it would move or re-file
	llm.StructureProposal
}

// ProposeStructure asks the LLM for 0–3 structure changes, drops ones pending or decided within
// the cooldown, stores the rest and sends them to managers.
func (l *Service) ProposeStructure(ctx context.Context) ([]StoredProposal, error) {
	if !l.Cfg.LLMConfigured() {
		return nil, ErrNoLLM
	}
	now := l.now()
	if n, err := sqlite.ExpireProposals(l.DB, now.AddDate(0, 0, -7*ProposalExpiryWeeks)); err != nil {
		return nil, err
	} else if n > 0 {
		log.Printf("Structure Proposals: %d expired undecided", n)
	}
	tax, err := sqlite.LoadTaxonomy(l.DB)
	if err != nil {
		return nil, err
	}
	in, activity, authors, err := l.proposalInput(tax)
	if err != nil {
		return nil, err
	}
	if len(in.Unplaced) < 5 && len(in.Large) == 0 && len(in.Retirable) == 0 {
		log.Printf("Structure Proposals: nothing to propose from")
		return nil, nil
	}
	system, user := llm.BuildProposalPrompt(in)
	text, err := l.complete(ctx, system, user)
	if err != nil {
		return nil, fmt.Errorf("Structure Proposals: %w", err)
	}
	parsed, err := llm.ParseProposals(text, in)
	if err != nil {
		return nil, err
	}
	blocked, err := sqlite.BlockedProposals(l.DB, now.AddDate(0, 0, -7*ProposalCooldownWeeks))
	if err != nil {
		return nil, err
	}
	parents := map[int64]bool{}
	for _, p := range in.Parents {
		parents[p.ID] = true
	}
	known := TaxonomyNames(tax.Sections())
	var out []StoredProposal
	for _, p := range FilterBlocked(parsed, blocked) {
		if err := ValidateProposal(p, known, parents, authors); err != nil {
			log.Printf("Structure Proposal %s dropped: invalid: %v", llm.ProposalKey(p), err)
			continue
		}
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
	log.Printf("Structure Proposals: parsed=%d new=%d", len(parsed), len(out))
	if len(out) > 0 && l.Notify != nil {
		l.Notify.ProposalsReady(out)
	}
	return out, nil
}

// FilterBlocked drops proposals whose change is pending or was decided within the cooldown: the
// same key, or a new Section under the same parent with mostly the same evidence.
func FilterBlocked(ps []llm.StructureProposal, blocked []sqlite.Proposal) []llm.StructureProposal {
	keys := map[string]bool{}
	var blockedNew []llm.StructureProposal
	for _, b := range blocked {
		keys[b.Key] = true
		var p llm.StructureProposal
		if b.Kind == llm.ProposeNew && json.Unmarshal([]byte(b.PayloadJSON), &p) == nil {
			blockedNew = append(blockedNew, p)
		}
	}
	var out []llm.StructureProposal
	for _, p := range ps {
		if keys[llm.ProposalKey(p)] {
			continue
		}
		if p.Kind == llm.ProposeNew && sharesEvidence(p, blockedNew) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func sharesEvidence(p llm.StructureProposal, blocked []llm.StructureProposal) bool {
	for _, b := range blocked {
		if b.ParentID != p.ParentID || len(p.ItemIDs) == 0 {
			continue
		}
		had := map[int64]bool{}
		for _, id := range b.ItemIDs {
			had[id] = true
		}
		shared := 0
		for _, id := range p.ItemIDs {
			if had[id] {
				shared++
			}
		}
		if float64(shared) >= sameEvidence*float64(len(p.ItemIDs)) {
			return true
		}
	}
	return false
}

// proposalInput gathers the evidence and the authors of the Work Items in it.
func (l *Service) proposalInput(tax domain.Taxonomy) (llm.ProposalInput, map[int64]sqlite.SectionActivity, []string, error) {
	now := l.now()
	recent := now.AddDate(0, 0, -7*proposalWindowWeeks)
	activity, err := sqlite.GetSectionActivity(l.DB, recent, now.AddDate(0, 0, -7*RetireWindowWeeks))
	if err != nil {
		return llm.ProposalInput{}, nil, nil, err
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
	var authors []string
	addItem := func(id int64, desc, author string) {
		if !seen[id] && len(in.Unplaced) < maxUnplaced {
			seen[id] = true
			in.Unplaced = append(in.Unplaced, llm.ProposalItem{ID: id, Description: desc})
			authors = append(authors, author)
		}
	}
	open, err := sqlite.GetOpenItems(l.DB)
	if err != nil {
		return in, nil, nil, err
	}
	for _, it := range open {
		if it.SectionID == 0 && it.FilingState != domain.FilingPending {
			addItem(it.ID, it.Description, it.Author)
		}
	}
	windowItems, err := sqlite.GetItemsByDateRange(l.DB, recent, now.Add(time.Second))
	if err != nil {
		return in, nil, nil, err
	}
	for _, it := range windowItems {
		if it.SectionID == 0 && it.FilingState != domain.FilingPending {
			addItem(it.ID, it.Description, it.Author)
		}
	}
	picks, err := sqlite.GetOtherPicks(l.DB, recent)
	if err != nil {
		return in, nil, nil, err
	}
	for _, p := range picks {
		addItem(p.WorkItemID, p.Description, p.Author)
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
				authors = append(authors, it.Author)
			}
		}
		in.Large = append(in.Large, li)
	}
	return in, activity, authors, nil
}

// ApproveProposal applies a pending proposal once, through the same Taxonomy edits as the web
// editor, and queues re-filing of Open Work Items.
func (l *Service) ApproveProposal(id int64, actorID string) (StoredProposal, error) {
	if sp, err := l.loadProposal(id); err == nil && sp.Kind == llm.ProposeRetire {
		if open, err := l.openItemsIn(sp.SectionID); err != nil {
			return sp, err
		} else if open > 0 {
			if won, err := sqlite.DecideProposal(l.DB, id, sqlite.ProposalStale, actorID); err != nil {
				return sp, err
			} else if !won {
				return sp, ErrAlreadyDecided
			}
			log.Printf("Structure Proposal %d stale: Section %d has %d Open Work Items", id, sp.SectionID, open)
			return sp, ErrProposalStale
		}
	}
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
			log.Printf("Structure Proposal %d reopen: %v", id, e)
		}
		return sp, err
	}
	if err := sqlite.EnqueueReclassify(l.DB); err != nil {
		return sp, err
	}
	if l.Wake != nil {
		l.Wake()
	}
	log.Printf("Structure Proposal %d approved kind=%s by=%s", id, sp.Kind, actorID)
	return sp, nil
}

// RejectProposal stores the rejection; the same change is not proposed again for the cooldown.
func (l *Service) RejectProposal(id int64, actorID string) error {
	won, err := sqlite.DecideProposal(l.DB, id, sqlite.ProposalRejected, actorID)
	if err != nil {
		return err
	}
	if !won {
		return ErrAlreadyDecided
	}
	log.Printf("Structure Proposal %d rejected by=%s", id, actorID)
	return nil
}

// openItemsIn counts the Open Work Items filed in a Section now.
func (l *Service) openItemsIn(sectionID int64) (int, error) {
	n := 0
	open, err := sqlite.GetOpenItems(l.DB)
	for _, it := range open {
		if it.SectionID == sectionID {
			n++
		}
	}
	return n, err
}

func (l *Service) loadProposal(id int64) (StoredProposal, error) {
	p, err := sqlite.GetProposal(l.DB, id)
	if err != nil {
		return StoredProposal{}, err
	}
	sp := StoredProposal{ID: p.ID, Affected: p.Affected}
	err = json.Unmarshal([]byte(p.PayloadJSON), &sp.StructureProposal)
	return sp, err
}

func (l *Service) applyProposal(p llm.StructureProposal) error {
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
