package outbox_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// drainConfig is a Config for Run: a ticker too slow to matter unless a test sets it.
func drainConfig(st store.Outbox, p *poster) outbox.Config {
	return outbox.Config{Store: st, Platform: p, Now: func() time.Time { return t0 }, CallTimeout: time.Second, Interval: time.Hour}
}

// start runs o.Run until the end of the test, which checks that Run returned the context's error.
func start(t *testing.T, o *outbox.Outbox) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Run after cancel = %v; want context.Canceled", err)
		}
	})
}

// eventually fails the test if cond does not hold within five seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func ref(st store.Outbox, id string) string {
	row, _ := st.GetOutbox(context.Background(), id)
	return row.MessageRef
}

// TestEnqueueIsOneLocalWrite: no Slack call, one row for one ID, and no blocking when nothing is
// draining (a notice is enqueued on Slack's acknowledgement path).
func TestEnqueueIsOneLocalWrite(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	p := &poster{}
	o := outbox.New(drainConfig(st, p))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 5 { // far more than any signal buffer holds; the same ID, twice over
			for _, src := range []string{"n1", "n2", "n1"} {
				id, err := o.Enqueue(t.Context(), outbox.Post{Source: src, Role: store.OutboxNotice, Channel: "C1", Out: reply(src).Out})
				if err != nil || id != outbox.ID(src, store.OutboxNotice) {
					t.Errorf("Enqueue = %q, %v", id, err)
				}
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue blocked with nothing draining")
	}
	rows, _ := st.ListUnposted(t.Context())
	if posts, finds := p.counts(); len(rows) != 2 || rows[0].Payload == "" || posts != 0 || finds != 0 {
		t.Fatalf("%d rows, %d posts, %d finds; want 2 rows, with payloads, and no Slack call", len(rows), posts, finds)
	}
}

// TestRunDeliversWhatWasLeftAndWhatIsEnqueued: Run drains at start, with no signal, the row a
// crash left; then delivers an enqueued one on its signal. Both are looked up first, and the one
// Slack already has is not posted again.
func TestRunDeliversWhatWasLeftAndWhatIsEnqueued(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	left := store.OutboxEntry{ID: "left/notice", EventID: "left", Role: store.OutboxNotice, Channel: "C1", CreatedAt: t0, Payload: neutral("hi")}
	if _, err := st.CreateOutbox(t.Context(), left); err != nil {
		t.Fatal(err)
	}
	// A stream's row has no payload, and the renderer records its ref itself; Run must leave it alone.
	stream := store.OutboxEntry{ID: "s1/reply", EventID: "s1", Role: store.OutboxReply, Channel: "C1", CreatedAt: t0}
	p := &poster{found: map[string]string{"left/notice": "55.5"}}
	o := outbox.New(drainConfig(st, p))
	start(t, o) // nothing is enqueued yet, so only the drain at start can deliver the row a crash left
	eventually(t, "the drain at start", func() bool { return ref(st, "left/notice") == "55.5" })

	if _, err := st.CreateOutbox(t.Context(), stream); err != nil {
		t.Fatal(err)
	}
	for range 2 { // one row, however many times it is enqueued
		if _, err := o.Enqueue(t.Context(), reply("ev1")); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the enqueued post", func() bool { return ref(st, "ev1/reply") != "" })
	if posts, finds := p.counts(); posts != 1 || finds != 2 || ref(st, "s1/reply") != "" {
		t.Fatalf("posts = %d, finds = %d, stream ref %q; want 1 post, 2 finds, and the stream left alone", posts, finds, ref(st, "s1/reply"))
	}
}

// TestRunEndsWithItsContext: Run returns once its context ends, and leaves no goroutine.
func TestRunEndsWithItsContext(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	before := goleak.IgnoreCurrent()
	o := outbox.New(drainConfig(st, &poster{}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v; want context.Canceled", err)
	}
	goleak.VerifyNone(t, before)
}

// syncBuffer is a log destination that a test may read while the logger writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// TestAFailingRowBacksOff: a post that fails is retried, after a lookup, on a delay that doubles
// to a cap, and a row that keeps failing is logged at Warn without its text.
func TestAFailingRowBacksOff(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	var clock atomic.Int64
	p := &poster{}
	var logs syncBuffer
	cfg := drainConfig(st, p)
	cfg.Now = func() time.Time { return t0.Add(time.Duration(clock.Load())) }
	cfg.Interval, cfg.RetryBase, cfg.RetryMax = 2*time.Millisecond, time.Minute, 2*time.Minute
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	p.onPost = func(context.Context) error {
		if posts, _ := p.counts(); posts < 4 { // the first three posts fail
			return errors.New("connection reset")
		}
		return nil
	}
	o := outbox.New(cfg)
	if _, err := o.Enqueue(t.Context(), reply("ev1")); err != nil {
		t.Fatal(err)
	}
	start(t, o)
	posts := func() int { n, _ := p.counts(); return n }
	// A post counts before its failure sets the delay, and the drain logs its warning after: a step
	// waits for the warning, so the clock moves only once the delay is set.
	warns := func() int { return strings.Count(logs.String(), "level=WARN") }
	steps := []struct {
		advance      time.Duration
		posts, warns int // after the retry the step allows
		hold         bool
	}{
		{0, 1, 1, false},               // the first post fails: next try in 1 minute
		{time.Minute, 2, 2, false},     // the second fails: next in 2
		{time.Minute, 2, 2, true},      // too soon
		{time.Minute, 3, 3, false},     // the third fails: 4 minutes, capped at 2
		{2 * time.Minute, 4, 3, false}, // the fourth goes through
	}
	for i, s := range steps {
		clock.Add(int64(s.advance))
		eventually(t, "the step's retry", func() bool { return posts() >= s.posts && warns() >= s.warns })
		if s.hold {
			time.Sleep(50 * time.Millisecond) // many ticks: none may retry
		}
		if got := posts(); got != s.posts {
			t.Fatalf("step %d: %d posts; want %d", i, got, s.posts)
		}
	}
	eventually(t, "the ref", func() bool { return ref(st, "ev1/reply") != "" })
	if _, finds := p.counts(); finds != 4 {
		t.Errorf("finds = %d; want 4, one before each post", finds)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "ev1/reply") || strings.Contains(out, "hello ev1") {
		t.Errorf("log = %q; want a warning naming the row and not its text", out)
	}
}

// TestEnqueueRefusesEmptyContent: a notice whose text came out empty must fail where it is
// enqueued, not become a row that is never delivered and holds its ID.
func TestEnqueueRefusesEmptyContent(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	o := outbox.New(drainConfig(st, &poster{}))
	if id, err := o.Enqueue(t.Context(), outbox.Post{Source: "n1", Role: store.OutboxNotice, Channel: "C1"}); !errors.Is(err, store.ErrInvalid) || id != "" {
		t.Fatalf("Enqueue with no content = %q, %v; want ErrInvalid", id, err)
	}
	if rows, _ := st.ListUnposted(t.Context()); len(rows) != 0 {
		t.Fatalf("an empty post left %d rows", len(rows))
	}
}

// TestASignalDuringADrainIsNotLost holds a drain inside a post and enqueues another row: the drain
// that follows must take it, though the ticker is an hour away.
func TestASignalDuringADrainIsNotLost(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	inPost, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	p := &poster{onPost: func(context.Context) error {
		if !first.Swap(true) { // only the first post waits
			close(inPost)
			<-release
		}
		return nil
	}}
	o := outbox.New(drainConfig(st, p))
	if _, err := o.Enqueue(t.Context(), reply("ev1")); err != nil {
		t.Fatal(err)
	}
	start(t, o)
	<-inPost // the drain is inside its post
	if _, err := o.Enqueue(t.Context(), reply("ev2")); err != nil {
		t.Fatal(err)
	}
	close(release)
	eventually(t, "the row enqueued during the drain", func() bool { return ref(st, "ev2/reply") != "" })
}
