package outbox_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// notice is a notice of source, which a producer enqueues.
func notice(source string) outbox.Post {
	p := reply(source)
	p.Role = store.OutboxNotice
	return p
}

// TestARowThisOutboxEnqueuedIsPostedWithNoLookup: a row that Enqueue wrote and that nothing has tried cannot be on the platform
// yet, so the drain posts it at once, with no lookup, whether Run or Reconcile makes the drain. A row a crash left is still
// looked up first, so the count of lookups is that row's alone.
func TestARowThisOutboxEnqueuedIsPostedWithNoLookup(t *testing.T) {
	t.Run("Reconcile", func(t *testing.T) {
		st := sqlitetest.Open(t)
		p := &poster{}
		o := mustNew(t, drainConfig(st, p))
		if _, err := o.Enqueue(t.Context(), notice("n1")); err != nil {
			t.Fatal(err)
		}
		if err := o.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if posts, finds := p.counts(); posts != 1 || finds != 0 || ref(st, "n1/notice") == "" {
			t.Fatalf("posts = %d, finds = %d, ref %q; want the row posted once with no lookup", posts, finds, ref(st, "n1/notice"))
		}
		if n := o.Untried(); n != 0 {
			t.Errorf("Untried = %d after the post; want 0", n)
		}
	})
	t.Run("Run", func(t *testing.T) {
		st := sqlitetest.Open(t)
		left := store.OutboxEntry{ID: "left/notice", EventID: "left", Role: store.OutboxNotice, Channel: "C1", CreatedAt: t0, Payload: neutral("hi")}
		if _, err := st.CreateOutbox(t.Context(), left); err != nil {
			t.Fatal(err)
		}
		p := &poster{found: map[string]string{"left/notice": "55.5"}}
		o := mustNew(t, drainConfig(st, p))
		start(t, o)
		// The row a crash left is adopted by the drain at start, which shows that Run has begun, so the row enqueued next is
		// written after Run's start and is this Run's own.
		testutil.Eventually(t, "the drain at start", func() bool { return ref(st, "left/notice") == "55.5" })
		if _, err := o.Enqueue(t.Context(), notice("n1")); err != nil {
			t.Fatal(err)
		}
		testutil.Eventually(t, "the enqueued notice", func() bool { return ref(st, "n1/notice") != "" })
		if posts, finds := p.counts(); posts != 1 || finds != 1 {
			t.Fatalf("posts = %d, finds = %d; want 1 post and 1 lookup, the left row's", posts, finds)
		}
	})
}

// TestARowWhosePostWasTriedIsLookedUpOnTheRetry: the row leaves the set before its first post, so a post that failed, whose
// message may have landed all the same, is looked up before the next. Here it did land: the retry adopts it and does not post.
func TestARowWhosePostWasTriedIsLookedUpOnTheRetry(t *testing.T) {
	st := sqlitetest.Open(t)
	var clock atomic.Int64
	p := &poster{found: map[string]string{}}
	p.onPost = func(context.Context) error { // the message lands, and the answer is lost
		p.mu.Lock()
		p.found["n1/notice"] = "77.7"
		p.mu.Unlock()
		return context.DeadlineExceeded
	}
	cfg := drainConfig(st, p)
	cfg.Now = func() time.Time { return t0.Add(time.Duration(clock.Load())) }
	cfg.Logger = testutil.DiscardLogger()
	o := mustNew(t, cfg)
	if _, err := o.Enqueue(t.Context(), notice("n1")); err != nil {
		t.Fatal(err)
	}
	if err := o.Reconcile(t.Context()); err == nil {
		t.Fatal("Reconcile with a post that failed = nil; want its error")
	}
	if posts, finds := p.counts(); posts != 1 || finds != 0 {
		t.Fatalf("first try: posts = %d, finds = %d; want 1 post and no lookup", posts, finds)
	}
	clock.Add(int64(time.Hour)) // past the retry delay
	if err := o.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile of the retry: %v", err)
	}
	if posts, finds := p.counts(); posts != 1 || finds != 1 || ref(st, "n1/notice") != "77.7" {
		t.Fatalf("retry: posts = %d, finds = %d, ref %q; want a lookup that adopts 77.7 and no second post", posts, finds, ref(st, "n1/notice"))
	}
}

// TestARowEnqueuedBeforeRunStartsIsLookedUp: Run forgets which rows its Outbox wrote. The broker builds the Outbox and starts
// Run when the replica gains leadership, so a row written before that may have been delivered by a leader that has since gone,
// and only a lookup can tell.
func TestARowEnqueuedBeforeRunStartsIsLookedUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first bool // whether a Run ran, and ended, before the row was written
	}{
		{"before the first Run", false},
		{"between two Runs", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := sqlitetest.Open(t)
			p := &poster{found: map[string]string{"n1/notice": "66.6"}} // delivered by another leader
			o := mustNew(t, drainConfig(st, p))
			if tc.first {
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- o.Run(ctx) }()
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("first Run = %v; want context.Canceled", err)
				}
			}
			if _, err := o.Enqueue(t.Context(), notice("n1")); err != nil {
				t.Fatal(err)
			}
			start(t, o)
			testutil.Eventually(t, "the notice", func() bool { return ref(st, "n1/notice") != "" })
			if posts, finds := p.counts(); posts != 0 || finds != 1 || ref(st, "n1/notice") != "66.6" {
				t.Fatalf("posts = %d, finds = %d, ref %q; want one lookup that adopts 66.6 and no post", posts, finds, ref(st, "n1/notice"))
			}
		})
	}
}

// TestARowThisOutboxDidNotWriteIsLookedUp: a row another process wrote (an old leader, which may have posted it before it
// went) and a row a crash left (this process's predecessor) are both looked up by a new Outbox, which knows of no row as its own;
// and so is such a row that a producer of the new Outbox enqueues again (a redelivered event), since that finds the row there.
func TestARowThisOutboxDidNotWriteIsLookedUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, st store.Outbox, p *poster)
		again bool // whether the new Outbox enqueues the row too
	}{
		{"another Outbox enqueued it", func(t *testing.T, st store.Outbox, p *poster) {
			old := mustNew(t, drainConfig(st, p))
			if _, err := old.Enqueue(t.Context(), notice("n1")); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"another Outbox enqueued it, and this one enqueues it again", func(t *testing.T, st store.Outbox, p *poster) {
			old := mustNew(t, drainConfig(st, p))
			if _, err := old.Enqueue(t.Context(), notice("n1")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"a crash left it", func(t *testing.T, st store.Outbox, _ *poster) {
			row := store.OutboxEntry{ID: "n1/notice", EventID: "n1", Role: store.OutboxNotice, Channel: "C1", CreatedAt: t0, Payload: neutral("hi")}
			if _, err := st.CreateOutbox(t.Context(), row); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := sqlitetest.Open(t)
			p := &poster{found: map[string]string{"n1/notice": "66.6"}} // posted before the old process went
			tc.write(t, st, p)
			o := mustNew(t, drainConfig(st, p))
			if tc.again {
				if _, err := o.Enqueue(t.Context(), notice("n1")); err != nil {
					t.Fatal(err)
				}
			}
			if err := o.Reconcile(t.Context()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if posts, finds := p.counts(); posts != 0 || finds != 1 || ref(st, "n1/notice") != "66.6" {
				t.Fatalf("posts = %d, finds = %d, ref %q; want one lookup that adopts 66.6 and no post", posts, finds, ref(st, "n1/notice"))
			}
		})
	}
}

// TestARowWhoseConnectionIsDownStaysUntried: a drain that finds the connection down tries nothing, so the row keeps its place
// in the set and is posted with no lookup once the connection runs.
func TestARowWhoseConnectionIsDownStaysUntried(t *testing.T) {
	st := sqlitetest.Open(t)
	b := &poster{}
	set := chat.PlatformSet[outbox.Platform]{connA: &poster{}}
	o := mustNew(t, outbox.Config{Store: st, Platforms: set, Now: func() time.Time { return t0 }, CallTimeout: time.Second, Logger: testutil.DiscardLogger()})
	p := notice("n1")
	p.Connection = connB
	if _, err := o.Enqueue(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if err := o.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile with b down: %v", err)
	}
	set[connB] = b // b runs now; no drain is in flight
	if err := o.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile with b up: %v", err)
	}
	if posts, finds := b.counts(); posts != 1 || finds != 0 {
		t.Fatalf("posts = %d, finds = %d; want the row posted with no lookup once b runs", posts, finds)
	}
}

// createHook is a store whose CreateOutbox calls after once the row is written, before it returns.
type createHook struct {
	store.Outbox
	after func()
}

func (s *createHook) CreateOutbox(ctx context.Context, row store.OutboxEntry) (bool, error) {
	created, err := s.Outbox.CreateOutbox(ctx, row)
	if err == nil && s.after != nil {
		after := s.after
		s.after = nil
		after()
	}
	return created, err
}

// TestEveryWayOutOfTheBacklogLeavesTheSet: the set holds only rows that are queued and untried, so it is bounded by the backlog.
// A row that leaves the backlog by any path leaves the set too: posted by the drain, posted by a Send of the same ID, abandoned
// past Retention, cancelled by the removal of its reaction, and tried by a drain while its Enqueue was still writing it (which
// must not then mark it untried, or it would stay for good).
func TestEveryWayOutOfTheBacklogLeavesTheSet(t *testing.T) {
	t.Run("a Send of the same ID", func(t *testing.T) {
		st := sqlitetest.Open(t)
		p := &poster{}
		o := mustNew(t, drainConfig(st, p))
		if _, err := o.Enqueue(t.Context(), reply("ev1")); err != nil {
			t.Fatal(err)
		}
		if _, err := o.Send(t.Context(), reply("ev1")); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if posts, finds := p.counts(); posts != 1 || finds != 0 || o.Untried() != 0 {
			t.Fatalf("posts = %d, finds = %d, Untried = %d; want one post, no lookup, and an empty set", posts, finds, o.Untried())
		}
	})
	t.Run("expired", func(t *testing.T) {
		st := sqlitetest.Open(t)
		var clock atomic.Int64
		p := &poster{}
		cfg := drainConfig(st, p)
		cfg.Now = func() time.Time { return t0.Add(time.Duration(clock.Load())) }
		cfg.Logger = testutil.DiscardLogger()
		o := mustNew(t, cfg)
		if _, err := o.Enqueue(t.Context(), notice("n1")); err != nil {
			t.Fatal(err)
		}
		clock.Add(int64(8 * 24 * time.Hour))
		if err := o.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if posts, _ := p.counts(); posts != 0 || !abandoned(t, st, "n1/notice") || o.Untried() != 0 {
			t.Fatalf("posts = %d, abandoned = %v, Untried = %d; want the row abandoned unposted and an empty set", posts, abandoned(t, st, "n1/notice"), o.Untried())
		}
	})
	t.Run("a reaction its removal cancelled", func(t *testing.T) {
		st := sqlitetest.Open(t)
		r := &reactor{}
		b := &poster{reactor: r}
		// The reaction's connection is down, so it waits untried; its removal, on a connection that runs, cancels it.
		o := mustNew(t, outbox.Config{Store: st, Platforms: chat.PlatformSet[outbox.Platform]{connB: b}, Now: func() time.Time { return t0 }, CallTimeout: time.Second, Logger: testutil.DiscardLogger()})
		add, remove := queuedReaction, queuedReaction
		add.Connection, remove.Connection = connA, connB
		if _, err := o.EnqueueReaction(t.Context(), add); err != nil {
			t.Fatal(err)
		}
		if _, err := o.EnqueueUnreaction(t.Context(), remove); err != nil {
			t.Fatal(err)
		}
		if err := o.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if !abandoned(t, st, outbox.ID(add.Source, store.OutboxReaction)) || o.Untried() != 0 {
			t.Fatalf("reaction abandoned = %v, Untried = %d; want the reaction cancelled and an empty set", abandoned(t, st, outbox.ID(add.Source, store.OutboxReaction)), o.Untried())
		}
	})
	t.Run("tried while Enqueue wrote it", func(t *testing.T) {
		st := &createHook{Outbox: sqlitetest.Open(t)}
		p := &poster{}
		o := mustNew(t, outbox.Config{Store: st, Platforms: chattest.Same[outbox.Platform](p), Now: func() time.Time { return t0 }, CallTimeout: time.Second, Logger: testutil.DiscardLogger()})
		st.after = func() { // the row is written and Enqueue has not returned: a drain takes it now
			if err := o.Reconcile(t.Context()); err != nil {
				t.Errorf("Reconcile inside the write: %v", err)
			}
		}
		if _, err := o.Enqueue(t.Context(), notice("n1")); err != nil {
			t.Fatal(err)
		}
		if posts, finds := p.counts(); posts != 1 || finds != 1 || o.Untried() != 0 {
			t.Fatalf("posts = %d, finds = %d, Untried = %d; want the drain to look it up (it was not known as untried yet), post it once, and leave the set empty", posts, finds, o.Untried())
		}
	})
}
