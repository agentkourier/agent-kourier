package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

func slackErr(code string) error { return slackgo.SlackErrorResponse{Err: code} }

// abandons records the OnAbandon calls as "id reason".
type abandons struct {
	mu  sync.Mutex
	got []string
}

func (a *abandons) hook(id, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, id+" "+reason)
}

func (a *abandons) list() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.got...)
}

// abandoning returns an Outbox over p, its OnAbandon log and its warnings. mod may change the Config.
func abandoning(t *testing.T, st store.Outbox, p *poster, mod func(*outbox.Config)) (*outbox.Outbox, *abandons, *syncBuffer) {
	t.Helper()
	a, logs := &abandons{}, &syncBuffer{}
	cfg := drainConfig(st, p)
	cfg.OnAbandon, cfg.Logger = a.hook, slog.New(slog.NewTextHandler(logs, nil))
	if mod != nil {
		mod(&cfg)
	}
	return outbox.New(cfg), a, logs
}

func warnings(l *syncBuffer) int { return strings.Count(l.String(), "level=WARN") }

// TestPermanentFailuresAbandonTheRow: a post that can never succeed gives up the row, once and
// loudly, and nothing retries it or posts it again.
func TestPermanentFailuresAbandonTheRow(t *testing.T) {
	tests := []struct {
		err    error
		reason string
	}{
		{slackErr("channel_not_found"), "channel_not_found"},
		{slackErr("is_archived"), "is_archived"},
		{slackErr("not_in_channel"), "not_in_channel"},
		{slackErr("msg_too_long"), "msg_too_long"},
		{slackErr("invalid_blocks"), "invalid_blocks"},
		{slackErr("invalid_blocks_format"), "invalid_blocks_format"},
		{fmt.Errorf("blocks: %w", store.ErrInvalid), outbox.AbandonInvalid},
	}
	for _, tt := range tests {
		st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
		p := &poster{onPost: func(context.Context) error { return tt.err }}
		ob, hooks, logs := abandoning(t, st, p, nil)
		if _, err := ob.Send(t.Context(), reply("ev1")); !errors.Is(err, outbox.ErrAbandoned) {
			t.Fatalf("%s: Send = %v; want ErrAbandoned", tt.reason, err)
		}
		row, _ := st.GetOutbox(t.Context(), "ev1/reply")
		if row.AbandonedAt.IsZero() || row.Payload != "" || row.MessageRef != "" {
			t.Errorf("%s: row = %+v; want it abandoned with no payload", tt.reason, row)
		}
		// Neither a drain nor another Send posts it again, or says any more.
		if err := ob.Reconcile(t.Context()); err != nil {
			t.Errorf("%s: Reconcile = %v; an abandoned row is no drain failure", tt.reason, err)
		}
		if _, err := ob.Send(t.Context(), reply("ev1")); !errors.Is(err, outbox.ErrAbandoned) {
			t.Errorf("%s: Send of an abandoned row = %v; want ErrAbandoned", tt.reason, err)
		}
		if posts, _ := p.counts(); posts != 1 {
			t.Errorf("%s: %d posts; want 1", tt.reason, posts)
		}
		if got := hooks.list(); len(got) != 1 || got[0] != "ev1/reply "+tt.reason {
			t.Errorf("%s: OnAbandon calls = %q; want one", tt.reason, got)
		}
		if out := logs.String(); warnings(logs) != 1 || strings.Contains(out, "hello ev1") {
			t.Errorf("%s: log = %q; want one warning, with no text", tt.reason, out)
		}
	}
}

// TestTransientFailuresNeverAbandon: errors that a later try may cure leave the row unposted.
func TestTransientFailuresNeverAbandon(t *testing.T) {
	for _, cause := range []error{slackErr("ratelimited"), slackErr("internal_error"), slackErr("invalid_auth"),
		errors.New("connection reset"), context.DeadlineExceeded} {
		st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
		ob, hooks, _ := abandoning(t, st, &poster{onPost: func(context.Context) error { return cause }}, nil)
		_, err := ob.Send(t.Context(), reply("ev1"))
		row, _ := st.GetOutbox(t.Context(), "ev1/reply")
		if err == nil || errors.Is(err, outbox.ErrAbandoned) || !row.AbandonedAt.IsZero() || row.Payload == "" || len(hooks.list()) != 0 {
			t.Errorf("%v: Send = %v, row %+v, hooks %q; want a plain failure and an untouched row", cause, err, row, hooks.list())
		}
	}
}

// TestFiveLookupTimeoutsInARowAbandon: the script says how each pass's lookup ends, T for a
// timeout. Four in a row are not enough, and one lookup that ends otherwise starts the count again.
func TestFiveLookupTimeoutsInARowAbandon(t *testing.T) {
	const script = "TTTTNTTTTT"
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	if _, err := st.CreateOutbox(t.Context(), store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", CreatedAt: t0, Payload: `{"text":"hi"}`}); err != nil {
		t.Fatal(err)
	}
	var clock, pass atomic.Int64
	p := &poster{onPost: func(context.Context) error { return errors.New("connection reset") }}
	p.onFind = func(ctx context.Context) error {
		if script[pass.Load()] == 'T' {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil // not found, and then the post fails
	}
	ob, hooks, _ := abandoning(t, st, p, func(c *outbox.Config) {
		c.CallTimeout, c.Now = 20*time.Millisecond, func() time.Time { return t0.Add(time.Duration(clock.Load())) }
	})
	for i := range script {
		pass.Store(int64(i))
		clock.Add(int64(time.Hour)) // past any retry delay
		_ = ob.Reconcile(t.Context())
		row, _ := st.GetOutbox(t.Context(), "ev1/reply")
		if gone := !row.AbandonedAt.IsZero(); gone != (i == len(script)-1) {
			t.Fatalf("after pass %d (%c) abandoned = %v", i+1, script[i], gone)
		}
	}
	if got := hooks.list(); len(got) != 1 || got[0] != "ev1/reply "+outbox.AbandonLookupTimeout {
		t.Fatalf("OnAbandon calls = %q; want one, for the lookup timeouts", got)
	}
}

// TestRetentionAbandonsOldUnpostedText: the purge runs before a drain looks at the rows, so an
// expired row is not looked up, and a young row or a stream's is left alone.
func TestRetentionAbandonsOldUnpostedText(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	add := func(id string, age time.Duration, payload string) {
		e := store.OutboxEntry{ID: id, EventID: id, Role: store.OutboxReply, Channel: "C1", CreatedAt: t0.Add(-age), Payload: payload}
		if _, err := st.CreateOutbox(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	day := 24 * time.Hour
	add("old", 8*day, `{"text":"a"}`) // past the default seven days
	add("young", 6*day, `{"text":"b"}`)
	add("stream", 8*day, "")
	p := &poster{}
	ob, hooks, logs := abandoning(t, st, p, nil)
	for range 2 {
		if err := ob.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if old, _ := st.GetOutbox(t.Context(), "old"); old.AbandonedAt.IsZero() || old.Payload != "" {
		t.Errorf("old row = %+v; want it abandoned, with no payload", old)
	}
	if stream, _ := st.GetOutbox(t.Context(), "stream"); !stream.AbandonedAt.IsZero() {
		t.Error("a stream's row was abandoned")
	}
	if posts, finds := p.counts(); posts != 1 || finds != 1 || ref(st, "young") == "" {
		t.Errorf("posts = %d, finds = %d; want only the young row looked up and posted", posts, finds)
	}
	if got := hooks.list(); len(got) != 1 || got[0] != "old "+outbox.AbandonExpired || warnings(logs) != 1 {
		t.Errorf("OnAbandon calls = %q, warnings = %d; want one of each", got, warnings(logs))
	}
}

// TestACorruptPayloadIsAbandoned: a row whose payload is not a message will never post.
func TestACorruptPayloadIsAbandoned(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	if _, err := st.CreateOutbox(t.Context(), store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", CreatedAt: t0, Payload: "{"}); err != nil {
		t.Fatal(err)
	}
	p := &poster{}
	ob, hooks, _ := abandoning(t, st, p, nil)
	if err := ob.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if posts, _ := p.counts(); posts != 0 || len(hooks.list()) != 1 || hooks.list()[0] != "ev1/reply "+outbox.AbandonInvalid {
		t.Fatalf("posts = %d, OnAbandon calls = %q; want none posted, one abandon as invalid", posts, hooks.list())
	}
}

// TestRunReportsAnAbandonOnce: Run's own warning for a failed drain does not say it again.
func TestRunReportsAnAbandonOnce(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	p := &poster{onPost: func(context.Context) error { return slackErr("is_archived") }}
	ob, hooks, logs := abandoning(t, st, p, nil)
	start(t, ob)
	id, err := ob.Enqueue(t.Context(), reply("ev1"))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the abandon", func() bool { row, _ := st.GetOutbox(t.Context(), id); return !row.AbandonedAt.IsZero() })
	p.mu.Lock()
	p.onPost = nil // a second row, to make Run drain once more after the abandon
	p.mu.Unlock()
	next, _ := ob.Enqueue(t.Context(), reply("ev2"))
	eventually(t, "the next drain", func() bool { return ref(st, next) != "" })
	if got := hooks.list(); len(got) != 1 || warnings(logs) != 1 {
		t.Fatalf("OnAbandon calls = %q, warnings = %d (%s); want one of each", got, warnings(logs), logs.String())
	}
}

// failingAbandon is a store that cannot record an abandon.
type failingAbandon struct{ store.Outbox }

func (failingAbandon) AbandonOutbox(context.Context, string, time.Time) (bool, error) {
	return false, errors.New("disk full")
}

// TestAnAbandonThatCannotBeRecordedIsNotReported: the row is still work, so no hook and no ErrAbandoned.
func TestAnAbandonThatCannotBeRecordedIsNotReported(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	p := &poster{onPost: func(context.Context) error { return slackErr("is_archived") }}
	ob, hooks, _ := abandoning(t, failingAbandon{st}, p, nil)
	if _, err := ob.Send(t.Context(), reply("ev1")); err == nil || errors.Is(err, outbox.ErrAbandoned) || len(hooks.list()) != 0 {
		t.Fatalf("Send = %v, hooks %q; want a plain failure and no report", err, hooks.list())
	}
}

// TestADeadlineOfTheCallersDoesNotCountAsALookupTimeout: only the call timeout does. A caller whose
// own deadline has passed (a shutdown) must not push a row towards abandonment.
func TestADeadlineOfTheCallersDoesNotCountAsALookupTimeout(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	if _, err := st.CreateOutbox(t.Context(), store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", CreatedAt: t0, Payload: `{"text":"hi"}`}); err != nil {
		t.Fatal(err)
	}
	p := &poster{onFind: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	var clock atomic.Int64
	ob, hooks, _ := abandoning(t, st, p, func(c *outbox.Config) {
		c.Now = func() time.Time { return t0.Add(time.Duration(clock.Load())) }
	})
	for range 6 { // the caller's deadline ends the lookup, well before the call timeout does
		clock.Add(int64(time.Hour)) // past any retry delay
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		_ = ob.Reconcile(ctx)
		cancel()
	}
	if row, _ := st.GetOutbox(t.Context(), "ev1/reply"); !row.AbandonedAt.IsZero() || len(hooks.list()) != 0 {
		t.Fatalf("row = %+v, hooks %q; want it untouched", row, hooks.list())
	}
}

// lateAbandon is a store whose row was abandoned, by someone else, a moment before this call.
type lateAbandon struct{ store.Outbox }

func (lateAbandon) AbandonOutbox(context.Context, string, time.Time) (bool, error) { return false, nil }

// TestAnAbandonSomeoneElseMadeIsNotReportedAgain: only the call that abandons a row reports it.
func TestAnAbandonSomeoneElseMadeIsNotReportedAgain(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	p := &poster{onPost: func(context.Context) error { return slackErr("is_archived") }}
	ob, hooks, logs := abandoning(t, lateAbandon{st}, p, nil)
	if _, err := ob.Send(t.Context(), reply("ev1")); !errors.Is(err, outbox.ErrAbandoned) || len(hooks.list()) != 0 || warnings(logs) != 0 {
		t.Fatalf("Send = %v, hooks %q, warnings %d; want ErrAbandoned and no second report", err, hooks.list(), warnings(logs))
	}
}
