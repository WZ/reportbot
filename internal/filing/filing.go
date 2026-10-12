// Package filing files Work Items into the Taxonomy as they arrive: one decision-model
// request picks the Section and answers the narrow questions code cannot decide.
package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/integrations/decision"
	"reportbot/internal/similar"
	"reportbot/internal/storage/sqlite"
)

const (
	placedIndexLimit   = 5000
	updateAuthorTopK   = 3
	updateTeamTopK     = 2
	updateThreshold    = 0.5
	choiceButtons      = 3
	defaultSimilarSize = 30
)

// Asker sends one decision-model request (*decision.Client in production).
type Asker interface {
	Ask(ctx context.Context, state any, questions map[string]decision.Question) (*decision.Response, error)
}

// Why a Work Item is being filed.
const (
	KindArrival    = "arrival"     // a new /report
	KindEdit       = "edit"        // its text changed
	KindCodeChange = "code_change" // a newly fetched Code Change
	KindRetry      = "retry"       // the sweep, after a failure or a stall
)

// Payload travels with a filing job: where to reply and why the item is being filed.
type Payload struct {
	Kind        string `json:"kind"`
	ResponseURL string `json:"response_url,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	Replace     bool   `json:"replace,omitempty"` // the reply replaces the "Saved" message
}

func (p Payload) JSON() string {
	b, _ := json.Marshal(p)
	return string(b)
}

func ParsePayload(s string) Payload {
	var p Payload
	_ = json.Unmarshal([]byte(s), &p)
	return p
}

// Result is what the reporter is told after a filing pass.
type Result struct {
	Item    domain.WorkItem
	Section *domain.Section // nil when Undetermined
	Payload Payload
	// Choices for "Where does this go?": the decision model's most likely Sections.
	Choices []domain.Section
	// Kept is the Section an edited item stays in while the reporter decides.
	Kept   *domain.Section
	Update *Candidate // likely Update target, the reporter confirms
	Link   *Candidate // likely Work Item for a Code Change, the author confirms
	Failed bool       // a model was unreachable after retries
}

// Notifier tells people what filing decided (Slack in production).
type Notifier interface {
	Filed(r Result)
	PlacementOverridden(item domain.WorkItem, eventID int64, from, to domain.Section)
	// FiledByFallback: nobody answered "Where does this go?" in time (or before publishing).
	FiledByFallback(item domain.WorkItem, s domain.Section, atPublish bool)
}

type Filer struct {
	DB       *sql.DB
	Cfg      config.Config
	Decision Asker
	Notify   Notifier
	Now      func() time.Time
}

func (f *Filer) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Filer) similarCount() int {
	if f.Cfg.SimilarItemCount > 0 {
		return f.Cfg.SimilarItemCount
	}
	return defaultSimilarSize
}

// context is everything a filing pass needs that does not depend on the item.
type filingContext struct {
	tax      domain.Taxonomy
	sections []domain.Section
	active   map[int64]domain.Section
	index    *similar.Index
	placed   []domain.PlacedItem
}

func (f *Filer) loadContext() (*filingContext, error) {
	tax, err := sqlite.LoadTaxonomy(f.DB)
	if err != nil {
		return nil, err
	}
	c := &filingContext{tax: tax, sections: tax.Sections(), active: map[int64]domain.Section{}}
	for _, s := range c.sections {
		c.active[s.ID] = s
	}
	placed, err := sqlite.GetPlacedItems(f.DB, placedIndexLimit)
	if err != nil {
		return nil, err
	}
	for _, p := range placed {
		if _, ok := c.active[p.SectionID]; ok {
			c.placed = append(c.placed, p)
		}
	}
	c.index = similar.NewIndex(c.placed)
	return c, nil
}

// similarLines returns the k most similar placed Work Items with their Sections, one per text.
func (c *filingContext) similarLines(item domain.WorkItem, k int) []FiledLine {
	seen := map[string]bool{}
	var out []FiledLine
	for _, p := range c.index.TopK(item.Description, 4*k, func(p domain.PlacedItem) bool { return p.WorkItemID != item.ID }) {
		line := p.Author + ": " + p.Description
		if seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, FiledLine{Item: line, FiledUnder: c.active[p.SectionID].Path()})
		if len(out) == k {
			break
		}
	}
	return out
}

// sectionAnswer reads the Section Choice: the pick, its confidence and the top Sections.
func (c *filingContext) sectionAnswer(a decision.Answer) (pick int64, confidence float64, top []domain.Section) {
	if id, ok := domain.ParseSectionKey(a.Choice); ok {
		if _, active := c.active[id]; active {
			pick = id
		}
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
	for _, k := range keys {
		if id, ok := domain.ParseSectionKey(k); ok {
			if s, active := c.active[id]; active && len(top) < choiceButtons {
				top = append(top, s)
			}
		}
	}
	return pick, a.Confidence, top
}

func judgmentJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// FileItem runs one filing pass. An error means a model was unreachable or the DB failed;
// the job is retried.
func (f *Filer) FileItem(ctx context.Context, itemID int64, payload Payload) error {
	item, err := sqlite.GetWorkItemByID(f.DB, itemID)
	if err == sql.ErrNoRows {
		return nil // deleted (e.g. merged as an Update) before filing
	}
	if err != nil {
		return err
	}
	c, err := f.loadContext()
	if err != nil {
		return err
	}
	if len(c.sections) == 0 {
		out := sqlite.FilingOutcome{Placement: sqlite.Placement{PlacedBy: domain.PlacedByModel, FilingState: domain.FilingFiled,
			Reason: domain.ReasonNoTaxonomy, HistoryNote: sqlite.ReasonArrival}}
		if err := sqlite.ApplyFiling(f.DB, itemID, out); err != nil {
			return err
		}
		item, _ = sqlite.GetWorkItemByID(f.DB, itemID)
		f.notify(Result{Item: item, Payload: payload})
		return nil
	}

	in := RequestInput{Item: item, Sections: c.sections, Similar: c.similarLines(item, f.similarCount())}
	if item.Status == "" {
		in.AskStatus, in.StatusText = true, item.StatusNote
	}
	if item.TicketIDs == "" && item.Source == "slack" {
		if _, cands := FindTickets(item.Description); len(cands) > 0 {
			in.Tickets = cands
		}
	}
	// Update and Link candidates are for a new Work Item's first filing only.
	first := payload.Kind == KindArrival || payload.Kind == KindCodeChange
	if payload.Kind == KindRetry {
		filed, err := sqlite.HasBeenFiled(f.DB, item.ID)
		if err != nil {
			return err
		}
		first = !filed
	}
	var codeChange *domain.CodeChange
	switch item.Source {
	case "slack":
		if first {
			in.Updates, err = f.updateCandidates(c, item)
		}
	case "gitlab", "github":
		if first {
			if cc, ok := f.ownCodeChange(item.ID); ok {
				codeChange = &cc
				in.Links, err = f.updateCandidates(c, item)
			}
		}
	}
	if err != nil {
		return err
	}

	state, qs := BuildRequest(in)
	ans, err := f.Decision.Ask(ctx, state, qs)
	if err != nil {
		return fmt.Errorf("decision model: %w", err)
	}

	out := sqlite.FilingOutcome{Judgments: judgments(ans)}
	pick, conf, top := c.sectionAnswer(ans.Answers[QSection])
	out.Placement = sqlite.Placement{PlacedBy: domain.PlacedByModel, ModelSectionID: pick, Confidence: &conf, HistoryNote: sqlite.ReasonArrival}
	result := Result{Payload: payload}
	if pick != 0 && conf >= f.Cfg.DecisionModelConfidence {
		out.Placement.SectionID, out.Placement.FilingState = pick, domain.FilingFiled
		s := c.active[pick]
		result.Section = &s
	} else {
		out.Placement.FilingState, out.Placement.Reason = domain.FilingAsking, domain.ReasonLowConfidence
		result.Choices = top
		if cur, ok := c.active[item.SectionID]; ok && !first {
			// An unsure re-file keeps the Section the item has, and still asks the reporter.
			out.Placement = sqlite.Placement{SectionID: cur.ID, PlacedBy: item.PlacedBy, Confidence: item.Confidence,
				ModelSectionID: pick, FilingState: domain.FilingFiled, HistoryNote: sqlite.ReasonArrival}
			result.Kept = &cur
		}
	}
	if in.AskStatus {
		if st, ok := StatusFromChoice(ans.Answers[QStatus].Choice); ok {
			out.Status, out.StatusNote = st, item.StatusNote
		}
	}
	if len(in.Tickets) > 0 {
		out.TicketIDs = TicketFromChoice(ans.Answers[QTicket].Choice, in.Tickets)
	}
	result.Update = bestCandidate(ans, in.Updates, UpdateQuestionID)
	result.Link = bestCandidate(ans, in.Links, LinkQuestionID)

	if err := sqlite.ApplyFiling(f.DB, itemID, out); err != nil {
		return err
	}
	if codeChange != nil && result.Link != nil {
		if err := sqlite.SetCodeChangeSuggestion(f.DB, codeChange.ID, result.Link.Item.ID); err != nil {
			return err
		}
	}
	if result.Item, err = sqlite.GetWorkItemByID(f.DB, itemID); err != nil {
		return err
	}
	f.notify(result)
	return nil
}

// judgments keeps every raw answer of a request; usage is recorded once.
func judgments(ans *decision.Response) []sqlite.Judgment {
	ids := make([]string, 0, len(ans.Answers))
	for id := range ans.Answers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]sqlite.Judgment, 0, len(ids))
	for i, id := range ids {
		j := sqlite.Judgment{Kind: judgmentKind(id), Subject: id, AnswerJSON: string(ans.Answers[id].Raw),
			Model: ans.Model, LatencyMS: ans.Latency.Milliseconds()}
		if i == 0 {
			j.InputTokens, j.OutputTokens = ans.Usage.InputTokens, ans.Usage.OutputTokens
		}
		out = append(out, j)
	}
	return out
}

func judgmentKind(questionID string) string {
	switch {
	case questionID == QSection:
		return "section_choice"
	case questionID == QStatus, questionID == QTicket:
		return questionID
	case strings.HasPrefix(questionID, qUpdatePrefix):
		return "update_candidate"
	case strings.HasPrefix(questionID, qLinkPrefix):
		return "link_candidate"
	}
	return questionID
}

func bestCandidate(ans *decision.Response, cands []Candidate, qid func(int64) string) *Candidate {
	if ans == nil {
		return nil
	}
	var best *Candidate
	bestP := updateThreshold
	for i := range cands {
		if p := ans.Answers[qid(cands[i].Item.ID)].Noul; p >= bestP {
			best, bestP = &cands[i], p
		}
	}
	return best
}

func (f *Filer) ownCodeChange(itemID int64) (domain.CodeChange, bool) {
	ccs, err := sqlite.GetCodeChangesForItem(f.DB, itemID)
	if err != nil {
		return domain.CodeChange{}, false
	}
	for _, cc := range ccs {
		if cc.LinkKind == domain.LinkOwn {
			return cc, true
		}
	}
	return domain.CodeChange{}, false
}

// updateCandidates proposes Open Work Items the new item may be about: exact ticket
// overlap, then the most similar of the same author's and the team's open items.
func (f *Filer) updateCandidates(c *filingContext, item domain.WorkItem) ([]Candidate, error) {
	open, err := sqlite.GetOpenItems(f.DB)
	if err != nil {
		return nil, err
	}
	byID := map[int64]domain.WorkItem{}
	var mine, team []domain.PlacedItem
	for _, o := range open {
		if o.ID == item.ID || o.FilingState == domain.FilingPending {
			continue
		}
		byID[o.ID] = o
		p := domain.PlacedItem{WorkItemID: o.ID, Description: o.Description, Author: o.Author, SectionID: o.SectionID}
		if sameAuthor(o, item) {
			mine = append(mine, p)
		} else {
			team = append(team, p)
		}
	}
	var ids []int64
	seen := map[int64]bool{}
	addID := func(id int64) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	certain, _ := FindTickets(item.Description)
	tickets := append(certain, splitTickets(item.TicketIDs)...)
	for _, o := range open {
		if _, ok := byID[o.ID]; ok && ticketsOverlap(tickets, splitTickets(o.TicketIDs)) {
			addID(o.ID)
		}
	}
	for _, p := range similar.NewIndex(mine).TopK(item.Description, updateAuthorTopK, nil) {
		addID(p.WorkItemID)
	}
	for _, p := range similar.NewIndex(team).TopK(item.Description, updateTeamTopK, nil) {
		addID(p.WorkItemID)
	}
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		o := byID[id]
		path := "Undetermined"
		if s, ok := c.tax.SectionAny(o.SectionID); ok {
			path = s.Path()
		}
		out = append(out, Candidate{Item: o, SectionPath: path})
	}
	return out, nil
}

func sameAuthor(a, b domain.WorkItem) bool {
	if a.AuthorID != "" && b.AuthorID != "" {
		return a.AuthorID == b.AuthorID
	}
	return strings.EqualFold(strings.TrimSpace(a.Author), strings.TrimSpace(b.Author))
}

func splitTickets(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func ticketsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// MarkFailed files an item as Undetermined because a model stayed unreachable.
func (f *Filer) MarkFailed(itemID int64, payload Payload) error {
	out := sqlite.FilingOutcome{Placement: sqlite.Placement{PlacedBy: domain.PlacedByModel, FilingState: domain.FilingFailed,
		Reason: domain.ReasonModelUnreachable, HistoryNote: sqlite.ReasonArrival}}
	item, err := sqlite.GetWorkItemByID(f.DB, itemID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if item.SectionID != 0 { // keep a Section it already has (e.g. a re-file after an edit)
		out.Placement.SectionID, out.Placement.PlacedBy, out.Placement.Confidence = item.SectionID, item.PlacedBy, item.Confidence
		out.Placement.Reason = ""
	}
	if err := sqlite.ApplyFiling(f.DB, itemID, out); err != nil {
		return err
	}
	// Tell the author once per Work Item, not on every sweep retry.
	first, err := sqlite.MarkFailureNotified(f.DB, itemID)
	if err != nil || !first {
		return err
	}
	item, err = sqlite.GetWorkItemByID(f.DB, itemID)
	if err != nil {
		return err
	}
	f.notify(Result{Item: item, Payload: payload, Failed: true})
	return nil
}

func (f *Filer) notify(r Result) {
	if f.Notify == nil {
		return
	}
	if r.Section == nil && r.Kept == nil && len(r.Choices) == 0 && r.Item.SectionID != 0 {
		if tax, err := sqlite.LoadTaxonomy(f.DB); err == nil {
			if s, ok := tax.SectionAny(r.Item.SectionID); ok {
				r.Section = &s
			}
		}
	}
	f.Notify.Filed(r)
}
