package outbox_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// timedOutRow returns an Outbox over a store holding one unposted row, whose lookups hang until
// the 20 ms call timeout, and a clock the test moves. Another row's post, which the test sends
// itself, is how "some other Slack call worked" is shown.
func timedOutRow(t *testing.T) (ob *outbox.Outbox, st store.Outbox, clock *atomic.Int64, hooks *abandons) {
	t.Helper()
	st = openStore(t, filepath.Join(t.TempDir(), "c.db"))
	if _, err := st.CreateOutbox(t.Context(), store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", CreatedAt: t0, Payload: neutral("hi")}); err != nil {
		t.Fatal(err)
	}
	clock = &atomic.Int64{}
	p := &poster{onFind: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	ob, hooks, _ = abandoning(t, st, p, func(c *outbox.Config) {
		c.CallTimeout, c.Now = 20*time.Millisecond, func() time.Time { return t0.Add(time.Duration(clock.Load())) }
	})
	return ob, st, clock, hooks
}

func abandoned(t *testing.T, st store.Outbox, id string) bool {
	t.Helper()
	row, err := st.GetOutbox(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return !row.AbandonedAt.IsZero()
}

// TestLookupTimeoutsNeedAnHourAndAWorkingSlack: the lookups of a row time out ten minutes apart. Five
// in a row are not enough, nor is an hour since the first, unless some other Slack call worked in
// between: with none, it is Slack that is down, and no row is given up on for that.
func TestLookupTimeoutsNeedAnHourAndAWorkingSlack(t *testing.T) {
	ob, st, clock, _ := timedOutRow(t)
	for pass := 1; pass <= 12; pass++ { // an outage: no other call works
		clock.Add(int64(10 * time.Minute))
		_ = ob.Reconcile(t.Context())
		if abandoned(t, st, "ev1/reply") {
			t.Fatalf("abandoned at pass %d, though no Slack call worked in between", pass)
		}
	}

	ob, st, clock, hooks := timedOutRow(t)
	for pass := 1; pass <= 8; pass++ {
		clock.Add(int64(10 * time.Minute))
		if pass == 3 { // another post goes through, once the first lookup has timed out
			if _, err := ob.Send(t.Context(), reply("other")); err != nil {
				t.Fatal(err)
			}
		}
		_ = ob.Reconcile(t.Context())
		// Timeouts at passes 1 to 5 are five; the hour since the first is reached at pass 7.
		if got, want := abandoned(t, st, "ev1/reply"), pass >= 7; got != want {
			t.Fatalf("after pass %d abandoned = %v; want %v", pass, got, want)
		}
	}
	if got := hooks.list(); len(got) != 1 || got[0] != "ev1/reply "+outbox.AbandonLookupTimeout {
		t.Fatalf("OnAbandon calls = %q; want one, for the lookup timeouts", got)
	}
}

// TestAnUnreachableChannelFailsTheLookupForGood: a lookup that Slack answers with a permanent
// code is classified like a post, not retried for the whole retention.
func TestAnUnreachableChannelFailsTheLookupForGood(t *testing.T) {
	for _, code := range []string{"channel_not_found", "not_in_channel"} { // both are channel_gone to a metric
		st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
		if _, err := st.CreateOutbox(t.Context(), store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", CreatedAt: t0, Payload: neutral("hi")}); err != nil {
			t.Fatal(err)
		}
		p := &poster{onFind: func(context.Context) error { return slackErr(code) }}
		ob, hooks, logs := abandoning(t, st, p, nil)
		for range 2 {
			if err := ob.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if posts, finds := p.counts(); posts != 0 || finds != 1 || !abandoned(t, st, "ev1/reply") {
			t.Errorf("%s: posts = %d, finds = %d, abandoned = %v; want a single lookup and an abandoned row", code, posts, finds, abandoned(t, st, "ev1/reply"))
		}
		if got := hooks.list(); len(got) != 1 || got[0] != "ev1/reply channel_gone" || warnings(logs) != 1 || !strings.Contains(logs.String(), "code="+code) {
			t.Errorf("%s: OnAbandon calls = %q, warnings = %d; want one of each", code, got, warnings(logs))
		}
	}
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	if _, err := st.CreateOutbox(t.Context(), store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", CreatedAt: t0, Payload: neutral("hi")}); err != nil {
		t.Fatal(err)
	}
	ob, hooks, _ := abandoning(t, st, &poster{onFind: func(context.Context) error { return slackErr("ratelimited") }}, nil)
	_ = ob.Reconcile(t.Context())
	if abandoned(t, st, "ev1/reply") || len(hooks.list()) != 0 {
		t.Error("a rate-limited lookup abandoned the row")
	}
}

// TestAShutdownMidAbandonStillRecordsIt: the context ends while the post is in flight and fails
// for good; the abandon is recorded all the same.
func TestAShutdownMidAbandonStillRecordsIt(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	ctx, cancel := context.WithCancel(t.Context())
	p := &poster{onPost: func(context.Context) error { cancel(); return slackErr("is_archived") }}
	ob, hooks, _ := abandoning(t, st, p, nil)
	if _, err := ob.Send(ctx, reply("ev1")); !errors.Is(err, outbox.ErrAbandoned) {
		t.Fatalf("Send = %v; want ErrAbandoned", err)
	}
	if !abandoned(t, st, "ev1/reply") || len(hooks.list()) != 1 {
		t.Fatalf("abandoned = %v, hooks %q; want the abandon recorded and reported", abandoned(t, st, "ev1/reply"), hooks.list())
	}
}

// TestThePurgeRunsAtMostEveryFiveMinutes: Reconcile runs on every signal and tick, and the purge is
// a write; a row that has just come due waits for the next one.
func TestThePurgeRunsAtMostEveryFiveMinutes(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	var clock atomic.Int64
	p := &poster{onPost: func(context.Context) error { return errors.New("connection reset") }}
	ob, hooks, _ := abandoning(t, st, p, func(c *outbox.Config) {
		c.Now = func() time.Time { return t0.Add(time.Duration(clock.Load())) }
	})
	if err := ob.Reconcile(t.Context()); err != nil { // the first one purges
		t.Fatal(err)
	}
	old := store.OutboxEntry{ID: "old/reply", EventID: "old", Role: store.OutboxReply, Channel: "C1", CreatedAt: t0.Add(-8 * 24 * time.Hour), Payload: neutral("hi")}
	if _, err := st.CreateOutbox(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		at   time.Duration
		gone bool
	}{{time.Minute, false}, {4 * time.Minute, false}, {5 * time.Minute, true}} {
		clock.Store(int64(step.at))
		_ = ob.Reconcile(t.Context())
		if got := abandoned(t, st, "old/reply"); got != step.gone {
			t.Fatalf("at %v abandoned = %v; want %v", step.at, got, step.gone)
		}
	}
	if got := hooks.list(); len(got) != 1 || got[0] != "old/reply "+outbox.AbandonExpired {
		t.Fatalf("OnAbandon calls = %q; want one", got)
	}
}
