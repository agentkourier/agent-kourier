package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// newInteraction returns a valid interaction created at t0 that is due after d.
func newInteraction(id string, d time.Duration) store.Interaction {
	return store.Interaction{
		ID: id, Key: key, TaskID: "task1", Kind: store.InteractionAskUser,
		Request: []byte(`{"q":1}`), CreatedAt: t0, Deadline: t0.Add(d),
	}
}

func TestInteractionsCreateAndGet(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	i := newInteraction("i1", time.Hour)
	i.MessageRef = "ignored"
	i.State, i.Decider = store.InteractionAnswered, "U1" // ignored on create, like MessageRef
	if c, err := s.CreateInteraction(ctx, i); err != nil || !c {
		t.Fatalf("create = %v, %v; want true, nil", c, err)
	}
	dup := newInteraction("i1", 2*time.Hour)
	if c, err := s.CreateInteraction(ctx, dup); err != nil || c {
		t.Fatalf("duplicate create = %v, %v; want false, nil", c, err)
	}
	want := i
	want.State, want.Decider, want.MessageRef = store.InteractionPending, "", ""
	if got, err := s.GetInteraction(ctx, "i1"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("get = %+v, %v; want %+v", got, err, want)
	}
	if _, err := s.GetInteraction(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get missing = %v; want ErrNotFound", err)
	}

	for name, mutate := range map[string]func(*store.Interaction){
		"empty ID":        func(i *store.Interaction) { i.ID = "" },
		"empty key part":  func(i *store.Interaction) { i.Key.ThreadTS = "" },
		"empty task ID":   func(i *store.Interaction) { i.TaskID = "" },
		"unknown kind":    func(i *store.Interaction) { i.Kind = "bogus" },
		"zero created":    func(i *store.Interaction) { i.CreatedAt = time.Time{} },
		"zero deadline":   func(i *store.Interaction) { i.Deadline = time.Time{} },
		"deadline = made": func(i *store.Interaction) { i.Deadline = i.CreatedAt },
	} {
		bad := newInteraction("bad", time.Hour)
		mutate(&bad)
		if _, err := s.CreateInteraction(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: create = %v; want ErrInvalid", name, err)
		}
	}
}

func TestListDueInteractions(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	for _, i := range []store.Interaction{newInteraction("late", 3*time.Hour), newInteraction("due", time.Hour), newInteraction("soon", 2*time.Hour)} {
		if _, err := s.CreateInteraction(ctx, i); err != nil {
			t.Fatalf("create %s: %v", i.ID, err)
		}
	}
	ids := func(now time.Time) (out []string) {
		t.Helper()
		l, err := s.ListDueInteractions(ctx, now)
		if err != nil {
			t.Fatalf("ListDueInteractions: %v", err)
		}
		for _, i := range l {
			out = append(out, i.ID)
		}
		return out
	}
	// A deadline equal to now is due; one nanosecond later is not.
	for _, c := range []struct {
		now  time.Time
		want []string
	}{
		{t0.Add(time.Hour - 1), nil},
		{t0.Add(time.Hour), []string{"due"}},
		{t0.Add(2 * time.Hour), []string{"due", "soon"}},
		{t0.Add(4 * time.Hour), []string{"due", "soon", "late"}},
	} {
		if got := ids(c.now); !reflect.DeepEqual(got, c.want) {
			t.Errorf("due at %v = %v; want %v", c.now.Sub(t0), got, c.want)
		}
	}
	// Decided rows are never due.
	if _, err := s.write.ExecContext(ctx, `UPDATE interactions SET state = 'answered' WHERE id = 'due'`); err != nil {
		t.Fatalf("mark answered: %v", err)
	}
	if got := ids(t0.Add(4 * time.Hour)); !reflect.DeepEqual(got, []string{"soon", "late"}) {
		t.Errorf("due after answering = %v; want soon, late", got)
	}
	if _, err := s.ListDueInteractions(ctx, time.Time{}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("zero now = %v; want ErrInvalid", err)
	}
}
