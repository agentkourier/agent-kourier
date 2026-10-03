package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// reactor is a Reactor that records its calls as "channel ts name", and those that take a reaction off
// as "- channel ts name"; fail, if set, decides each call's result.
type reactor struct {
	mu    sync.Mutex
	calls []string
	fail  func(n int) error // n counts the calls, of both kinds, from 1
}

func (r *reactor) React(_ context.Context, channel, ts, name string) error {
	return r.call(channel + " " + ts + " " + name)
}

func (r *reactor) Unreact(_ context.Context, channel, ts, name string) error {
	return r.call("- " + channel + " " + ts + " " + name)
}

func (r *reactor) call(what string) error {
	r.mu.Lock()
	r.calls = append(r.calls, what)
	n, fail := len(r.calls), r.fail
	r.mu.Unlock()
	if fail != nil {
		return fail(n)
	}
	return nil
}

func (r *reactor) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

var queuedReaction = outbox.Reaction{Source: "slack:ns/conn:C1:1.5/queued", Channel: "C1", TS: "1.5", Name: "hourglass"}

func reacting(t *testing.T, st store.Outbox, p *poster, r reacts) (*outbox.Outbox, *abandons, *syncBuffer) {
	t.Helper()
	p.reactor = r
	return abandoning(t, st, p, nil)
}

// TestReactionIsEnqueuedOnceByIDAndMadeOnce: the row is named after its source and the role, a second
// enqueue of the same reaction is the same row, and the drain reacts once, with no post or lookup,
// and records the message's ts as the ref, so no later drain reacts again.
func TestReactionIsEnqueuedOnceByIDAndMadeOnce(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	p, r := &poster{}, &reactor{}
	o, _, _ := reacting(t, st, p, r)
	for range 2 {
		id, err := o.EnqueueReaction(t.Context(), queuedReaction)
		if want := queuedReaction.Source + "/reaction"; err != nil || id != want {
			t.Fatalf("EnqueueReaction = %q, %v; want %q", id, err, want)
		}
	}
	if rows, _ := st.ListUnposted(t.Context()); len(rows) != 1 || rows[0].Role != store.OutboxReaction || rows[0].Channel != "C1" {
		t.Fatalf("unposted = %+v; want one reaction row", rows)
	}
	if got := r.list(); len(got) != 0 {
		t.Fatalf("EnqueueReaction reacted (%q); it is a local write", got)
	}
	for range 2 {
		if err := o.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile = %v", err)
		}
	}
	if got := r.list(); len(got) != 1 || got[0] != "C1 1.5 hourglass" {
		t.Fatalf("reactions made = %q; want one, on C1 1.5", got)
	}
	if posts, finds := p.counts(); posts != 0 || finds != 0 {
		t.Fatalf("%d posts and %d lookups for a reaction; want none", posts, finds)
	}
	if got := ref(st, queuedReaction.Source+"/reaction"); got != "1.5" {
		t.Fatalf("ref = %q; want the reacted message's ts", got)
	}
}

// TestReactionIsDoneWhenReactIsDone: React reports success for a reaction that is there already
// (the Slack client turns already_reacted into nil), so the row is recorded and nothing is retried.
func TestReactionIsDoneWhenReactIsDone(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	o, hooks, _ := reacting(t, st, &poster{}, &reactor{})
	if _, err := o.EnqueueReaction(t.Context(), queuedReaction); err != nil {
		t.Fatal(err)
	}
	if err := o.Reconcile(t.Context()); err != nil || ref(st, queuedReaction.Source+"/reaction") == "" || len(hooks.list()) != 0 {
		t.Fatalf("Reconcile = %v, ref %q, abandons %q; want it done", err, ref(st, queuedReaction.Source+"/reaction"), hooks.list())
	}
}

// TestPermanentReactionFailuresAbandonTheRow: a reaction that can never be made is given up on, once,
// and never tried again; any other failure leaves the row for a retry.
func TestPermanentReactionFailuresAbandonTheRow(t *testing.T) {
	id := queuedReaction.Source + "/reaction"
	for _, tc := range []struct {
		err    error
		reason string // "" for a failure that is retried; neutral, as OnAbandon is told it
		code   string // the platform's own code, for the log
	}{
		{slackErr("message_not_found"), "channel_gone", "message_not_found"},
		{slackErr("invalid_name"), "invalid_content", "invalid_name"},
		{slackErr("no_item_specified"), "invalid_content", "no_item_specified"},
		{slackErr("not_in_channel"), "channel_gone", "not_in_channel"},
		{slackErr("channel_not_found"), "channel_gone", "channel_not_found"},
		{slackErr("thread_locked"), "other", "thread_locked"},
		{fmt.Errorf("react: %w", store.ErrInvalid), outbox.AbandonInvalid, ""},
		{slackErr("ratelimited"), "", ""},
		{slackErr("invalid_auth"), "", ""},
		{errors.New("connection reset"), "", ""},
		{context.DeadlineExceeded, "", ""},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
			r := &reactor{fail: func(int) error { return tc.err }}
			o, hooks, logs := reacting(t, st, &poster{}, r)
			if _, err := o.EnqueueReaction(t.Context(), queuedReaction); err != nil {
				t.Fatal(err)
			}
			err := o.Reconcile(t.Context())
			row, _ := st.GetOutbox(t.Context(), id)
			if tc.reason == "" {
				if err == nil || !row.AbandonedAt.IsZero() || row.Payload == "" || len(hooks.list()) != 0 {
					t.Fatalf("Reconcile = %v, row %+v, abandons %q; want a plain failure and an untouched row", err, row, hooks.list())
				}
				return
			}
			if err != nil || row.AbandonedAt.IsZero() || row.Payload != "" || row.MessageRef != "" {
				t.Fatalf("Reconcile = %v, row %+v; want it abandoned, with no error from the drain", err, row)
			}
			if got := hooks.list(); len(got) != 1 || got[0] != id+" "+tc.reason || warnings(logs) != 1 {
				t.Fatalf("abandons %q, %d warnings; want %q once", got, warnings(logs), id+" "+tc.reason)
			}
			if tc.code != "" && !strings.Contains(logs.String(), "code="+tc.code) {
				t.Fatalf("log %q; want the platform's own code, %s", logs.String(), tc.code)
			}
			if err := o.Reconcile(t.Context()); err != nil || len(r.list()) != 1 {
				t.Fatalf("a second Reconcile = %v after %d calls; want no more", err, len(r.list()))
			}
		})
	}
}

// failRecord is a store whose first RecordMessageRef fails, as a crash right after the reaction would.
type failRecord struct {
	store.Outbox
	failed bool
}

func (s *failRecord) RecordMessageRef(ctx context.Context, id, ref string) error {
	if !s.failed {
		s.failed = true
		return errors.New("disk gone")
	}
	return s.Outbox.RecordMessageRef(ctx, id, ref)
}

// TestReactionIsRetriedAfterACrash: a row whose reaction failed, or whose ref was never recorded,
// stays unposted, and the next process makes the reaction (again, which Slack takes as done) and
// records it.
func TestReactionIsRetriedAfterACrash(t *testing.T) {
	id := queuedReaction.Source + "/reaction"
	for name, failFirst := range map[string]func(*reactor, *failRecord){
		"the reaction failed":   func(r *reactor, _ *failRecord) { r.fail = func(int) error { return errors.New("connection reset") } },
		"the ref was not saved": func(*reactor, *failRecord) {},
	} {
		t.Run(name, func(t *testing.T) {
			base := openStore(t, filepath.Join(t.TempDir(), "c.db"))
			st := &failRecord{Outbox: base, failed: name == "the reaction failed"}
			r := &reactor{}
			failFirst(r, st)
			first, _, _ := reacting(t, st, &poster{}, r)
			if _, err := first.EnqueueReaction(t.Context(), queuedReaction); err != nil {
				t.Fatal(err)
			}
			if err := first.Reconcile(t.Context()); err == nil || ref(base, id) != "" {
				t.Fatalf("Reconcile = %v, ref %q; want a failure and no ref", err, ref(base, id))
			}
			r.mu.Lock()
			r.fail = nil
			r.mu.Unlock()
			second, _, _ := reacting(t, st, &poster{}, r) // the restarted process
			if err := second.Reconcile(t.Context()); err != nil || ref(base, id) != "1.5" {
				t.Fatalf("Reconcile after the restart = %v, ref %q; want it made and recorded", err, ref(base, id))
			}
			if got := r.list(); len(got) != 2 {
				t.Fatalf("reactions made = %q; want two attempts", got)
			}
		})
	}
}

// TestEnqueueReactionRefusesWhatCannotBeMade: an empty field is ErrInvalid and leaves no row, and
// Enqueue and Send, which carry text, refuse the reaction role.
func TestEnqueueReactionRefusesWhatCannotBeMade(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	o, _, _ := reacting(t, st, &poster{}, &reactor{})
	for _, mod := range []func(*outbox.Reaction){
		func(r *outbox.Reaction) { r.Source = "" }, func(r *outbox.Reaction) { r.Channel = "" },
		func(r *outbox.Reaction) { r.TS = "" }, func(r *outbox.Reaction) { r.Name = "" },
	} {
		bad := queuedReaction
		mod(&bad)
		if id, err := o.EnqueueReaction(t.Context(), bad); !errors.Is(err, store.ErrInvalid) || id != "" {
			t.Errorf("EnqueueReaction(%+v) = %q, %v; want ErrInvalid", bad, id, err)
		}
	}
	text := outbox.Post{Source: "s", Role: store.OutboxReaction, Channel: "C1", Out: reply("s").Out}
	if _, err := o.Enqueue(t.Context(), text); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("Enqueue with the reaction role = %v; want ErrInvalid", err)
	}
	if _, err := o.Send(t.Context(), text); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("Send with the reaction role = %v; want ErrInvalid", err)
	}
	if rows, _ := st.ListUnposted(t.Context()); len(rows) != 0 {
		t.Fatalf("%d rows left by refused reactions", len(rows))
	}
}

// TestRunMakesAnEnqueuedReaction: the signal of EnqueueReaction wakes Run, though its ticker is an
// hour away.
func TestRunMakesAnEnqueuedReaction(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	r := &reactor{}
	o, _, _ := reacting(t, st, &poster{}, r)
	start(t, o)
	if _, err := o.EnqueueReaction(t.Context(), queuedReaction); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the reaction", func() bool { return ref(st, queuedReaction.Source+"/reaction") == "1.5" })
}

// TestAHungReactionEndsAtCallTimeout: React does not bound its own call, so the outbox does, as for a
// post: a Slack that never answers must not hold the delivery lock for ever. The row stays unposted,
// for a retry, and the error is the timeout.
func TestAHungReactionEndsAtCallTimeout(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	o, hooks, _ := abandoning(t, st, &poster{reactor: hangingReactor{}}, func(c *outbox.Config) { c.CallTimeout = 50 * time.Millisecond })
	if _, err := o.EnqueueReaction(t.Context(), queuedReaction); err != nil {
		t.Fatal(err)
	}
	err := o.Reconcile(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) || ref(st, queuedReaction.Source+"/reaction") != "" || len(hooks.list()) != 0 {
		t.Fatalf("Reconcile = %v, ref %q, abandons %q; want the timeout, an unposted row, and no abandon", err, ref(st, queuedReaction.Source+"/reaction"), hooks.list())
	}
}

// hangingReactor waits for its context to end, as a Slack that never answers does.
type hangingReactor struct{}

func (hangingReactor) React(ctx context.Context, _, _, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func (hangingReactor) Unreact(ctx context.Context, _, _, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

var (
	reactionID   = queuedReaction.Source + "/reaction"
	unreactionID = queuedReaction.Source + "/unreaction"
)

// TestUnreactionIsEnqueuedOnceByIDAndTakesTheReactionOffAfterItIsMade: the removal row is named after the
// reaction's source, so asking twice is one row, and it is a local write. The drain makes the reaction and then
// takes it off, in that order, with no post or lookup, and records the message's ts on each.
func TestUnreactionIsEnqueuedOnceByIDAndTakesTheReactionOffAfterItIsMade(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	p, r := &poster{}, &reactor{}
	o, _, _ := reacting(t, st, p, r)
	if _, err := o.EnqueueReaction(t.Context(), queuedReaction); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		id, err := o.EnqueueUnreaction(t.Context(), queuedReaction)
		if err != nil || id != unreactionID || id != outbox.UnreactionID(queuedReaction.Source) {
			t.Fatalf("EnqueueUnreaction = %q, %v; want %q", id, err, unreactionID)
		}
	}
	if rows, _ := st.ListUnposted(t.Context()); len(rows) != 2 {
		t.Fatalf("unposted = %+v; want the reaction and its removal", rows)
	}
	if got := r.list(); len(got) != 0 {
		t.Fatalf("EnqueueUnreaction reacted (%q); it is a local write", got)
	}
	for range 2 {
		if err := o.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile = %v", err)
		}
	}
	if got, want := r.list(), []string{"C1 1.5 hourglass", "- C1 1.5 hourglass"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %q; want %q, in that order and once each", got, want)
	}
	if posts, finds := p.counts(); posts != 0 || finds != 0 {
		t.Fatalf("%d posts and %d lookups for a removal; want none", posts, finds)
	}
	if ref(st, reactionID) != "1.5" || ref(st, unreactionID) != "1.5" {
		t.Fatalf("refs = %q, %q; want the message's ts on both", ref(st, reactionID), ref(st, unreactionID))
	}
}

// TestUnreactionCancelsAReactionThatIsNotYetMade is the ordering rule: a reaction that failed and waits for its
// retry must not be made after its removal, or it would stay for good. The removal cancels it (no abandon is
// reported, since nobody gave up) and takes off whatever a cut-short delivery may have made.
func TestUnreactionCancelsAReactionThatIsNotYetMade(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	r := &reactor{fail: func(n int) error {
		if n == 1 {
			return errors.New("connection reset") // the reaction's first try
		}
		return nil
	}}
	o, hooks, logs := reacting(t, st, &poster{}, r)
	if _, err := o.EnqueueReaction(t.Context(), queuedReaction); err != nil {
		t.Fatal(err)
	}
	if err := o.Reconcile(t.Context()); err == nil { // it fails, and waits out a retry delay that the frozen clock never ends
		t.Fatal("Reconcile succeeded; want the reaction's failure")
	}
	if _, err := o.EnqueueUnreaction(t.Context(), queuedReaction); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := o.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile = %v", err)
		}
	}
	if got, want := r.list(), []string{"C1 1.5 hourglass", "- C1 1.5 hourglass"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %q; want the failed try and the removal, and no later reaction", got)
	}
	row, _ := st.GetOutbox(t.Context(), reactionID)
	if row.AbandonedAt.IsZero() || row.MessageRef != "" || ref(st, unreactionID) != "1.5" {
		t.Fatalf("reaction row %+v, removal ref %q; want the reaction cancelled and the removal done", row, ref(st, unreactionID))
	}
	if got := hooks.list(); len(got) != 0 || warnings(logs) != 0 {
		t.Fatalf("abandons %q, %d warnings; want a cancel to be neither", got, warnings(logs))
	}
}

// TestUnreactionEnqueuedFirstStopsTheReaction: the order of the two enqueues does not matter. A reaction that
// arrives after its removal is not made and gets no row, and the removal, which finds nothing to take off, asks
// the platform for nothing.
func TestUnreactionEnqueuedFirstStopsTheReaction(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	r := &reactor{}
	o, _, _ := reacting(t, st, &poster{}, r)
	if _, err := o.EnqueueUnreaction(t.Context(), queuedReaction); err != nil {
		t.Fatal(err)
	}
	if id, err := o.EnqueueReaction(t.Context(), queuedReaction); err != nil || id != reactionID {
		t.Fatalf("EnqueueReaction after its removal = %q, %v; want its ID, and no error", id, err)
	}
	if _, err := st.GetOutbox(t.Context(), reactionID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the reaction's row = %v; want none", err)
	}
	for range 2 {
		if err := o.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile = %v", err)
		}
	}
	if got := r.list(); len(got) != 0 {
		t.Fatalf("calls = %q; want none: nothing was made, so nothing is taken off", got)
	}
	if ref(st, unreactionID) != "1.5" {
		t.Fatalf("removal ref = %q; want it done", ref(st, unreactionID))
	}
}

// TestUnreactionAsksUnlessNoReactionWasEverEnqueued: the removal of a reaction that was never enqueued takes
// nothing off. For one that was, in whatever state, the platform is asked, even if the platform refused it for
// good: that costs a call, and the removal cannot tell such a row from one an earlier delivery of the removal
// cancelled (see TestUnreactionIsAskedAgainAfterItCancelledTheReactionAndFailed).
func TestUnreactionAsksUnlessNoReactionWasEverEnqueued(t *testing.T) {
	for _, tc := range []struct {
		name      string
		react     bool // the reaction is enqueued
		failReact bool // and refused for good
		want      []string
	}{
		{"never enqueued", false, false, nil},
		{"refused for good", true, true, []string{"C1 1.5 hourglass", "- C1 1.5 hourglass"}},
		{"made", true, false, []string{"C1 1.5 hourglass", "- C1 1.5 hourglass"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
			r := &reactor{fail: func(int) error {
				if tc.failReact {
					return slackErr("message_not_found")
				}
				return nil
			}}
			o, _, _ := reacting(t, st, &poster{}, r)
			if tc.react {
				if _, err := o.EnqueueReaction(t.Context(), queuedReaction); err != nil {
					t.Fatal(err)
				}
				_ = o.Reconcile(t.Context())
			}
			if tc.failReact {
				r.mu.Lock()
				r.fail = nil
				r.mu.Unlock()
			}
			if _, err := o.EnqueueUnreaction(t.Context(), queuedReaction); err != nil {
				t.Fatal(err)
			}
			if err := o.Reconcile(t.Context()); err != nil {
				t.Fatalf("Reconcile = %v", err)
			}
			if got := r.list(); !slices.Equal(got, tc.want) {
				t.Fatalf("calls = %q; want %q", got, tc.want)
			}
			if ref(st, unreactionID) != "1.5" {
				t.Fatalf("removal ref = %q; want it done", ref(st, unreactionID))
			}
		})
	}
}

// TestUnreactionIsAskedAgainAfterItCancelledTheReactionAndFailed: the reaction's call failed in a way that leaves
// it unknown whether it was made (a reset, a timeout), and the first call to take it off failed too, after the
// removal had cancelled the reaction. The retry, here in a restarted process, finds the reaction abandoned, and
// must still ask Slack: the reaction may be there, and would stay for good.
func TestUnreactionIsAskedAgainAfterItCancelledTheReactionAndFailed(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	r := &reactor{fail: func(n int) error {
		if n <= 2 { // the reaction, then the first removal
			return errors.New("connection reset")
		}
		return nil
	}}
	first, _, _ := reacting(t, st, &poster{}, r)
	for _, enqueue := range []func(context.Context, outbox.Reaction) (string, error){first.EnqueueReaction, first.EnqueueUnreaction} {
		if _, err := enqueue(t.Context(), queuedReaction); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Reconcile(t.Context()); err == nil {
		t.Fatal("Reconcile succeeded; want the failures")
	}
	if row, _ := st.GetOutbox(t.Context(), reactionID); row.AbandonedAt.IsZero() || ref(st, unreactionID) != "" {
		t.Fatalf("reaction row %+v, removal ref %q; want the reaction cancelled and the removal not done", row, ref(st, unreactionID))
	}
	second, _, _ := reacting(t, st, &poster{}, r) // the restarted process
	if err := second.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile after the restart = %v", err)
	}
	if got, want := r.list(), []string{"C1 1.5 hourglass", "- C1 1.5 hourglass", "- C1 1.5 hourglass"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %q; want the removal asked of Slack again, %q", got, want)
	}
	if ref(st, unreactionID) != "1.5" {
		t.Fatalf("removal ref = %q; want it done", ref(st, unreactionID))
	}
}

// TestPermanentUnreactionFailuresAbandonTheRow: a removal that can never be made is given up on, once; any
// other failure leaves the row for a retry, and the reaction stays cancelled.
func TestPermanentUnreactionFailuresAbandonTheRow(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string // "" for a failure that is retried
	}{
		{slackErr("message_not_found"), "channel_gone"},
		{slackErr("invalid_name"), "invalid_content"},
		{slackErr("ratelimited"), ""},
		{errors.New("connection reset"), ""},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
			r := &reactor{}
			o, hooks, _ := reacting(t, st, &poster{}, r)
			for _, enqueue := range []func(context.Context, outbox.Reaction) (string, error){o.EnqueueReaction, o.EnqueueUnreaction} {
				if _, err := enqueue(t.Context(), queuedReaction); err != nil {
					t.Fatal(err)
				}
			}
			r.fail = func(n int) error {
				if n == 2 { // the removal
					return tc.err
				}
				return nil
			}
			err := o.Reconcile(t.Context())
			row, _ := st.GetOutbox(t.Context(), unreactionID)
			if tc.reason == "" {
				if err == nil || !row.AbandonedAt.IsZero() || row.Payload == "" || len(hooks.list()) != 0 {
					t.Fatalf("Reconcile = %v, row %+v, abandons %q; want a plain failure and an untouched row", err, row, hooks.list())
				}
				return
			}
			if err != nil || row.AbandonedAt.IsZero() {
				t.Fatalf("Reconcile = %v, row %+v; want it abandoned, with no error from the drain", err, row)
			}
			if got := hooks.list(); len(got) != 1 || got[0] != unreactionID+" "+tc.reason {
				t.Fatalf("abandons %q; want %q once", got, unreactionID+" "+tc.reason)
			}
		})
	}
}

// TestEnqueueUnreactionRefusesWhatCannotBeMade: an empty field is ErrInvalid and leaves no row.
func TestEnqueueUnreactionRefusesWhatCannotBeMade(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	o, _, _ := reacting(t, st, &poster{}, &reactor{})
	for _, mod := range []func(*outbox.Reaction){
		func(r *outbox.Reaction) { r.Source = "" }, func(r *outbox.Reaction) { r.Channel = "" },
		func(r *outbox.Reaction) { r.TS = "" }, func(r *outbox.Reaction) { r.Name = "" },
	} {
		bad := queuedReaction
		mod(&bad)
		if id, err := o.EnqueueUnreaction(t.Context(), bad); !errors.Is(err, store.ErrInvalid) || id != "" {
			t.Errorf("EnqueueUnreaction(%+v) = %q, %v; want ErrInvalid", bad, id, err)
		}
	}
	if rows, _ := st.ListUnposted(t.Context()); len(rows) != 0 {
		t.Fatalf("%d rows left by refused removals", len(rows))
	}
}

// TestRunTakesAReactionOff: the signal of EnqueueUnreaction wakes Run too.
func TestRunTakesAReactionOff(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	r := &reactor{}
	o, _, _ := reacting(t, st, &poster{}, r)
	start(t, o)
	for _, enqueue := range []func(context.Context, outbox.Reaction) (string, error){o.EnqueueReaction, o.EnqueueUnreaction} {
		if _, err := enqueue(t.Context(), queuedReaction); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the removal", func() bool { return ref(st, unreactionID) == "1.5" })
	if got, want := r.list(), []string{"C1 1.5 hourglass", "- C1 1.5 hourglass"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %q; want %q", got, want)
	}
}

// TestAHungUnreactionEndsAtCallTimeout: as for a reaction, the outbox bounds the call, and the row stays for a
// retry.
func TestAHungUnreactionEndsAtCallTimeout(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	o, hooks, _ := abandoning(t, st, &poster{reactor: hangingReactor{}}, func(c *outbox.Config) { c.CallTimeout = 50 * time.Millisecond })
	if _, err := o.EnqueueReaction(t.Context(), queuedReaction); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetOutbox(t.Context(), reactionID); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordMessageRef(t.Context(), reactionID, "1.5"); err != nil { // made
		t.Fatal(err)
	}
	if _, err := o.EnqueueUnreaction(t.Context(), queuedReaction); err != nil {
		t.Fatal(err)
	}
	err := o.Reconcile(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) || ref(st, unreactionID) != "" || len(hooks.list()) != 0 {
		t.Fatalf("Reconcile = %v, ref %q, abandons %q; want the timeout, an unposted row, and no abandon", err, ref(st, unreactionID), hooks.list())
	}
}
