package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"google.golang.org/adk/v2/session"

	"github.com/agentkourier/agent-kourier/addons/ax/store/pgtest"
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

func mustLead(t *testing.T, s *Store, holder string, d time.Duration) Lease {
	t.Helper()
	l, ok, err := s.TryLead(context.Background(), holder, "http://"+holder+":8090", d)
	if err != nil || !ok {
		t.Fatalf("TryLead(%s) = %+v, %v, %v; want the lease", holder, l, ok, err)
	}
	return l
}

func createTask(s *Store, id string) error {
	_, err := s.Create(asCaller("agent-kourier"), &a2a.Task{ID: a2a.TaskID(id), ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	return err
}

func createSession(s *Store, id string) error {
	_, err := s.Sessions().Create(asCaller("agent-kourier"), &session.CreateRequest{AppName: AppName, UserID: "agent-kourier", SessionID: id})
	return err
}

func TestSQLiteElectsNoLeader(t *testing.T) {
	s := openSQLite(t, filepath.Join(t.TempDir(), "ax.db"))
	ctx := context.Background()
	if _, _, err := s.TryLead(ctx, "a", "http://a:8090", time.Minute); !errors.Is(err, ErrNoElection) {
		t.Fatalf("TryLead on SQLite: %v, want ErrNoElection", err)
	}
	for name, err := range map[string]error{"Renew": s.Renew(ctx, time.Minute), "Release": s.Release(ctx)} {
		if !errors.Is(err, ErrNoElection) {
			t.Errorf("%s on SQLite: %v, want ErrNoElection", name, err)
		}
	}
	if _, err := s.CurrentLeader(ctx); !errors.Is(err, ErrNoElection) {
		t.Errorf("CurrentLeader on SQLite: %v, want ErrNoElection", err)
	}
	if err := createTask(s, "t1"); err != nil {
		t.Fatalf("a SQLite write is never fenced: %v", err)
	}
	if err := createSession(s, "c1"); err != nil {
		t.Fatalf("an ADK write on SQLite is never fenced: %v", err)
	}
}

// Two replicas that try at once: exactly one leads, and the other reads it as the leader.
func TestPostgresOneLeader(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPostgres(t, dsn), openPostgresWith(t, dsn, Options{})
	type result struct {
		lease Lease
		ok    bool
		err   error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := []string{"a", "b"}[i]
			l, ok, err := s.TryLead(context.Background(), name, "http://"+name+":8090", time.Minute)
			results[i] = result{l, ok, err}
		}()
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
// write, through the store or through ADK, nor renew.
func TestPostgresTakeoverAfterExpiry(t *testing.T) {
	dsn := pgtest.DSN(t)
	var deposed atomic.Int32
	a := openPostgresWith(t, dsn, Options{OnDeposed: func() { deposed.Add(1) }})
	b := openPostgres(t, dsn)
	ctx := context.Background()
	first := mustLead(t, a, "a", time.Second)
	if err := createTask(a, "t1"); err != nil {
		t.Fatalf("the leader's write: %v", err)
	}
	if err := createSession(a, "c1"); err != nil {
		t.Fatalf("the leader's ADK write: %v", err)
	}
	if l, ok, err := b.TryLead(ctx, "b", "http://b:8090", time.Second); err != nil || ok || !l.Live || l.Holder != "a" {
		t.Fatalf("TryLead while the lease is live = %+v, %v, %v; want a's live lease", l, ok, err)
	}
	time.Sleep(1200 * time.Millisecond)
	second := mustLead(t, b, "b", time.Minute)
	if second.Epoch <= first.Epoch {
		t.Fatalf("epoch %d after %d: a takeover must move it", second.Epoch, first.Epoch)
	}
	if err := createTask(a, "t2"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("the old leader's write: %v, want ErrDeposed", err)
	}
	if err := createSession(a, "c2"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("the old leader's ADK write: %v, want ErrDeposed", err)
	}
	if deposed.Load() != 2 {
		t.Fatalf("OnDeposed ran %d times, want once for each refused write", deposed.Load())
	}
	if err := a.Renew(ctx, time.Minute); !errors.Is(err, ErrDeposed) {
		t.Fatalf("the old leader's renewal: %v, want ErrDeposed", err)
	}
	if err := createTask(b, "t2"); err != nil {
		t.Fatalf("the new leader's write: %v", err)
	}
	if err := createSession(b, "c2"); err != nil {
		t.Fatalf("the new leader's ADK write: %v", err)
	}
}

// Renewal keeps the lease past its first expiry, and moves no epoch.
func TestPostgresRenewKeepsTheLease(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPostgres(t, dsn), openPostgresWith(t, dsn, Options{})
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
	if err := createTask(a, "t1"); err != nil {
		t.Fatalf("the renewed leader's write: %v", err)
	}
}

// Release ends the lease at once: a standby takes it on its next try, and the releasing store writes no more.
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
	if err := createTask(a, "t1"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("a write after Release: %v, want ErrDeposed", err)
	}
	if err := a.Release(ctx); err != nil {
		t.Fatalf("a second Release: %v, want none", err)
	}
	if cur, err := b.CurrentLeader(ctx); err != nil || !cur.Live || cur.Holder != "b" {
		t.Fatalf("a second Release by a changed b's lease: %+v, %v", cur, err)
	}
	if deposed.Load() != 0 {
		t.Fatal("OnDeposed ran for a store that released its lease: it holds no epoch, so nothing deposed it")
	}
}

// A replica whose name already holds the lease (its container restarted in the same pod) takes it at once, with a new
// epoch: the process that held it is gone.
func TestPostgresSameHolderRetakes(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, again := openPostgres(t, dsn), openPostgresWith(t, dsn, Options{})
	first := mustLead(t, a, "pod-1", time.Hour)
	second := mustLead(t, again, "pod-1", time.Hour)
	if second.Epoch <= first.Epoch {
		t.Fatalf("epoch %d after %d", second.Epoch, first.Epoch)
	}
	if err := createTask(a, "t1"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("the earlier process's write: %v, want ErrDeposed", err)
	}
}

// A store that never led refuses every write, the store's and ADK's, without calling OnDeposed: a standby writes
// nothing, so it can neither settle the leader's running turns nor delete its contexts.
func TestPostgresStandbyWritesNothing(t *testing.T) {
	var deposed atomic.Int32
	s := openPostgresWith(t, pgtest.DSN(t), Options{OnDeposed: func() { deposed.Add(1) }})
	if err := createTask(s, "t1"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("a standby's write: %v, want ErrDeposed", err)
	}
	if _, err := s.SettleInterrupted(context.Background()); !errors.Is(err, ErrDeposed) {
		t.Fatalf("a standby's settlement: %v, want ErrDeposed", err)
	}
	if err := createSession(s, "c1"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("a standby's ADK write: %v, want ErrDeposed", err)
	}
	if deposed.Load() != 0 {
		t.Fatal("OnDeposed ran for a store that never led")
	}
}

// A takeover waits for the old leader's write that already passed the epoch check, so that write commits under the
// old epoch and every later one fails: the check and the takeover are serialized, not racing.
func TestPostgresTakeoverWaitsForAWriteInFlight(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPostgres(t, dsn), openPostgresWith(t, dsn, Options{})
	ctx := context.Background()
	mustLead(t, a, "a", 500*time.Millisecond)
	inside, finish := make(chan struct{}), make(chan struct{})
	// The write ends before the stores close (cleanups run last first), so a failure below cannot leave it, and the
	// test, waiting.
	var finishOnce sync.Once
	end := func() { finishOnce.Do(func() { close(finish) }) }
	t.Cleanup(end)
	wrote := make(chan error, 1)
	go func() {
		wrote <- a.write(ctx, func(q querier) error {
			close(inside)
			<-finish
			_, err := q.exec(ctx, `INSERT INTO contexts (owner, context_id, last_active) VALUES ('agent-kourier', 'c1', 0)`)
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
	var n int
	if err := b.conn.reader().queryRow(ctx, `SELECT COUNT(*) FROM contexts WHERE context_id = 'c1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the write in flight is lost: %d, %v", n, err)
	}
	if err := a.TouchContext(asCaller("agent-kourier"), "c2"); !errors.Is(err, ErrDeposed) {
		t.Fatalf("the old leader's next write: %v, want ErrDeposed", err)
	}
}
