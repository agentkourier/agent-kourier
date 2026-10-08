package broker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

type readinessSink struct{}

func (readinessSink) Turn(context.Context, session.TurnRef) agent.Handler { return nil }
func (readinessSink) Notice(context.Context, session.Key, session.Notice) {}

type readinessBindings struct{}

// These pinned-answer tests never send an ordinary turn, so they need no Agent.
func (readinessBindings) Backend(string) (session.Backend, error) {
	return session.Backend{Dialect: "a2a", ThreadTTL: time.Hour}, nil
}

func readinessParts(t *testing.T, entered chan<- struct{}) (interactionDeps, *interactionRuntime, *session.Manager) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	aud, err := audit.New(st, audit.Options{OnFailure: audit.NoFailure, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	platform := builtPlatform{Platform: chattest.NewFake()}
	ob, err := outbox.New(outbox.Config{Store: st, Platform: platform, OnAbandon: outbox.NoAbandon, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := NewMetrics(nil)
	if err != nil {
		t.Fatal(err)
	}
	d := interactionDeps{st: st, ob: ob, platform: platform, auditor: aud, bindings: &Bindings{}, metrics: metrics, log: log}
	ix, err := d.build(readinessSink{})
	if err != nil {
		t.Fatal(err)
	}
	textReply := ix.replies.Decide
	if entered != nil {
		textReply = func(ctx context.Context, key session.Key, reply store.Reply, task a2a.Task) (session.TextReplyOutcome, error) {
			entered <- struct{}{}
			return ix.replies.Decide(ctx, key, reply, task)
		}
	}
	mgr, err := session.New(session.Config{Context: t.Context(), Store: st, Questions: st, Bindings: readinessBindings{}, Users: users{platform}, Sink: ix.tap, Audit: aud, TextReply: textReply, ThreadContext: session.NoThreadContext, OnTurnEnd: metrics.TurnOutcome, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ix.late.bind(mgr)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ix.tap.Close(ctx)
		_ = mgr.Close(ctx)
	})
	return d, ix, mgr
}

// The real broker wiring holds decisions before press recovery and releases them when
// interactions start, before intake opens. Deleting or moving close(ix.ready) fails this test.
func TestInteractionStartupReleasesTextRepliesAfterPressRecovery(t *testing.T) {
	d, ix, mgr := readinessParts(t, nil)
	key := store.ConversationKey{Connection: "ns/conn", Channel: "C1", Thread: "1"}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	outcome, err := ix.replies.Decide(ctx, key, store.Reply{}, a2a.Task{})
	cancel()
	if outcome != session.TextReplyRetry || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("before startup = %v, %v", outcome, err)
	}
	d.trace = func(step string) {
		select {
		case <-ix.ready:
			if step == "recover-presses" {
				t.Error("text decisions released before press recovery completed")
			}
		default:
			if step == "start-interactions" || step == "open-intake" {
				t.Errorf("text decisions still blocked at %s", step)
			}
		}
	}
	var stops stopList
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stops.run(ctx)
	})
	if _, err := d.start(t.Context(), mgr, ix, &stops); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	outcome, err = ix.replies.Decide(ctx, key, store.Reply{}, a2a.Task{})
	if outcome != session.TextReplyRetry || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after startup = %v, %v; want the missing session, not a blocked handler", outcome, err)
	}
	// The leader opens intake after start returns. This synthetic trace checks that
	// Ready remains released at that point; start-interactions checks the real release.
	d.traced("open-intake")
}

// A startup failure after recovery must not strand a runner on Ready. This uses the real
// interaction handler and leaves its startup signal unopened until Manager.Close cancels it.
func TestManagerCloseCancelsRealTextReplyStartupWait(t *testing.T) {
	entered := make(chan struct{}, 1)
	d, ix, mgr := readinessParts(t, entered)
	key := store.ConversationKey{Connection: "ns/conn", Channel: "C1", Thread: "1"}
	now := time.Now()
	if err := d.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: "ns/b", LastActivity: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.st.EnqueueReply(t.Context(), store.Reply{Key: key, ID: "pinned-answer", InteractionID: "question", UserID: "U1", MessageRef: "2", Text: "answer", EnqueuedAt: now}); err != nil {
		t.Fatal(err)
	}
	// The observer signals entry, then calls the real constructed handler. A missing
	// cancellation case in its Ready select would strand this runner during Close.
	if err := mgr.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered runner never entered text handler")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := mgr.Close(ctx); err != nil {
		t.Fatalf("Close while interactions never start = %v", err)
	}
	select {
	case <-ix.ready:
		t.Fatal("shutdown released the startup signal")
	default:
	}
	reply, err := d.st.NextReply(t.Context(), key)
	if err != nil || reply.InteractionID != "question" {
		t.Fatalf("shutdown consumed the pending reply: %+v, %v", reply, err)
	}
}
