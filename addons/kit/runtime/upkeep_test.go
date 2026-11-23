package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/kit/store"
)

// releaser lets go of each context but those it is told to fail, and those it is told have a turn running.
type releaser struct {
	fail     map[string]bool
	busy     map[string]bool
	released []string
}

func (r *releaser) ReleaseContext(_ context.Context, c store.Context) error {
	if r.fail[c.ContextID] {
		return errors.New("the backend could not let go")
	}
	if r.busy[c.ContextID] {
		return fmt.Errorf("context %s: %w", c.ContextID, ErrContextBusy)
	}
	r.released = append(r.released, c.ContextID)
	return nil
}

type reporter struct {
	n   int
	err error
}

func (r *reporter) IdleContextsDeleted(n int, err error) { r.n, r.err = n, err }

// With a ContextReleaser, the sweep has the backend let go of each idle context, then forgets the context; one the
// backend could not let go of is left for the next sweep. Without one, the store deletes the idle contexts and the
// IdleReporter is told how many.
func TestTheSweepReleasesIdleContexts(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	t.Run("with a releaser", func(t *testing.T) {
		st := newFakeStore()
		st.idle = []store.Context{{Owner: "o", ContextID: "c1", Ref: "s1"}, {Owner: "o", ContextID: "c2", Ref: "s2"}}
		var logs syncBuffer
		r := &releaser{fail: map[string]bool{"c2": true}}
		u := &upkeep{store: st, releaser: r, idle: time.Hour, now: func() time.Time { return now }, log: slog.New(slog.NewJSONHandler(&logs, nil))}
		u.sweep(t.Context())
		if !slices.Equal(r.released, []string{"c1"}) || !slices.Equal(st.forgotten, []string{"c1"}) {
			t.Fatalf("released %q, forgotten %q; want c1 alone", r.released, st.forgotten)
		}
		if !strings.Contains(logs.String(), `"msg":"release an idle context; will retry","context":"c2"`) {
			t.Fatalf("the log = %s", logs.String())
		}
	})
	// A context whose turn has begun since the store listed it is left for a later sweep, logged at DEBUG: the race is
	// normal, and a WARN for it would cry wolf.
	t.Run("a context whose turn has begun", func(t *testing.T) {
		st := newFakeStore()
		st.idle = []store.Context{{Owner: "o", ContextID: "c1", Ref: "s1"}, {Owner: "o", ContextID: "c3", Ref: "s3"}}
		var logs syncBuffer
		r := &releaser{busy: map[string]bool{"c3": true}}
		u := &upkeep{store: st, releaser: r, idle: time.Hour, now: func() time.Time { return now },
			log: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
		u.sweep(t.Context())
		if !slices.Equal(r.released, []string{"c1"}) || !slices.Equal(st.forgotten, []string{"c1"}) {
			t.Fatalf("released %q, forgotten %q; want c1 alone", r.released, st.forgotten)
		}
		if !strings.Contains(logs.String(), `"level":"DEBUG","msg":"an idle context's turn has begun; left for a later sweep","context":"c3"`) ||
			strings.Contains(logs.String(), `"level":"WARN"`) {
			t.Fatalf("the log = %s; want c3 at DEBUG and no WARN", logs.String())
		}
	})
	t.Run("without one", func(t *testing.T) {
		st := newFakeStore()
		st.deleted = 3
		rep := &reporter{}
		u := &upkeep{store: st, reporter: rep, idle: time.Hour, now: func() time.Time { return now }, log: slog.New(slog.DiscardHandler)}
		u.sweep(t.Context())
		if rep.n != 3 || rep.err != nil || len(st.forgotten) != 0 {
			t.Fatalf("reported %d, %v; forgotten %q", rep.n, rep.err, st.forgotten)
		}
	})
}
