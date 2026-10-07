package storetest

import (
	"math"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// limits pins the one rule for the size of a batch, the same for every store: a limit is 1 to store.MaxLimit. Postgres
// binds it as 32 bits and SQLite would take more, which once made the stores answer differently; a limit above the
// bound is store.ErrInvalid and changes nothing, and the largest legal one is a batch like any other.
func (su suite) limits(t *testing.T) {
	horizons := store.SessionHorizons{Otherwise: t0, ByBinding: map[string]time.Time{"ns/b": t0}}
	batches := []struct {
		name string
		call func(s store.Store, limit int) error
	}{
		{"PurgeAudit", func(s store.Store, limit int) error { _, err := s.PurgeAudit(t.Context(), t0, limit); return err }},
		{"PurgeExpiredSessions", func(s store.Store, limit int) error {
			_, err := s.PurgeExpiredSessions(t.Context(), horizons, limit)
			return err
		}},
		{"ListStaleTasks", func(s store.Store, limit int) error {
			_, err := s.ListStaleTasks(t.Context(), horizons, limit)
			return err
		}},
	}
	for _, b := range batches {
		su.run(t, b.name+" accepts a limit up to store.MaxLimit and refuses one above it", func(t *testing.T, s store.Store) {
			noErr(t, b.call(s, 1), "a limit of 1")
			noErr(t, b.call(s, store.MaxLimit), "the largest limit")
			isErr(t, b.call(s, store.MaxLimit+1), store.ErrInvalid, "a limit above store.MaxLimit")
			isErr(t, b.call(s, 0), store.ErrInvalid, "a limit of 0")
		})
	}
}

// times pins the one rule for a time that is set, the same for every store: after the Unix epoch and no later than
// time.Unix(0, math.MaxInt64), which is the latest time that SQLite keeps (a later one wraps and reads back as an earlier
// one, and Postgres is held to the same bound so that the stores answer alike). Each edge is tried on a call that
// changes nothing visible.
func (su suite) times(t *testing.T) {
	latest := time.Unix(0, math.MaxInt64)
	calls := []struct {
		name string
		call func(s store.Store, at time.Time) error
	}{
		{"PurgeTriggerRuns", func(s store.Store, at time.Time) error { _, err := s.PurgeTriggerRuns(t.Context(), at); return err }},
		{"ListTriggerDigests", func(s store.Store, at time.Time) error { _, err := s.ListTriggerDigests(t.Context(), at); return err }},
		{"PurgeTriggerThreads", func(s store.Store, at time.Time) error {
			_, err := s.PurgeTriggerThreads(t.Context(), at, time.Time{})
			return err
		}},
		{"AbandonOutboxBefore", func(s store.Store, at time.Time) error {
			_, err := s.AbandonOutboxBefore(t.Context(), at, t0)
			return err
		}},
	}
	for _, c := range calls {
		su.run(t, c.name+" accepts a time after the epoch up to the latest SQLite holds, and refuses the rest", func(t *testing.T, s store.Store) {
			noErr(t, c.call(s, time.Unix(0, 1)), "a nanosecond past the epoch")
			noErr(t, c.call(s, latest), "the latest time")
			isErr(t, c.call(s, time.Unix(0, 0)), store.ErrInvalid, "the epoch")
			isErr(t, c.call(s, latest.Add(time.Nanosecond)), store.ErrInvalid, "a nanosecond past the latest time")
			isErr(t, c.call(s, time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)), store.ErrInvalid, "the year 2300")
		})
	}
}
