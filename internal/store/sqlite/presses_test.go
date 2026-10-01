package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var roomy = store.PressLimits{Total: 100, PerUser: 100}

func press(id, user string, at time.Time) store.Press {
	return store.Press{InteractionID: id, User: user, Channel: "C1", MessageTS: "1700.01", Inputs: []byte(`{"q0":{"a0":"typed"}}`), PressedAt: at}
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
		"no channel": func(p *store.Press) { p.Channel = "" }, "no message": func(p *store.Press) { p.MessageTS = "" },
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
