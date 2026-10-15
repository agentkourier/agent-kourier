package interact_test

import (
	"context"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Each constructor refuses the hook that its wiring left out, and the error names it: a hook that is not there would
// compile and run and do nothing. The collaborators are inert values (an interface that panics if it is called), so the
// error can only be about the hook.

type (
	inertStore      struct{ store.Interactions }
	inertAuditLog   struct{ store.Audit }
	inertAuditor    struct{ interact.Auditor }
	inertSession    struct{ interact.Session }
	inertReader     struct{ interact.SessionReader }
	inertEnqueuer   struct{ interact.Enqueuer }
	inertLookup     struct{ interact.Lookup }
	inertFitter     struct{ interact.Fitter }
	inertSender     struct{ interact.Sender }
	inertPressStore struct{ interact.PressStore }
	inertAuthorizer struct{ interact.Authorizer }
	inertDecider    struct{ interact.PressDecider }
)

func TestConstructorsRefuseAnUnwiredHookAndNameIt(t *testing.T) {
	for _, tc := range []struct {
		name, hook string
		build      func() error
	}{
		{"the intake", "OnApprovalOutcome", func() error {
			_, err := interact.New(interact.Config{Store: inertStore{}, AuditReader: inertAuditLog{}, Lookup: inertLookup{}, Platform: inertFitter{}, Outbox: inertSender{}, Session: inertSession{}, Audit: inertAuditor{}})
			return err
		}},
		{"the intake's AuditReader", "AuditReader", func() error {
			_, err := interact.New(interact.Config{Store: inertStore{}, Lookup: inertLookup{}, Platform: inertFitter{}, Outbox: inertSender{}, Session: inertSession{}, Audit: inertAuditor{}, OnApprovalOutcome: func(string, interact.Outcome) {}})
			return err
		}},
		{"the decider's Replacer", "Replacer", func() error {
			_, err := interact.NewDecider(interact.DecideConfig{Store: inertStore{}, Auditor: inertAuditor{}, Ephemerals: inertShow{}, Session: inertSession{}, Sessions: inertReader{}, Outbox: inertEnqueuer{}, OnDecision: func(string) {}})
			return err
		}},
		{"the decider's OnDecision", "OnDecision", func() error {
			_, err := interact.NewDecider(interact.DecideConfig{Store: inertStore{}, Auditor: inertAuditor{}, Ephemerals: inertShow{}, Session: inertSession{}, Sessions: inertReader{}, Outbox: inertEnqueuer{}, Replacer: noReplace{}})
			return err
		}},
		{"the decider's OnApprovalOutcome", "OnApprovalOutcome", func() error {
			_, err := interact.NewDecider(interact.DecideConfig{Store: inertStore{}, Auditor: inertAuditor{}, Ephemerals: inertShow{}, Session: inertSession{}, Sessions: inertReader{}, Outbox: inertEnqueuer{}, Replacer: noReplace{}, OnDecision: func(string) {}})
			return err
		}},
		{"the press pipeline", "Notify", func() error {
			_, err := interact.NewPipeline(interact.PressConfig{Store: inertPressStore{}, Sessions: inertReader{}, Lookup: inertLookup{}, Authorizer: inertAuthorizer{}, Ephemerals: inertShow{}, Decider: inertDecider{}, Auditor: inertAuditor{}})
			return err
		}},
		{"the sweeper", "OnTimeout", func() error {
			_, err := interact.NewSweeper(interact.SweepConfig{Store: inertStore{}, Sessions: inertReader{}, Lookup: inertLookup{}, Session: inertSession{}, Updater: inertUpdater{}, Outbox: inertEnqueuer{}, Auditor: inertAuditor{}, Presses: inertPresses{}, OnApprovalOutcome: func(string, interact.Outcome) {}})
			return err
		}},
		{"the sweeper's OnApprovalOutcome", "OnApprovalOutcome", func() error {
			_, err := interact.NewSweeper(interact.SweepConfig{Store: inertStore{}, Sessions: inertReader{}, Lookup: inertLookup{}, Session: inertSession{}, Updater: inertUpdater{}, Outbox: inertEnqueuer{}, Auditor: inertAuditor{}, Presses: inertPresses{}, OnTimeout: func(string) {}})
			return err
		}},
		{"the sweeper's Reopened", "Reopened", func() error {
			_, err := interact.NewSweeper(interact.SweepConfig{Store: inertStore{}, Sessions: inertReader{}, Lookup: inertLookup{}, Session: inertSession{}, Updater: inertUpdater{}, Outbox: inertEnqueuer{}, Auditor: inertAuditor{}, Presses: inertPresses{}, OnTimeout: func(string) {}, OnApprovalOutcome: func(string, interact.Outcome) {}})
			return err
		}},
		{"the sweeper's Lookup", "Lookup", func() error {
			_, err := interact.NewSweeper(interact.SweepConfig{Store: inertStore{}, Sessions: inertReader{}, Session: inertSession{}, Updater: inertUpdater{}, Outbox: inertEnqueuer{}, Auditor: inertAuditor{}, Presses: inertPresses{}, OnTimeout: func(string) {}, OnApprovalOutcome: func(string, interact.Outcome) {}})
			return err
		}},
		{"the retention loop", "OnCleared", func() error {
			_, err := interact.NewRetention(interact.RetentionConfig{Store: inertRetention{}})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.build(); err == nil || !strings.Contains(err.Error(), tc.hook) {
				t.Fatalf("constructor = %v; want an error that names %s", err, tc.hook)
			}
		})
	}
}

// inertShow is an interact.Ephemeral (a method named for its interface cannot be embedded).
type inertShow struct{}

func (inertShow) Ephemeral(context.Context, chat.Target, string, chat.Message) error { return nil }

type (
	inertUpdater   struct{ interact.Updater }
	inertPresses   struct{ interact.PressQueue }
	inertRetention struct{ interact.RetentionStore }
)
