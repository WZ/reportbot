package learning

import (
	"context"
	"log"
	"time"
)

const (
	nightlyHour          = 2 // Learned Notes run at 02:00 in the configured timezone
	secondOpinionPeriod  = time.Hour
	nightlyRunTimeLimit  = 2 * time.Hour
	secondOpinionTimeout = 30 * time.Minute
)

// NextNightly is the next 02:00 after now in loc.
func NextNightly(now time.Time, loc *time.Location) time.Time {
	now = now.In(loc)
	next := time.Date(now.Year(), now.Month(), now.Day(), nightlyHour, 0, 0, 0, loc)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// Start runs Learned Notes nightly and the Second Opinion hourly until ctx ends. Without an
// LLM the loops are off.
func (l *Learner) Start(ctx context.Context) {
	if !l.Cfg.LLMConfigured() {
		log.Printf("learning loops disabled: no LLM configured")
		return
	}
	loc := l.Cfg.Location
	if loc == nil {
		loc = time.Local
	}
	go func() {
		for {
			next := NextNightly(time.Now(), loc)
			log.Printf("learning: next Learned Notes run at %s", next.Format(time.RFC3339))
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
			runCtx, cancel := context.WithTimeout(ctx, nightlyRunTimeLimit)
			if _, err := l.RunNotes(runCtx, TriggerNightly); err != nil {
				log.Printf("learning nightly: %v", err)
			}
			cancel()
		}
	}()
	go func() {
		t := time.NewTicker(secondOpinionPeriod)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runCtx, cancel := context.WithTimeout(ctx, secondOpinionTimeout)
				if n, err := l.RunDueSecondOpinions(runCtx); err != nil {
					log.Printf("learning second opinion: %v", err)
				} else if n > 0 {
					log.Printf("learning second opinion: filed %d Work Items", n)
				}
				cancel()
			}
		}
	}()
}
