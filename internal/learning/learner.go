// Package learning runs the LLM's Structure Proposals: weekly suggestions to add, split or
// retire Sections, which managers approve or reject.
package learning

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/integrations/llm"
)

var ErrNoLLM = errors.New("the LLM isn't configured")

// Notifier sends Structure Proposals to managers (Slack in production).
type Notifier interface {
	ProposalsReady(ps []StoredProposal)
}

// Learner proposes structure changes and applies the approved ones.
type Learner struct {
	DB     *sql.DB
	Cfg    config.Config
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
