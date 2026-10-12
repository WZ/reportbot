// Package proposals runs the LLM's Structure Proposals: weekly suggestions to add, split or
// retire Sections, which managers approve or reject.
package proposals

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/integrations/llm"
)

var ErrNoLLM = errors.New("the LLM isn't configured")

// Notifier sends Structure Proposals to managers (Slack in production).
type Notifier interface {
	ProposalsReady(ps []StoredProposal)
}

// Service proposes structure changes and applies the approved ones.
type Service struct {
	DB     *sql.DB
	Cfg    config.Config
	Notify Notifier
	Wake   func() // starts re-filing after an approved Structure Proposal
	Now    func() time.Time
	// Complete calls the LLM; nil uses the configured provider.
	Complete func(ctx context.Context, system, user string) (string, error)
}

func (l *Service) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Service) complete(ctx context.Context, system, user string) (string, error) {
	if l.Complete != nil {
		return l.Complete(ctx, system, user)
	}
	text, _, err := llm.Complete(ctx, l.Cfg, system, user)
	return text, err
}

// afterPublishTimeout bounds the Structure Proposals run that follows a publish.
const afterPublishTimeout = 30 * time.Minute

// AfterPublish generates the week's Structure Proposals; Slack and web Generate both call it.
func (s *Service) AfterPublish() {
	ctx, cancel := context.WithTimeout(context.Background(), afterPublishTimeout)
	defer cancel()
	if _, err := s.ProposeStructure(ctx); err != nil {
		log.Printf("Structure Proposals after publish: %v", err)
	}
}
