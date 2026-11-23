package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
)

func mustLead(t *testing.T, db *DB, holder string, d time.Duration) Lease {
	t.Helper()
	l, ok, err := db.TryLead(context.Background(), holder, "http://"+holder+":8090", d)
	if err != nil || !ok {
		t.Fatalf("TryLead(%s) = %+v, %v, %v; want the lease", holder, l, ok, err)
	}
	return l
}

// writeNote is a write of the add-on's, through Write.
func writeNote(db *DB, id string) error {
	ctx := context.Background()
	return db.Write(ctx, func(q Querier) error {
		_, err := q.Exec(ctx, `INSERT INTO notes (id) VALUES (?)`, id)
		return err
	})
}

// fencedNote is a write of a library's on SQL, which runs Fence in its transaction as ADK's gorm callback does.
func fencedNote(db *DB, id string) error {
	ctx := context.Background()
	tx, err := db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := db.Fence(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notes (id) VALUES ($1)`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Two replicas that try at once: exactly one leads, and the other reads it as the leader.
func TestPostgresOneLeader(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPostgres(t, dsn), openPostgres(t, dsn)
	type result struct {
		lease Lease
		ok    bool
		err   error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, db := range []*DB{a, b} {
		wg.Go(func() {
			name := []string{"a", "b"}[i]
			l, ok, err := db.TryLead(context.Background(), name, "http://"+name+":8090", time.Minute)
			results[i] = result{l, ok, err}
		})
	}
	wg.Wait()
	leaders := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("TryLead %d: %v", i, r.err)
		}
		if r.ok {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("%d leaders, want 1: %+v", leaders, results)
	}
	won, lost := results[0], results[1]
	if lost.ok {
		won, lost = lost, won
	}
	if lost.lease.Holder != won.lease.Holder || lost.lease.Address != won.lease.Address || !lost.lease.Live || lost.lease.Epoch != won.lease.Epoch {
		t.Fatalf("the standby read %+v, the leader holds %+v", lost.lease, won.lease)
	}
	cur, err := a.CurrentLeader(context.Background())
	if err != nil || cur.Holder != won.lease.Holder || !cur.Live {
		t.Fatalf("CurrentLeader = %+v, %v", cur, err)
	}
}

// A standby takes the lease once it expires, by the database's clock; the epoch moves, and the old leader can neither
// write, through Write or a library's fenced transaction, nor renew.
func TestPostgresTakeoverAfterExpiry(t *testing.T) {
	dsn := pgtest.DSN(t)
	var deposed atomic.Int32
	a := openPostgresWith(t, dsn, Options{OnDeposed: func() { deposed.Add(1) }})
	b := openPostgres(t, dsn)
	ctx := context.Background()
	first := mustLead(t, a, "a", time.Second)
	if err := writeNote(a, "n1"); err != nil {
		t.Fatalf("the leader's write: %v", err)
	}
	if err := fencedNote(a, "f1"); err != nil {
		t.Fatalf("the leader's fenced write: %v", err)
	}
	if l, ok, err := b.TryLead(ctx, "b", "http://b:8090", time.Second); err != nil || ok || !l.Live || l.Holder != "a" {
		t.Fatalf("TryLead while the lease is live = %+v, %v, %v; want a's live lease", l, ok, err)
	}
	time.Sleep(1200 * time.Millisecond)
	second := mustLead(t, b, "b", time.Minute)
	if second.Epoch <= first.Epoch {
		t.Fatalf("epoch %d after %d: a takeover must move it", second.Epoch, first.Epoch)
	}
	if err := writeNote(a, "n2"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("the old leader's write: %v, want ErrDeposed", err)
	}
	if err := fencedNote(a, "f2"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("the old leader's fenced write: %v, want ErrDeposed", err)
	}
	if deposed.Load() != 2 {
		t.Fatalf("OnDeposed ran %d times, want once for each refused write", deposed.Load())
	}
	if err := a.Renew(ctx, time.Minute); !errors.Is(err, ErrDeposed) {
		t.Fatalf("the old leader's renewal: %v, want ErrDeposed", err)
	}
	if err := writeNote(b, "n2"); err != nil {
		t.Fatalf("the new leader's write: %v", err)
	}
	if err := fencedNote(b, "f2"); err != nil {
		t.Fatalf("the new leader's fenced write: %v", err)
	}
}

// Renewal keeps the lease past its first expiry, and moves no epoch.
func TestPostgresRenewKeepsTheLease(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPostgres(t, dsn), openPostgres(t, dsn)
	ctx := context.Background()
	first := mustLead(t, a, "a", time.Second)
	for range 3 {
		time.Sleep(500 * time.Millisecond)
		if err := a.Renew(ctx, time.Second); err != nil {
			t.Fatalf("Renew: %v", err)
		}
	}
	if l, ok, err := b.TryLead(ctx, "b", "http://b:8090", time.Second); err != nil || ok || l.Epoch != first.Epoch {
		t.Fatalf("TryLead of a renewed lease = %+v, %v, %v", l, ok, err)
	}
	if err := writeNote(a, "n1"); err != nil {
		t.Fatalf("the renewed leader's write: %v", err)
	}
}

// Release ends the lease at once: a standby takes it on its next try, and the releasing database writes no more.
func TestPostgresReleaseHandsOver(t *testing.T) {
	dsn := pgtest.DSN(t)
	var deposed atomic.Int32
	a := openPostgresWith(t, dsn, Options{OnDeposed: func() { deposed.Add(1) }})
	b := openPostgres(t, dsn)
	ctx := context.Background()
	mustLead(t, a, "a", time.Hour)
	if err := a.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	cur, err := b.CurrentLeader(ctx)
	if err != nil || cur.Live {
		t.Fatalf("CurrentLeader after a release = %+v, %v; want no live lease", cur, err)
	}
	mustLead(t, b, "b", time.Hour)
	if err := writeNote(a, "n1"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("a write after Release: %v, want ErrDeposed", err)
	}
	if err := a.Release(ctx); err != nil {
		t.Fatalf("a second Release: %v, want none", err)
	}
	if cur, err := b.CurrentLeader(ctx); err != nil || !cur.Live || cur.Holder != "b" {
		t.Fatalf("a second Release by a changed b's lease: %+v, %v", cur, err)
	}
	if deposed.Load() != 0 {
		t.Fatal("OnDeposed ran for a database that released its lease: it holds no epoch, so nothing deposed it")
	}
}

// A replica whose name already holds the lease (its container restarted in the same pod) takes it at once, with a new
// epoch: the process that held it is gone.
func TestPostgresSameHolderRetakes(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, again := openPostgres(t, dsn), openPostgres(t, dsn)
	first := mustLead(t, a, "pod-1", time.Hour)
	second := mustLead(t, again, "pod-1", time.Hour)
	if second.Epoch <= first.Epoch {
		t.Fatalf("epoch %d after %d", second.Epoch, first.Epoch)
	}
	if err := writeNote(a, "n1"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("the earlier process's write: %v, want ErrDeposed", err)
	}
}

// A database that never led refuses every write, Write's and a library's fenced one, without calling OnDeposed: a
// standby writes nothing.
func TestPostgresStandbyWritesNothing(t *testing.T) {
	var deposed atomic.Int32
	db := openPostgresWith(t, pgtest.DSN(t), Options{OnDeposed: func() { deposed.Add(1) }})
	if err := writeNote(db, "n1"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("a standby's write: %v, want ErrDeposed", err)
	}
	if err := fencedNote(db, "f1"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("a standby's fenced write: %v, want ErrDeposed", err)
	}
	if deposed.Load() != 0 {
		t.Fatal("OnDeposed ran for a database that never led")
	}
}

// A takeover waits for the old leader's write that already passed the epoch check, so that write commits under the
// old epoch and every later one fails: the check and the takeover are serialized, not racing.
func TestPostgresTakeoverWaitsForAWriteInFlight(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPostgres(t, dsn), openPostgres(t, dsn)
	ctx := context.Background()
	mustLead(t, a, "a", 500*time.Millisecond)
	inside, finish := make(chan struct{}), make(chan struct{})
	// The write ends before the databases close (cleanups run last first), so a failure below cannot leave it, and the
	// test, waiting.
	var finishOnce sync.Once
	end := func() { finishOnce.Do(func() { close(finish) }) }
	t.Cleanup(end)
	wrote := make(chan error, 1)
	go func() {
		wrote <- a.Write(ctx, func(q Querier) error {
			close(inside)
			<-finish
			_, err := q.Exec(ctx, `INSERT INTO notes (id) VALUES ('n1')`)
			return err
		})
	}()
	select {
	case <-inside:
	case err := <-wrote:
		t.Fatalf("the write ended before it ran: %v", err)
	}
	time.Sleep(600 * time.Millisecond) // the lease has expired; the write still holds its check
	took := make(chan error, 1)
	go func() {
		_, ok, err := b.TryLead(ctx, "b", "http://b:8090", time.Minute)
		if err == nil && !ok {
			err = errors.New("the expired lease was not taken")
		}
		took <- err
	}()
	select {
	case err := <-took:
		t.Fatalf("the takeover finished while a write that passed the check was open: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	end()
	if err := <-wrote; err != nil {
		t.Fatalf("the write in flight: %v", err)
	}
	if err := <-took; err != nil {
		t.Fatalf("the takeover: %v", err)
	}
	if n := count(t, b, `SELECT COUNT(*) FROM notes WHERE id = 'n1'`); n != 1 {
		t.Fatalf("the write in flight is lost: %d rows", n)
	}
	if err := writeNote(a, "n2"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("the old leader's next write: %v, want ErrDeposed", err)
	}
}

// errFenced stands for a deposed leader's refusal.
var errFenced = errors.New("fenced")

// Write and Fence run the fence in the write's own transaction, on Postgres too, where a library's transaction (gorm's
// on SQL) numbers the placeholders: a marker the fence reads back and writes goes with the refused write. A fence that
// ran on another connection would leave it behind.
func TestPostgresWritesRunTheFenceInTheirTransaction(t *testing.T) {
	db := openPostgres(t, pgtest.DSN(t))
	mustLead(t, db, "a", time.Hour)
	calls := 0
	SetFenceForTest(db, func(ctx context.Context, q Querier) error {
		calls++
		marker := fmt.Sprintf("marker-%d", calls)
		if _, err := q.Exec(ctx, `INSERT INTO notes (id) VALUES (?)`, marker); err != nil {
			t.Errorf("the fence's marker: %v", err)
		}
		var seen string
		if err := q.QueryRow(ctx, `SELECT id FROM notes WHERE id = ?`, marker).Scan(&seen); err != nil {
			t.Errorf("the fence's read: %v", err)
		}
		return errFenced
	})
	if err := writeNote(db, "w1"); !errors.Is(err, errFenced) {
		t.Errorf("Write: %v, want the fence's error", err)
	}
	if err := fencedNote(db, "w2"); !errors.Is(err, errFenced) {
		t.Errorf("Fence: %v, want the fence's error", err)
	}
	if calls != 2 {
		t.Errorf("the fence ran %d times, want once for each write", calls)
	}
	SetFenceForTest(db, db.checkEpoch)
	if n := count(t, db, `SELECT COUNT(*) FROM notes`); n != 0 {
		t.Fatalf("the refused writes left %d rows: the fence ran outside their transactions", n)
	}
	if err := writeNote(db, "w3"); err != nil {
		t.Fatalf("a write under the epoch check again: %v", err)
	}
}
