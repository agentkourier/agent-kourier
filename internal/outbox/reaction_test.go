package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// reactor is a Reactor that records its calls as "channel ts name"; fail, if set, decides each
// call's result.
type reactor struct {
	mu    sync.Mutex
	calls []string
	fail  func(n int) error // n counts the calls from 1
}

func (r *reactor) React(_ context.Context, channel, ts, name string) error {
	r.mu.Lock()
	r.calls = append(r.calls, channel+" "+ts+" "+name)
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

func reacting(t *testing.T, st store.Outbox, p *poster, r outbox.Reactor) (*outbox.Outbox, *abandons, *syncBuffer) {
	t.Helper()
	return abandoning(t, st, p, func(c *outbox.Config) { c.Reactor = r })
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
		reason string // "" for a failure that is retried
	}{
		{slackErr("message_not_found"), "message_not_found"},
		{slackErr("invalid_name"), "invalid_name"},
		{slackErr("no_item_specified"), "no_item_specified"},
		{slackErr("not_in_channel"), "not_in_channel"},
		{slackErr("channel_not_found"), "channel_not_found"},
		{fmt.Errorf("react: %w", store.ErrInvalid), outbox.AbandonInvalid},
		{slackErr("ratelimited"), ""},
		{slackErr("invalid_auth"), ""},
		{errors.New("connection reset"), ""},
		{context.DeadlineExceeded, ""},
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

// TestReactionWithNoReactorIsAbandonedAsInvalid: nothing could ever make it, so it is given up on
// and logged, not kept for ever.
func TestReactionWithNoReactorIsAbandonedAsInvalid(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	o, hooks, logs := reacting(t, st, &poster{}, nil)
	if _, err := o.EnqueueReaction(t.Context(), queuedReaction); err != nil {
		t.Fatal(err)
	}
	id := queuedReaction.Source + "/reaction"
	if err := o.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	row, _ := st.GetOutbox(t.Context(), id)
	if got := hooks.list(); row.AbandonedAt.IsZero() || len(got) != 1 || got[0] != id+" "+outbox.AbandonInvalid || warnings(logs) != 1 || !strings.Contains(logs.String(), id) {
		t.Fatalf("row %+v, abandons %q, log %q; want it abandoned as invalid and logged", row, got, logs.String())
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
	text := outbox.Post{Source: "s", Role: store.OutboxReaction, Channel: "C1", Content: reply("s").Content}
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
