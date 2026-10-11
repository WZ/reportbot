package learning

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/filing"
	"reportbot/internal/integrations/decision"
	"reportbot/internal/integrations/llm"
	"reportbot/internal/storage/sqlite"
)

// Why a learning run started.
const (
	TriggerNightly    = "nightly"
	TriggerRetrospect = "retrospect"
)

// signalWindow bounds how far back a Section's new signals reach when it was never considered.
const signalWindow = 7 * 24 * time.Hour

var ErrNoLLM = errors.New("the LLM isn't configured")

// Notifier tells people what the loops did (Slack in production).
type Notifier interface {
	NotesApplied(r NotesReport)
	ProposalsReady(ps []StoredProposal)
	AssistantFiled(item domain.WorkItem, s domain.Section)
}

// Learner runs the learning loops against the live database.
type Learner struct {
	DB     *sql.DB
	Cfg    config.Config
	Filer  *filing.Filer // its Decision client answers replays; its Snapshot is live filing's context
	Notify Notifier
	Wake   func() // starts re-filing after an approved Structure Proposal
	Now    func() time.Time
	// Complete calls the LLM; nil uses the configured provider.
	Complete func(ctx context.Context, system, user string) (string, error)
}

func (l *Learner) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Learner) complete(ctx context.Context, system, user string) (string, error) {
	if l.Complete != nil {
		return l.Complete(ctx, system, user)
	}
	text, _, err := llm.Complete(ctx, l.Cfg, system, user)
	return text, err
}

// NoteOutcome is one Section's candidate Learned Notes in a run.
type NoteOutcome struct {
	Section   domain.Section
	Old, New  string
	HistoryID int64 // set when applied, for Undo
}

// NotesReport is what one Learned Notes run did.
type NotesReport struct {
	RunID      int64
	Trigger    string
	Decision   string // sqlite.RunApplied | RunDiscarded | RunSkipped | RunFailed
	Reason     string
	Signals    int
	Candidates []NoteOutcome
	Invalid    []InvalidNote // drafts rejected before the replay
	Gate       *GateResult
	Applied    bool
}

// InvalidNote is a drafted note that failed ValidateNote.
type InvalidNote struct {
	Section domain.Section
	Notes   string
	Reason  string
}

func invalidReasons(ns []InvalidNote) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = n.Section.Label() + ": " + n.Reason
	}
	return joinReasons(parts)
}

// RunNotes drafts Learned Notes for up to three Sections with new signals and applies them
// only if a replay of recent person decisions passes the Gate.
func (l *Learner) RunNotes(ctx context.Context, trigger string) (NotesReport, error) {
	if !l.Cfg.LLMConfigured() {
		return NotesReport{}, ErrNoLLM
	}
	started := l.now()
	rep := NotesReport{Trigger: trigger}
	run := sqlite.LearningRun{Trigger: trigger, StartedAt: started}
	finish := func(decision, reason string, err error) (NotesReport, error) {
		rep.Decision, rep.Reason = decision, reason
		run.Decision, run.Reason, run.Signals = decision, reason, rep.Signals
		if rep.Gate != nil {
			oldAcc, newAcc := rep.Gate.OldAccuracy(), rep.Gate.NewAccuracy()
			run.ReplayItems, run.OldAccuracy, run.NewAccuracy = rep.Gate.Items, &oldAcc, &newAcc
		}
		if b, e := json.Marshal(candidateLog(rep.Candidates, rep.Invalid)); e == nil {
			run.Candidates = string(b)
		}
		id, e := sqlite.InsertLearningRun(l.DB, run)
		if e != nil && err == nil {
			err = e
		}
		rep.RunID = id
		log.Printf("learning notes run=%d trigger=%s decision=%s signals=%d sections=%v reason=%q", id, trigger, decision, rep.Signals, run.Sections, reason)
		return rep, err
	}

	snap, err := l.Filer.Snapshot()
	if err != nil {
		return finish(sqlite.RunFailed, err.Error(), err)
	}
	signals, sections, withSignals, err := l.newSignals(snap, started)
	if err != nil {
		return finish(sqlite.RunFailed, err.Error(), err)
	}
	rep.Signals = len(signals)
	if withSignals == 0 {
		return finish(sqlite.RunSkipped, "no new signals", nil)
	}
	if len(sections) == 0 {
		return finish(sqlite.RunSkipped, fmt.Sprintf("no Section has new signals from %d people", MinSignalPeople), nil)
	}
	replayItems, err := l.replayItems(snap, started)
	if err != nil {
		return finish(sqlite.RunFailed, err.Error(), err)
	}
	if len(replayItems) < MinReplayItems {
		return finish(sqlite.RunSkipped, fmt.Sprintf("only %d person-decided Work Items in %d weeks (need %d)", len(replayItems), ReplayWindowWeeks, MinReplayItems), nil)
	}
	run.Sections = sectionIDs(sections)

	drafts, err := l.draftNotes(ctx, snap, sections, signals, replayItems)
	if err != nil {
		return finish(sqlite.RunFailed, "drafting: "+err.Error(), err)
	}
	known := TaxonomyNames(snap.Sections())
	var authors []string
	for _, sg := range signals {
		authors = append(authors, sg.Author)
	}
	for _, d := range drafts {
		s, _ := snap.Section(d.SectionID)
		if d.Notes == s.LearnedNotes {
			continue
		}
		if err := ValidateNote(d.Notes, known, authors); err != nil {
			rep.Invalid = append(rep.Invalid, InvalidNote{Section: s, Notes: d.Notes, Reason: err.Error()})
			log.Printf("learning notes section=%d discarded: invalid: %v", s.ID, err)
			continue
		}
		rep.Candidates = append(rep.Candidates, NoteOutcome{Section: s, Old: s.LearnedNotes, New: d.Notes})
	}
	if len(rep.Candidates) == 0 {
		if len(rep.Invalid) > 0 {
			return finish(sqlite.RunDiscarded, "discarded: invalid: "+invalidReasons(rep.Invalid), nil)
		}
		return finish(sqlite.RunDiscarded, "the LLM drafted no new notes", nil)
	}

	current := snap.Sections()
	candidate := withNotes(current, rep.Candidates)
	oldRes, err := l.replay(ctx, snap, replayItems, current)
	if err != nil {
		return finish(sqlite.RunFailed, "replay: "+err.Error(), err)
	}
	newRes, err := l.replay(ctx, snap, replayItems, candidate)
	if err != nil {
		return finish(sqlite.RunFailed, "replay: "+err.Error(), err)
	}
	g := Gate(oldRes, newRes)
	rep.Gate = &g
	if !g.Apply {
		return finish(sqlite.RunDiscarded, joinReasons(g.Reasons), nil)
	}

	rep, err = finish(sqlite.RunApplied, fmt.Sprintf("%+.1f pts on %d Work Items", g.GainPoints(), g.Items), nil)
	if err != nil {
		return rep, err
	}
	changes := make([]sqlite.NoteChange, len(rep.Candidates))
	for i, c := range rep.Candidates {
		changes[i] = sqlite.NoteChange{SectionID: c.Section.ID, Old: c.Old, New: c.New}
	}
	ids, err := sqlite.ApplyLearnedNotes(l.DB, rep.RunID, changes)
	if err != nil {
		_, _ = l.DB.Exec(`UPDATE learning_runs SET decision = ?, reason = ? WHERE id = ?`, sqlite.RunFailed, "apply: "+err.Error(), rep.RunID)
		rep.Decision, rep.Reason = sqlite.RunFailed, err.Error()
		return rep, err
	}
	for i := range rep.Candidates {
		rep.Candidates[i].HistoryID = ids[i]
	}
	rep.Applied = true
	if trigger == TriggerNightly && l.Notify != nil {
		l.Notify.NotesApplied(rep)
	}
	return rep, nil
}

func joinReasons(rs []string) string {
	out := ""
	for i, r := range rs {
		if i > 0 {
			out += "; "
		}
		out += r
	}
	return out
}

type candidateEntry struct {
	SectionID int64  `json:"section_id"`
	Old       string `json:"old,omitempty"`
	New       string `json:"new"`
	Invalid   string `json:"invalid,omitempty"`
}

func candidateLog(cs []NoteOutcome, invalid []InvalidNote) []candidateEntry {
	out := make([]candidateEntry, 0, len(cs)+len(invalid))
	for _, c := range cs {
		out = append(out, candidateEntry{SectionID: c.Section.ID, Old: c.Old, New: c.New})
	}
	for _, n := range invalid {
		out = append(out, candidateEntry{SectionID: n.Section.ID, New: n.Notes, Invalid: n.Reason})
	}
	return out
}

func sectionIDs(ss []domain.Section) []int64 {
	out := make([]int64, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

// newSignals returns the signals not yet considered, the (at most three) active Sections with the
// most of them, and how many Sections had any; a signal counts for both its model and person Section.
func (l *Learner) newSignals(snap *filing.Snapshot, now time.Time) ([]sqlite.LearningSignal, []domain.Section, int, error) {
	floor := now.Add(-signalWindow)
	all, err := sqlite.GetLearningSignals(l.DB, floor)
	if err != nil {
		return nil, nil, 0, err
	}
	marks, err := sqlite.SectionWatermarks(l.DB)
	if err != nil {
		return nil, nil, 0, err
	}
	isNew := func(s sqlite.LearningSignal, sec int64) bool {
		_, active := snap.Section(sec)
		return active && s.CreatedAt.After(marks[sec])
	}
	counts := map[int64]int{}
	bySection := map[int64][]sqlite.LearningSignal{}
	for _, s := range all {
		for _, sec := range []int64{s.ModelSection, s.ChosenSection} {
			if isNew(s, sec) {
				counts[sec]++
				bySection[sec] = append(bySection[sec], s)
			}
		}
	}
	// No single person steers a Section: its signals must come from MinSignalPeople people.
	ids := make([]int64, 0, len(counts))
	for id := range counts {
		if DistinctPeople(bySection[id]) >= MinSignalPeople {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if counts[ids[i]] != counts[ids[j]] {
			return counts[ids[i]] > counts[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > maxSectionsPerRun {
		ids = ids[:maxSectionsPerRun]
	}
	chosen := map[int64]bool{}
	var sections []domain.Section
	for _, id := range ids {
		s, _ := snap.Section(id)
		sections = append(sections, s)
		chosen[id] = true
	}
	var used []sqlite.LearningSignal
	for _, s := range all {
		if (chosen[s.ModelSection] && isNew(s, s.ModelSection)) || (chosen[s.ChosenSection] && isNew(s, s.ChosenSection)) {
			used = append(used, s)
		}
	}
	return used, sections, len(counts), nil
}

// DistinctPeople counts the Slack user IDs behind signals; a signal without one counts for nobody.
func DistinctPeople(signals []sqlite.LearningSignal) int {
	seen := map[string]bool{}
	for _, s := range signals {
		if s.Person != "" {
			seen[s.Person] = true
		}
	}
	return len(seen)
}

// replayItems are the recent person-decided Work Items filed in an active Section.
func (l *Learner) replayItems(snap *filing.Snapshot, now time.Time) ([]domain.WorkItem, error) {
	items, err := sqlite.GetPersonDecidedItems(l.DB, now.AddDate(0, 0, -7*ReplayWindowWeeks))
	if err != nil {
		return nil, err
	}
	var out []domain.WorkItem
	for _, it := range items {
		if _, ok := snap.Section(it.SectionID); ok {
			out = append(out, it)
		}
	}
	return out, nil
}

func (l *Learner) draftNotes(ctx context.Context, snap *filing.Snapshot, sections []domain.Section, signals []sqlite.LearningSignal, decided []domain.WorkItem) ([]llm.NoteDraft, error) {
	examples := map[int64][]string{}
	for _, it := range decided {
		if len(examples[it.SectionID]) < examplesPerSection {
			examples[it.SectionID] = append(examples[it.SectionID], it.Description)
		}
	}
	in := make([]llm.NotesSection, 0, len(sections))
	ids := make([]int64, 0, len(sections))
	listed := map[int64]bool{}
	for _, s := range sections {
		in = append(in, NotesSectionFor(s, examples[s.ID]))
		ids = append(ids, s.ID)
		listed[s.ID] = true
	}
	// The other side of each confusion is listed too, so the notes can name where work goes instead.
	for _, sg := range signals {
		for _, id := range []int64{sg.ModelSection, sg.ChosenSection} {
			if s, ok := snap.Section(id); ok && !listed[id] {
				listed[id] = true
				in = append(in, NotesSectionFor(s, examples[id]))
			}
		}
	}
	lines := make([]llm.NotesSignal, len(signals))
	for i, s := range signals {
		lines[i] = llm.NotesSignal{Description: s.Description, From: s.ModelSection, To: s.ChosenSection}
	}
	system, user := llm.BuildNotesPrompt(in, lines)
	text, err := l.complete(ctx, system, user)
	if err != nil {
		return nil, err
	}
	return llm.ParseNotes(text, ids)
}

// NotesSectionFor is how a Section is described to the LLM drafting its notes.
func NotesSectionFor(s domain.Section, examples []string) llm.NotesSection {
	return llm.NotesSection{ID: s.ID, Path: s.Path(), Description: s.Description, Notes: s.LearnedNotes, Examples: examples}
}

// withNotes returns sections with the candidates' notes in place.
func withNotes(sections []domain.Section, cands []NoteOutcome) []domain.Section {
	notes := map[int64]string{}
	for _, c := range cands {
		notes[c.Section.ID] = c.New
	}
	out := make([]domain.Section, len(sections))
	for i, s := range sections {
		if n, ok := notes[s.ID]; ok {
			s.LearnedNotes = n
		}
		out[i] = s
	}
	return out
}

// replay asks the decision model's Section Choice for each item over sections, as live filing would.
func (l *Learner) replay(ctx context.Context, snap *filing.Snapshot, items []domain.WorkItem, sections []domain.Section) ([]ReplayResult, error) {
	asker := &CachedAsker{DB: l.DB, Next: l.Filer.Decision, Model: l.Cfg.DecisionModelName}
	out := make([]ReplayResult, len(items))
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		go func(i int, it domain.WorkItem) { // the decision client bounds concurrency
			defer wg.Done()
			state, qs := snap.SectionRequest(it, sections)
			ans, err := asker.Ask(ctx, state, qs)
			if err != nil {
				errs[i] = err
				return
			}
			pick, _, _ := snap.Pick(ans.Answers[filing.QSection])
			out[i] = ReplayResult{ItemID: it.ID, Truth: it.SectionID, Pick: pick}
		}(i, it)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CachedAsker answers identical decision-model requests from the replay cache.
type CachedAsker struct {
	DB    *sql.DB
	Next  filing.Asker
	Model string
}

type cachedResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
}

func (c *CachedAsker) Ask(ctx context.Context, state any, qs map[string]decision.Question) (*decision.Response, error) {
	body, err := json.Marshal(struct {
		Model     string                       `json:"model"`
		State     any                          `json:"state"`
		Questions map[string]decision.Question `json:"questions"`
	}{c.Model, state, qs})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	if cached, ok, err := sqlite.GetReplayAnswer(c.DB, hash); err != nil {
		return nil, err
	} else if ok {
		var cr cachedResponse
		if err := json.Unmarshal([]byte(cached), &cr); err == nil {
			resp := &decision.Response{Model: cr.Model, Answers: map[string]decision.Answer{}}
			for id, raw := range cr.Answers {
				var a decision.Answer
				if json.Unmarshal(raw, &a) == nil {
					a.Raw = raw
					resp.Answers[id] = a
				}
			}
			return resp, nil
		}
	}
	resp, err := c.Next.Ask(ctx, state, qs)
	if err != nil {
		return nil, err
	}
	cr := cachedResponse{Model: resp.Model, Answers: map[string]json.RawMessage{}}
	for id, a := range resp.Answers {
		cr.Answers[id] = a.Raw
	}
	if b, err := json.Marshal(cr); err == nil {
		if err := sqlite.PutReplayAnswer(c.DB, hash, string(b), resp.Model); err != nil {
			log.Printf("learning replay cache write: %v", err)
		}
	}
	return resp, nil
}

// UndoNotes restores the Learned Notes an applied change replaced; any manager may undo.
func (l *Learner) UndoNotes(historyID int64, actorID string) (domain.Section, error) {
	h, err := sqlite.UndoLearnedNotes(l.DB, historyID, actorID)
	if err != nil {
		return domain.Section{}, err
	}
	tax, err := sqlite.LoadTaxonomy(l.DB)
	if err != nil {
		return domain.Section{}, err
	}
	s, _ := tax.SectionAny(h.SectionID)
	log.Printf("learning notes undone section=%d history=%d by=%s", h.SectionID, historyID, actorID)
	return s, nil
}
