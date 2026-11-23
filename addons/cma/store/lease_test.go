package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
)

// openPostgresWith opens another store on dsn, as a second replica would, with opts.
func openPostgresWith(t *testing.T, dsn string, opts Options) *Store {
	t.Helper()
	s, err := OpenPostgres(context.Background(), dsn, opts)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustLead(t *testing.T, s *Store, holder string, d time.Duration) kitstore.Lease {
	t.Helper()
	l, ok, err := s.TryLead(context.Background(), holder, "http://"+holder+":8090", d)
	if err != nil || !ok {
		t.Fatalf("TryLead(%s) = %+v, %v, %v; want the lease", holder, l, ok, err)
	}
	return l
}

func createTask(ctx context.Context, s *Store, id string) error {
	_, err := s.Create(ctx, &a2a.Task{ID: a2a.TaskID(id), ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	return err
}

func TestSQLiteElectsNoLeader(t *testing.T) {
	s := openSQLite(t, filepath.Join(t.TempDir(), "cma.db"))
	if _, _, err := s.TryLead(context.Background(), "a", "http://a:8090", time.Minute); !errors.Is(err, kitstore.ErrNoElection) {
		t.Fatalf("TryLead on SQLite: %v, want ErrNoElection", err)
	}
	if err := createTask(asKourier(), s, "t1"); err != nil {
		t.Fatalf("a SQLite write is never fenced: %v", err)
	}
}

// A standby takes the lease once it expires, by the database's clock; the epoch moves, and the old leader can neither
// write nor renew.
func TestPostgresTakeoverAfterExpiry(t *testing.T) {
	dsn := pgtest.DSN(t)
	var deposed atomic.Int32
	a := openPostgresWith(t, dsn, Options{OnDeposed: func() { deposed.Add(1) }})
	b := openPostgres(t, dsn)
	ctx := asKourier()
	first := mustLead(t, a, "a", time.Second)
	if err := createTask(ctx, a, "t1"); err != nil {
		t.Fatalf("the leader's write: %v", err)
	}
	if l, ok, err := b.TryLead(ctx, "b", "http://b:8090", time.Second); err != nil || ok || !l.Live || l.Holder != "a" {
		t.Fatalf("TryLead while the lease is live = %+v, %v, %v; want a's live lease", l, ok, err)
	}
	time.Sleep(1200 * time.Millisecond)
	second := mustLead(t, b, "b", time.Minute)
	if second.Epoch <= first.Epoch {
		t.Fatalf("epoch %d after %d: a takeover must move it", second.Epoch, first.Epoch)
	}
	if err := createTask(ctx, a, "t2"); !errors.Is(err, kitstore.ErrDeposed) {
		t.Fatalf("the old leader's write: %v, want ErrDeposed", err)
	}
	if deposed.Load() != 1 {
		t.Fatalf("OnDeposed ran %d times, want 1", deposed.Load())
	}
	if err := a.Renew(ctx, time.Minute); !errors.Is(err, kitstore.ErrDeposed) {
		t.Fatalf("the old leader's renewal: %v, want ErrDeposed", err)
	}
	if err := createTask(ctx, b, "t2"); err != nil {
		t.Fatalf("the new leader's write: %v", err)
	}
	if _, err := b.Get(ctx, "t2"); err != nil {
		t.Fatal(err)
	}
}

// A store that never led refuses every write, without calling OnDeposed: a standby writes nothing.
func TestPostgresStandbyWritesNothing(t *testing.T) {
	var deposed atomic.Int32
	s := openPostgresWith(t, pgtest.DSN(t), Options{OnDeposed: func() { deposed.Add(1) }})
	if err := createTask(asKourier(), s, "t1"); !errors.Is(err, kitstore.ErrDeposed) {
		t.Fatalf("a standby's write: %v, want ErrDeposed", err)
	}
	if deposed.Load() != 0 {
		t.Fatal("OnDeposed ran for a store that never led")
	}
}
