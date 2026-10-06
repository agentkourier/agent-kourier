package broker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

// Every part of the botMessage triggers' path is connected in the Intake Run builds, and each is the store's or the
// starter's own: a keyed trigger's threads are the store's; a start submits the rendered prompt through the
// starter; and the check that a stale reservation's turn was queued reads the store's reply queue, which is what
// closes the window between queueing a turn and marking its thread.
func TestIntakeConfigConnectsTheTriggerPath(t *testing.T) {
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := newStarter(t)
	cfg, err := newIntakeConfig(intakeParts{
		conn: v1alpha1.ObjectRef{Namespace: "courier-system", Name: "slack-main"}, snapshot: func() *config.Snapshot { return nil },
		submitter: s.sub, interactions: func(context.Context, chat.Interaction) error { return nil }, notice: s.notes.take,
		starter: s.triggerStarter, threads: st, runs: st, replies: st, log: s.log,
	})
	if err != nil {
		t.Fatalf("newIntakeConfig = %v", err)
	}

	if cfg.TriggerThreads != store.TriggerThreads(st) {
		t.Errorf("TriggerThreads = %v; want the store", cfg.TriggerThreads)
	}
	if cfg.TriggerRuns != store.TriggerRuns(st) {
		t.Errorf("TriggerRuns = %v; want the store: without it no limit holds", cfg.TriggerRuns)
	}
	if cfg.Notice == nil || cfg.Submitter == nil {
		t.Errorf("Notice %t, Submitter %t; want both set", cfg.Notice != nil, cfg.Submitter != nil)
	}

	if cfg.OnStart == nil {
		t.Fatal("OnStart is not set: a started thread would investigate nothing")
	}
	if err := cfg.OnStart(t.Context(), firing(t)); err != nil || len(s.sub.got) != 1 {
		t.Errorf("OnStart = %v with %d turns submitted; want the starter to submit the one turn", err, len(s.sub.got))
	}

	if cfg.TurnQueued == nil {
		t.Fatal("TurnQueued is not set: a lost mark would give the key a second turn")
	}
	const id = "slack:courier-system/slack-main:C0EXAMPLE01:1759560000.123456"
	queued := func() bool {
		t.Helper()
		ok, err := cfg.TurnQueued(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if queued() {
		t.Error("TurnQueued says a turn that was never queued was")
	}
	key := session.Key{Connection: "courier-system/slack-main", Channel: "C0EXAMPLE01", Thread: "1759560000.123456"}
	if _, err := st.EnqueueReply(context.Background(), store.Reply{ID: id, Key: key, UserID: session.SystemUser, MessageRef: key.Thread, Text: "x", EnqueuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if !queued() {
		t.Error("TurnQueued does not see a turn in the store's reply queue")
	}
}

// newIntakeConfig refuses a part that is left out, and the error names it: each is a part of the path that would compile
// and run without it, and do nothing.
func TestIntakeConfigNamesThePartThatIsMissing(t *testing.T) {
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := newStarter(t)
	full := intakeParts{
		conn: v1alpha1.ObjectRef{Namespace: "courier-system", Name: "slack-main"}, snapshot: func() *config.Snapshot { return nil },
		submitter: s.sub, interactions: func(context.Context, chat.Interaction) error { return nil }, notice: s.notes.take,
		starter: s.triggerStarter, threads: st, runs: st, replies: st, log: s.log,
	}
	for _, tc := range []struct {
		part  string
		clear func(*intakeParts)
	}{
		{"conn", func(p *intakeParts) { p.conn = v1alpha1.ObjectRef{} }},
		{"snapshot", func(p *intakeParts) { p.snapshot = nil }},
		{"submitter", func(p *intakeParts) { p.submitter = nil }},
		{"interactions", func(p *intakeParts) { p.interactions = nil }},
		{"notice", func(p *intakeParts) { p.notice = nil }},
		{"starter", func(p *intakeParts) { p.starter = nil }},
		{"threads", func(p *intakeParts) { p.threads = nil }},
		{"runs", func(p *intakeParts) { p.runs = nil }},
		{"replies", func(p *intakeParts) { p.replies = nil }},
		{"log", func(p *intakeParts) { p.log = nil }},
	} {
		t.Run(tc.part, func(t *testing.T) {
			p := full
			tc.clear(&p)
			if _, err := newIntakeConfig(p); err == nil || !strings.HasPrefix(err.Error(), "broker: "+tc.part+" is required") {
				t.Fatalf("newIntakeConfig = %v; want an error that names %s", err, tc.part)
			}
		})
	}
	if _, err := newIntakeConfig(full); err != nil {
		t.Fatalf("newIntakeConfig with every part = %v", err)
	}
}
