package interact_test

import (
	"context"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// The constructors of this package and the outbox and the audit writer refuse a hook that is left out, so the tests that
// are not about a metric fill it with one that counts nothing.

// newOutbox is outbox.New with the OnAbandon that cfg leaves out counting nothing.
func newOutbox(t *testing.T, cfg outbox.Config) *outbox.Outbox {
	t.Helper()
	if cfg.OnAbandon == nil {
		cfg.OnAbandon = outbox.NoAbandon
	}
	o, err := outbox.New(cfg)
	if err != nil {
		t.Fatalf("outbox.New = %v", err)
	}
	return o
}

// newWriter is audit.New with the OnFailure that opts leave out counting nothing.
func newWriter(t *testing.T, log store.Audit, opts audit.Options) *audit.Writer {
	t.Helper()
	if opts.OnFailure == nil {
		opts.OnFailure = audit.NoFailure
	}
	w, err := audit.New(log, opts)
	if err != nil {
		t.Fatalf("audit.New = %v", err)
	}
	return w
}

// noReplace is the PromptReplacer of a Decider that leaves the prompts as they are.
type noReplace struct{}

func (noReplace) Replace(context.Context, string) error { return nil }

// The configs below take what a test leaves out from the no-ops. A test that is about a hook sets it, and keeps its own.

func intakeCfg(c interact.Config) interact.Config {
	if c.OnApprovalOutcome == nil {
		c.OnApprovalOutcome = func(string, interact.Outcome) {}
	}
	return c
}

func decideCfg(c interact.DecideConfig) interact.DecideConfig {
	if c.Replacer == nil {
		c.Replacer = noReplace{}
	}
	if c.OnDecision == nil {
		c.OnDecision = func(string) {}
	}
	return c
}

func pressCfg(c interact.PressConfig) interact.PressConfig {
	if c.Notify == nil {
		c.Notify = func(store.Press) {}
	}
	return c
}

func retentionCfg(c interact.RetentionConfig) interact.RetentionConfig {
	if c.OnCleared == nil {
		c.OnCleared = func(int) {}
	}
	return c
}
