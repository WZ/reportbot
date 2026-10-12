package filing

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/integrations/decision"
	"reportbot/internal/storage/sqlite"
)

const (
	workerCount   = 4
	maxAttempts   = 5
	retryBase     = 30 * time.Second
	retryMax      = 15 * time.Minute
	pollInterval  = 5 * time.Second
	sweepInterval = 15 * time.Minute
	staleAfter    = 30 * time.Minute
)

// Queue runs filing jobs from the jobs table with a small worker pool.
type Queue struct {
	Filer *Filer
	wake  chan struct{}
	once  sync.Once
}

func NewQueue(f *Filer) *Queue {
	return &Queue{Filer: f, wake: make(chan struct{}, 1)}
}

// Wake makes an idle worker look for due jobs now.
func (q *Queue) Wake() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Start resets jobs a crash left running, then runs the workers and the sweep until ctx ends.
func (q *Queue) Start(ctx context.Context) {
	q.once.Do(func() {
		if n, err := sqlite.ResetRunningJobs(q.Filer.DB); err != nil {
			log.Printf("filing: reset running jobs: %v", err)
		} else if n > 0 {
			log.Printf("filing: re-queued %d jobs left running", n)
		}
		for i := 0; i < workerCount; i++ {
			go q.work(ctx)
		}
		go q.sweep(ctx)
	})
}

func (q *Queue) work(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-timer.C:
		}
		for q.RunOne(ctx) {
		}
		timer.Reset(pollInterval)
	}
}

// RunOne claims and runs one due job; it reports whether there was one.
func (q *Queue) RunOne(ctx context.Context) bool {
	f := q.Filer
	job, err := sqlite.ClaimJob(f.DB, f.now())
	if err != nil {
		log.Printf("filing: claim job: %v", err)
		return false
	}
	if job == nil {
		return false
	}
	err = q.run(ctx, job)
	switch {
	case err == nil:
		if err := sqlite.CompleteJob(f.DB, job.ID); err != nil {
			log.Printf("filing: complete job %d: %v", job.ID, err)
		}
	case job.Attempts < maxAttempts:
		delay := min(retryBase<<(job.Attempts-1), retryMax)
		log.Printf("filing: job %d (%s item=%d) attempt %d failed, retry in %s: %v", job.ID, job.Kind, job.WorkItemID, job.Attempts, delay, err)
		if err := sqlite.RetryJob(f.DB, job.ID, err.Error(), f.now().Add(delay)); err != nil {
			log.Printf("filing: retry job %d: %v", job.ID, err)
		}
	default:
		log.Printf("filing: job %d (%s item=%d) gave up after %d attempts: %v", job.ID, job.Kind, job.WorkItemID, job.Attempts, err)
		if err := sqlite.KillJob(f.DB, job.ID, err.Error()); err != nil {
			log.Printf("filing: kill job %d: %v", job.ID, err)
		}
		if job.Kind == sqlite.JobFile {
			if err := f.MarkFailed(job.WorkItemID, ParsePayload(job.Payload)); err != nil {
				log.Printf("filing: mark item %d failed: %v", job.WorkItemID, err)
			}
		}
	}
	return true
}

func (q *Queue) run(ctx context.Context, job *sqlite.Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	switch job.Kind {
	case sqlite.JobFile:
		return q.Filer.FileItem(ctx, job.WorkItemID, ParsePayload(job.Payload))
	case sqlite.JobReclassify:
		return q.Filer.Reclassify(ctx)
	}
	return fmt.Errorf("unknown job kind %q", job.Kind)
}

func (q *Queue) sweep(ctx context.Context) {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := q.Sweep(); n > 0 {
				q.Wake()
			}
		}
	}
}

// Sweep files unanswered unsure items by fallback and re-queues items whose filing failed
// or stalled; it returns how many were re-queued.
func (q *Queue) Sweep() int {
	f := q.Filer
	f.runDueFallbacks()
	items, err := sqlite.GetItemsNeedingRetry(f.DB, f.now().Add(-staleAfter))
	if err != nil {
		log.Printf("filing sweep: %v", err)
		return 0
	}
	for _, it := range items {
		if err := sqlite.SetFilingPending(f.DB, it.ID, Payload{Kind: KindRetry}.JSON()); err != nil {
			log.Printf("filing sweep item=%d: %v", it.ID, err)
		}
	}
	if len(items) > 0 {
		log.Printf("filing sweep: re-queued %d items", len(items))
	}
	return len(items)
}

// Reclassify re-files Open Work Items after a Taxonomy edit; confident moves apply even to
// person-placed items, whose owner is told and can undo. Unsure Undetermined items, done or
// not, are re-asked so the fallback never uses an answer from before the edit.
func (f *Filer) Reclassify(ctx context.Context) error {
	c, err := f.loadContext()
	if err != nil {
		return err
	}
	open, err := sqlite.GetOpenItems(f.DB)
	if err != nil {
		return err
	}
	asking, err := sqlite.GetAskingItems(f.DB)
	if err != nil {
		return err
	}
	var items []domain.WorkItem
	seen := map[int64]bool{}
	for _, it := range append(open, asking...) {
		if it.FilingState != domain.FilingPending && !seen[it.ID] {
			seen[it.ID] = true
			items = append(items, it)
		}
	}
	if len(c.sections) == 0 || len(items) == 0 {
		return nil
	}
	type outcome struct {
		resp *decision.Response
		err  error
	}
	results := make([]outcome, len(items))
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		go func(i int, it domain.WorkItem) { // the decision client bounds concurrency
			defer wg.Done()
			state, qs := BuildRequest(RequestInput{Item: it, Sections: c.sections, Similar: c.similarLines(it, f.similarCount())})
			results[i].resp, results[i].err = f.Decision.Ask(ctx, state, qs)
		}(i, it)
	}
	wg.Wait()

	moved := 0
	for i, r := range results {
		if r.err != nil {
			return fmt.Errorf("decision model: %w", r.err)
		}
		it := items[i]
		pick, conf, _ := c.sectionAnswer(r.resp.Answers[QSection])
		_, currentActive := c.active[it.SectionID]
		switch {
		case pick != 0 && pick != it.SectionID && conf >= f.Cfg.DecisionModelConfidence:
			if err := f.move(c, it, sqlite.Placement{SectionID: pick, PlacedBy: domain.PlacedByModel, Confidence: &conf,
				ModelSectionID: pick, FilingState: domain.FilingFiled, HistoryNote: sqlite.ReasonReclassify}, r.resp); err != nil {
				return err
			}
			moved++
		case it.SectionID != 0 && !currentActive:
			if err := sqlite.SetPlacement(f.DB, it.ID, sqlite.Placement{PlacedBy: domain.PlacedByModel, ModelSectionID: pick,
				FilingState: domain.FilingFiled, Reason: domain.ReasonSectionRetired, HistoryNote: sqlite.ReasonReclassify}); err != nil {
				return err
			}
		case it.SectionID == 0 && it.FilingState == domain.FilingAsking:
			// Keep the unsure answer, computed against the current Taxonomy, for the fallback.
			js := judgments(r.resp)
			for i := range js {
				js[i].WorkItemID = it.ID
				if js[i].Kind == "section_choice" {
					js[i].Subject = sqlite.SubjectReask
				}
			}
			if err := sqlite.InsertJudgments(f.DB, js); err != nil {
				return err
			}
		}
	}
	log.Printf("filing reclassify: open=%d moved=%d", len(items), moved)
	return nil
}

func (f *Filer) move(c *filingContext, it domain.WorkItem, p sqlite.Placement, resp *decision.Response) error {
	if err := sqlite.ApplyFiling(f.DB, it.ID, sqlite.FilingOutcome{Placement: p, Judgments: judgments(resp)}); err != nil {
		return err
	}
	if it.PlacedBy != domain.PlacedByPerson || f.Notify == nil {
		return nil
	}
	eventID, err := sqlite.LatestPlacementEventID(f.DB, it.ID)
	if err != nil {
		return err
	}
	from, _ := c.tax.SectionAny(it.SectionID)
	f.Notify.PlacementOverridden(it, eventID, from, c.active[p.SectionID])
	return nil
}
