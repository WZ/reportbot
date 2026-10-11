package learning

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/filing"
	"reportbot/internal/integrations/llm"
	"reportbot/internal/storage/sqlite"
)

// Second Opinion rules.
const (
	SecondOpinionWait     = 24 * time.Hour   // a reporter has this long to pick before the assistant does
	PublishSweepTimeout   = 60 * time.Second // publishing waits at most this long for the assistant
	secondOpinionChoices  = 3
	secondOpinionParallel = 4
)

// eligible: Undetermined because the decision model was unsure, and the reporter has not picked.
func eligible(it domain.WorkItem) bool {
	return it.SectionID == 0 && it.FilingState == domain.FilingAsking && it.UndeterminedReason == domain.ReasonLowConfidence
}

// RunDueSecondOpinions files every unsure Work Item whose reporter has not picked within SecondOpinionWait.
func (l *Learner) RunDueSecondOpinions(ctx context.Context) (int, error) {
	if !l.Cfg.LLMConfigured() {
		return 0, ErrNoLLM
	}
	items, err := sqlite.GetSecondOpinionCandidates(l.DB, l.now().Add(-SecondOpinionWait))
	if err != nil || len(items) == 0 {
		return 0, err
	}
	return l.secondOpinions(ctx, items), nil
}

// SweepUndetermined runs the Second Opinion on every eligible item before publishing, without the
// wait, and returns within timeout; items not answered by then stay Undetermined.
func (l *Learner) SweepUndetermined(ctx context.Context, items []domain.WorkItem, timeout time.Duration) int {
	if !l.Cfg.LLMConfigured() {
		return 0
	}
	var due []domain.WorkItem
	for _, it := range items {
		if eligible(it) {
			due = append(due, it)
		}
	}
	if len(due) == 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	n := l.secondOpinions(ctx, due)
	log.Printf("learning publish sweep: undetermined=%d filed=%d", len(due), n)
	return n
}

func (l *Learner) secondOpinions(ctx context.Context, items []domain.WorkItem) int {
	snap, err := l.Filer.Snapshot()
	if err != nil {
		log.Printf("learning second opinion: %v", err)
		return 0
	}
	var mu sync.Mutex
	filed := 0
	sem := make(chan struct{}, secondOpinionParallel)
	var wg sync.WaitGroup
	for _, it := range items {
		wg.Add(1)
		go func(it domain.WorkItem) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			ok, err := l.secondOpinion(ctx, snap, it)
			if err != nil {
				log.Printf("learning second opinion item=%d: %v", it.ID, err)
			}
			if ok {
				mu.Lock()
				filed++
				mu.Unlock()
			}
		}(it)
	}
	wg.Wait()
	return filed
}

// secondOpinion asks the LLM to choose one of the decision model's top 3 Sections for item,
// with the context the decision model saw, and files it with Placement source assistant.
func (l *Learner) secondOpinion(ctx context.Context, snap *filing.Snapshot, it domain.WorkItem) (bool, error) {
	answer, err := sqlite.LatestSectionChoice(l.DB, it.ID)
	if err != nil {
		return false, err
	}
	var probs struct {
		Probabilities map[string]float64 `json:"probabilities"`
	}
	_ = json.Unmarshal([]byte(answer), &probs)
	var choices []llm.OpinionChoice
	for _, id := range sqlite.TopSections(answer, secondOpinionChoices) {
		if s, ok := snap.Section(id); ok {
			choices = append(choices, llm.OpinionChoice{ID: id, Path: s.Path(), Criteria: filing.SectionOption(s),
				Probability: probs.Probabilities[domain.SectionKey(id)]})
		}
	}
	if len(choices) == 0 {
		return false, nil
	}
	in := llm.OpinionInput{Description: it.Description, Author: it.Author, Status: it.Status, Choices: choices}
	in.Source = "reported by the developer in Slack"
	if it.Source == "gitlab" || it.Source == "github" {
		in.Source = "a merge request fetched from code hosting"
	}
	for _, s := range snap.Similar(it) {
		in.Similar = append(in.Similar, llm.SimilarLine{Item: s.Item, FiledUnder: s.FiledUnder})
	}
	system, user := llm.BuildSecondOpinionPrompt(in)
	text, err := l.complete(ctx, system, user)
	if err != nil {
		return false, err
	}
	id, err := llm.ParseSecondOpinion(text, choices)
	if err != nil {
		return false, err
	}
	ok, err := sqlite.PlaceByAssistant(l.DB, it.ID, id)
	if err != nil || !ok {
		return false, err
	}
	if l.Notify != nil {
		s, _ := snap.Section(id)
		if item, err := sqlite.GetWorkItemByID(l.DB, it.ID); err == nil {
			l.Notify.AssistantFiled(item, s)
		}
	}
	return true, nil
}
