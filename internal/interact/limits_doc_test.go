package interact

import (
	"context"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// noEphemeral is an Ephemeral that sends nothing. Ephemeral cannot be embedded as the others are: its one method has its name.
type noEphemeral struct{}

func (noEphemeral) Ephemeral(context.Context, chat.Target, string, chat.Message) error { return nil }

// TestLimitsPage holds the page docs/reference/limits.md to the limits of the interactions. The broker sets neither the
// press limits nor the graces, so the defaults that the constructors fill in are the values. Each interface is given as a
// struct that embeds it and holds none: the constructors only check that one is set, and nothing is called.
func TestLimitsPage(t *testing.T) {
	p, err := NewPipeline(PressConfig{
		Store: struct{ PressStore }{}, Sessions: struct{ SessionReader }{}, Lookup: struct{ Lookup }{},
		Authorizer: struct{ Authorizer }{}, Ephemerals: noEphemeral{}, Decider: struct{ PressDecider }{},
		Auditor: struct{ Auditor }{}, Notify: func(store.Press) {},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	limitsdoc.Count(t, "pressTotal", p.cfg.Limits.Total)
	limitsdoc.Count(t, "pressPerUser", p.cfg.Limits.PerUser)

	r, err := NewRetention(RetentionConfig{Store: struct{ RetentionStore }{}, OnCleared: func(int) {}})
	if err != nil {
		t.Fatalf("NewRetention: %v", err)
	}
	limitsdoc.Duration(t, "clearGrace", r.cfg.ClearGrace)
	limitsdoc.Duration(t, "undeliveredGrace", r.cfg.UndeliveredGrace)

	// The broker builds the press worker with only its processor and logger, and the Slack client with PressAuthzTimeout.
	w, err := NewWorker(WorkerConfig{Processor: struct{ PressProcessor }{}})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	limitsdoc.Duration(t, "pressGiveUp", w.cfg.GiveUpAfter)
	limitsdoc.Duration(t, "pressAuthz", PressAuthzTimeout)
}
