package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var roomy = store.PressLimits{Total: 100, PerUser: 100}

func press(id, user string, at time.Time) store.Press {
	return store.Press{InteractionID: id, User: user, Channel: "C1", MessageRef: "1700.01", Inputs: []byte(`{"q0":{"a0":"typed"}}`), PressedAt: at}
}

// Presses come back in the order they were recorded, whoever pressed, and survive a restart.
func TestPressesAreListedBySeqAndSurviveARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "courier.db")
	s := openAt(t, path)
	var seqs []int64
	for _, who := range []string{"U2", "U1", "U2", "U3"} {
		seq, err := s.RecordPress(ctx, press("ix-"+who, who, t0), roomy)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	if seqs[0] >= seqs[1] || seqs[1] >= seqs[2] || seqs[2] >= seqs[3] {
		t.Errorf("seqs = %v; want them increasing", seqs)
	}
	check := func(s *Store) {
		t.Helper()
		got, err := s.ListPresses(ctx)
		if err != nil || len(got) != 4 {
			t.Fatalf("ListPresses = %+v, %v; want 4", got, err)
		}
		for i, who := range []string{"U2", "U1", "U2", "U3"} {
			want := press("ix-"+who, who, t0)
			want.Seq = seqs[i]
			if !reflect.DeepEqual(got[i], want) {
				t.Errorf("press %d = %+v; want %+v", i, got[i], want)
			}
		}
	}
	check(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	check(openAt(t, path))
}

func TestRecordPressRefusesIncompletePresses(t *testing.T) {
	s := openTemp(t)
	for name, mutate := range map[string]func(*store.Press){
		"no interaction": func(p *store.Press) { p.InteractionID = "" }, "no user": func(p *store.Press) { p.User = "" },
		"no channel": func(p *store.Press) { p.Channel = "" }, "no message": func(p *store.Press) { p.MessageRef = "" },
		"no time": func(p *store.Press) { p.PressedAt = time.Time{} },
	} {
		p := press("ix", "U1", t0)
		mutate(&p)
		if _, err := s.RecordPress(t.Context(), p, roomy); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: err = %v; want ErrInvalid", name, err)
		}
	}
	if _, err := s.RecordPress(t.Context(), press("ix", "U1", t0), store.PressLimits{}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("no limits: err = %v; want ErrInvalid", err)
	}
	if got, _ := s.ListPresses(t.Context()); len(got) != 0 {
		t.Errorf("%d presses recorded from refused calls; want none", len(got))
	}
}

// Past either limit the press is refused, and a processed press frees its place.
func TestRecordPressLimits(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	limits := store.PressLimits{Total: 4, PerUser: 2}
	record := func(who string) (int64, error) { return s.RecordPress(ctx, press("ix", who, t0), limits) }
	first, _ := record("U1")
	for _, who := range []string{"U1", "U2", "U3"} {
		if _, err := record(who); err != nil {
			t.Fatalf("%s: %v", who, err)
		}
	}
	if _, err := record("U1"); !errors.Is(err, store.ErrLimit) {
		t.Errorf("a third press by one user and a fifth in all: %v; want ErrLimit", err)
	}
	if _, err := record("U4"); !errors.Is(err, store.ErrLimit) {
		t.Errorf("a fifth press in all: %v; want ErrLimit", err)
	}
	if err := s.DeletePress(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := record("U4"); err != nil {
		t.Errorf("after a press was processed: %v; want room", err)
	}
	if _, err := record("U3"); err == nil || !errors.Is(err, store.ErrLimit) {
		t.Errorf("full again: %v; want ErrLimit", err)
	}
	// The per-user limit binds with room in the store: U1 may not take a third place, U2 may.
	s = openTemp(t)
	limits = store.PressLimits{Total: 10, PerUser: 2}
	for range 2 {
		if _, err := record("U1"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := record("U1"); !errors.Is(err, store.ErrLimit) {
		t.Errorf("U1's third press with room in the store: %v; want ErrLimit", err)
	}
	if _, err := record("U2"); err != nil {
		t.Errorf("U2's first press: %v; want room", err)
	}
}

// A processed press is gone, and deleting it again is not an error.
func TestDeletePress(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	seq, _ := s.RecordPress(ctx, press("ix", "U1", t0), roomy)
	other, _ := s.RecordPress(ctx, press("ix", "U2", t0), roomy)
	for range 2 {
		if err := s.DeletePress(ctx, seq); err != nil {
			t.Fatalf("DeletePress: %v", err)
		}
	}
	if got, _ := s.ListPresses(ctx); len(got) != 1 || got[0].Seq != other {
		t.Errorf("presses = %+v; want only the other one", got)
	}
}

// A press is purged an hour after its interaction's deadline, and not before; one for an interaction
// that is not in the store counts from its own time.
func TestPurgePresses(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	i := newInteraction("ix-known", time.Hour) // due at t0 + 1 h
	if _, err := s.CreateInteraction(ctx, i); err != nil {
		t.Fatal(err)
	}
	known, _ := s.RecordPress(ctx, press("ix-known", "U1", t0.Add(30*time.Minute)), roomy)
	stray, _ := s.RecordPress(ctx, press("ix-unknown", "U1", t0.Add(2*time.Hour)), roomy) // pressed at t0 + 2 h
	fresh, _ := s.RecordPress(ctx, press("ix-unknown", "U2", t0.Add(4*time.Hour)), roomy)
	grace := time.Hour
	for _, step := range []struct {
		at   time.Time
		gone int
		left []int64
	}{
		{i.Deadline.Add(grace - time.Nanosecond), 0, []int64{known, stray, fresh}}, // a nanosecond early
		{i.Deadline.Add(grace), 1, []int64{stray, fresh}},                          // the known press, at its deadline + 1 h
		{t0.Add(3*time.Hour - time.Nanosecond), 0, []int64{stray, fresh}},
		{t0.Add(3 * time.Hour), 1, []int64{fresh}}, // the stray, an hour after it was pressed
	} {
		n, err := s.PurgePresses(ctx, step.at, grace)
		got, _ := s.ListPresses(ctx)
		var seqs []int64
		for _, p := range got {
			seqs = append(seqs, p.Seq)
		}
		if err != nil || n != step.gone || !reflect.DeepEqual(seqs, step.left) {
			t.Fatalf("purge at %v: %d gone, %v, left %v; want %d gone, left %v", step.at, n, err, seqs, step.gone, step.left)
		}
	}
	if _, err := s.PurgePresses(ctx, time.Time{}, grace); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("no time: %v; want ErrInvalid", err)
	}
}

// A press with no inputs is recorded, and comes back with none.
func TestRecordPressWithNilInputs(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	p := press("ix", "U1", t0)
	p.Inputs = nil
	if _, err := s.RecordPress(ctx, p, roomy); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListPresses(ctx); err != nil || len(got) != 1 || len(got[0].Inputs) != 0 {
		t.Errorf("presses = %+v, %v; want one with no inputs", got, err)
	}
}

// The limits hold when many presses arrive together: counting and inserting are one step, so exactly
// the limit succeeds and the rest are refused, for one person and for everyone.
func TestRecordPressLimitsHoldUnderConcurrency(t *testing.T) {
	for name, tc := range map[string]struct {
		limits store.PressLimits
		user   func(i int) string
	}{
		"one user":   {store.PressLimits{Total: 1000, PerUser: 10}, func(int) string { return "U1" }},
		"many users": {store.PressLimits{Total: 10, PerUser: 10}, func(i int) string { return fmt.Sprintf("U%d", i) }},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := openTemp(t)
			var wg sync.WaitGroup
			var ok, refused atomic.Int32
			for i := range 50 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					switch _, err := s.RecordPress(ctx, press("ix", tc.user(i), t0), tc.limits); {
					case err == nil:
						ok.Add(1)
					case errors.Is(err, store.ErrLimit):
						refused.Add(1)
					default:
						t.Errorf("RecordPress: %v", err)
					}
				}()
			}
			wg.Wait()
			if got, _ := s.ListPresses(ctx); ok.Load() != 10 || refused.Load() != 40 || len(got) != 10 {
				t.Errorf("%d recorded, %d refused, %d stored; want exactly 10, 40 and 10", ok.Load(), refused.Load(), len(got))
			}
		})
	}
}

// PendingPresses says whether a press of the interaction made at or before a time is waiting.
func TestPendingPresses(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	seq, _ := s.RecordPress(ctx, press("ix-1", "U1", t0.Add(time.Minute)), roomy)
	_, _ = s.RecordPress(ctx, press("ix-2", "U2", t0), roomy)
	for _, tc := range []struct {
		id     string
		before time.Time
		want   bool
	}{
		{"ix-1", t0.Add(time.Minute), true},                    // the boundary is inclusive
		{"ix-1", t0.Add(time.Minute - time.Nanosecond), false}, // a nanosecond before the press
		{"ix-1", t0.Add(time.Hour), true},
		{"ix-2", t0.Add(time.Minute), true},
		{"ix-3", t0.Add(time.Hour), false}, // no press of that interaction
	} {
		if got, err := s.PendingPresses(ctx, tc.id, tc.before); err != nil || got != tc.want {
			t.Errorf("PendingPresses(%s, %v) = %v, %v; want %v", tc.id, tc.before, got, err, tc.want)
		}
	}
	if err := s.DeletePress(ctx, seq); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PendingPresses(ctx, "ix-1", t0.Add(time.Hour)); got {
		t.Error("a processed press is still pending")
	}
	if _, err := s.PendingPresses(ctx, "", t0); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("no interaction: %v; want ErrInvalid", err)
	}
	if _, err := s.PendingPresses(ctx, "ix-1", time.Time{}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("no time: %v; want ErrInvalid", err)
	}
}

// Deleted content is overwritten: secure_delete is on for every connection, and the lookup the
// sweep makes by interaction has its index.
func TestSecureDeleteAndThePendingIndex(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	for name, db := range map[string]*sql.DB{"write": s.write, "read": s.read} {
		var on int
		if err := db.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&on); err != nil || on != 1 {
			t.Errorf("%s pool: secure_delete = %d, %v; want 1", name, on, err)
		}
	}
	var plan string
	if err := s.read.QueryRowContext(ctx, `EXPLAIN QUERY PLAN SELECT 1 FROM interaction_presses WHERE interaction_id = ? AND pressed_at <= ?`, "x", 0).Scan(new(int), new(int), new(int), &plan); err != nil || !strings.Contains(plan, "interaction_presses_interaction") {
		t.Errorf("query plan = %q, %v; want it to use interaction_presses_interaction", plan, err)
	}
}

func TestTimelyUnclaimedTextReplyDelaysTimeoutOnlyForTextQuestions(t *testing.T) {
	for _, textReplies := range []bool{false, true} {
		t.Run(fmt.Sprintf("text_replies_%v", textReplies), func(t *testing.T) {
			s := openTemp(t)
			ctx := t.Context()
			i := newInteraction("question", time.Minute)
			i.TextReplies = textReplies
			if _, err := s.CreateInteraction(ctx, i); err != nil {
				t.Fatal(err)
			}
			if err := s.SetInteractionMessageRef(ctx, i.ID, "prompt"); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name string
				at   time.Time
				want bool
			}{
				{"before question", t0.Add(-time.Second), false},
				{"after deadline", i.Deadline.Add(time.Nanosecond), false},
				{"in time", i.Deadline.Add(-time.Second), textReplies},
			} {
				r := newReply(tc.name, i.Key)
				r.EnqueuedAt = tc.at
				if _, err := s.EnqueueReply(ctx, r); err != nil {
					t.Fatal(err)
				}
				pending, err := s.PendingPresses(ctx, i.ID, i.Deadline)
				if err != nil || pending != tc.want {
					t.Fatalf("%s pending=%v error=%v", tc.name, pending, err)
				}
				if err := s.MarkReplyDelivered(ctx, r.ID, t0.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if pending, err := s.PendingPresses(ctx, i.ID, i.Deadline); err != nil || pending {
				t.Fatalf("consumed replies pending=%v error=%v", pending, err)
			}
		})
	}
}
