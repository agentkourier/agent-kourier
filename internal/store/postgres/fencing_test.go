package postgres

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// auditRows counts the rows of the audit table in the schema of dsn: the evidence that a write changed nothing, read
// on a connection of the test's own.
func auditRows(t *testing.T, dsn string) int {
	t.Helper()
	var n int
	if err := admin(t).QueryRow(t.Context(), `SELECT count(*) FROM `+schemaOf(t, dsn)+`.audit`).Scan(&n); err != nil {
		t.Fatalf("count the audit rows: %v", err)
	}
	return n
}

// leadershipRow reads the one row of leadership.
func leadershipRow(t *testing.T, dsn string) (epoch int64, holder string, since time.Time) {
	t.Helper()
	if err := admin(t).QueryRow(t.Context(), `SELECT epoch, holder, since FROM `+schemaOf(t, dsn)+`.leadership`).Scan(&epoch, &holder, &since); err != nil {
		t.Fatalf("read the leadership row: %v", err)
	}
	return epoch, holder, since
}

// leadWithRetry is what the broker does with Lead: it tries until it is given an epoch or the time is up.
func leadWithRetry(ctx context.Context, s *Store, holder string) (int64, error) {
	var last error
	for ctx.Err() == nil {
		epoch, err := s.Lead(ctx, holder)
		if err == nil {
			return epoch, nil
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	return 0, errors.Join(ctx.Err(), last)
}

// advisoryWaiters counts the backends that wait for an advisory lock: a write that has passed the epoch check and waits for
// the lock it takes next.
const advisoryWaiters = `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`

// waitForWaiter polls the server, on a connection of the test's own, until the query counts a waiter, or fails the test.
func waitForWaiter(t *testing.T, query string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	conn := admin(t) // one connection for the whole wait: a new one per poll would exhaust the server's connections
	for {
		var n int
		if err := conn.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
			t.Fatalf("count the waiters: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing waits for the lock the test expected a waiter on")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestLeadTakesTheNextEpochAndSaysWho: the table starts at epoch 0 with nobody, and each Lead adds one and records the
// holder and when.
func TestLeadTakesTheNextEpochAndSaysWho(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	a, b := openStandbyAt(t, dsn), openStandbyAt(t, dsn)
	if epoch, holder, _ := leadershipRow(t, dsn); epoch != 0 || holder != "" {
		t.Fatalf("leadership after the migration = (%d, %q); want (0, \"\")", epoch, holder)
	}

	got, err := a.Lead(t.Context(), "pod-a")
	if err != nil || got != 1 {
		t.Fatalf("first Lead = %d, %v; want epoch 1", got, err)
	}
	got, err = b.Lead(t.Context(), "pod-b")
	if err != nil || got != 2 {
		t.Fatalf("second Lead = %d, %v; want epoch 2", got, err)
	}

	epoch, holder, since := leadershipRow(t, dsn)
	if epoch != 2 || holder != "pod-b" || time.Since(since) > time.Minute {
		t.Errorf("leadership = (%d, %q, %v); want epoch 2, holder pod-b, since just now", epoch, holder, since)
	}
	if _, err := a.Lead(t.Context(), ""); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("Lead with no holder = %v; want ErrInvalid", err)
	}
}

// TestOnlyOneRowOfLeadership: the table is one row, which no statement can add to.
func TestOnlyOneRowOfLeadership(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	openStandbyAt(t, dsn)
	if _, err := admin(t).Exec(t.Context(), `INSERT INTO `+schemaOf(t, dsn)+`.leadership (epoch, holder) VALUES (9, 'x')`); err == nil {
		t.Error("a second row of leadership was accepted")
	}
}

// TestStandbyRefusesWrites: a store that has not been given an epoch writes nothing, whichever write it is, and reads
// still work.
func TestStandbyRefusesWrites(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	s := openStandbyAt(t, dsn)
	hooked := false
	s.onDeposed = func() { hooked = true }

	err := s.AppendAudit(t.Context(), auditEntry("standby", t0))

	if !errors.Is(err, store.ErrDeposed) {
		t.Errorf("AppendAudit on a standby = %v; want store.ErrDeposed", err)
	}
	if n := auditRows(t, dsn); n != 0 {
		t.Errorf("%d audit rows after a refused write; want none", n)
	}
	if hooked {
		t.Error("the deposed hook ran for a store that never led: nothing was deposed, and the hook ends the process")
	}
	if _, err := s.GetEvent(t.Context(), "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetEvent on a standby = %v; want a read to work (not found)", err)
	}
}

// TestStaleEpochIsDeposed is the fence: A leads, B takes over, and A's next write fails with ErrDeposed, changes
// nothing, is counted and is told to the hook, while B's writes go on and A's reads still work.
func TestStaleEpochIsDeposed(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	var told atomic.Int32
	a := openStandbyAt(t, dsn, WithOnDeposed(func() { told.Add(1) }))
	b := openStandbyAt(t, dsn)
	if _, err := a.Lead(t.Context(), "pod-a"); err != nil {
		t.Fatal(err)
	}
	if err := a.AppendAudit(t.Context(), auditEntry("while leading", t0)); err != nil {
		t.Fatalf("the leader's own write: %v", err)
	}
	if _, err := b.Lead(t.Context(), "pod-b"); err != nil {
		t.Fatal(err)
	}

	err := a.AppendAudit(t.Context(), auditEntry("after the takeover", t0))

	if !errors.Is(err, store.ErrDeposed) {
		t.Fatalf("a deposed leader's write = %v; want store.ErrDeposed", err)
	}
	if n := auditRows(t, dsn); n != 1 {
		t.Errorf("%d audit rows; want 1: the deposed write must change nothing", n)
	}
	if told.Load() != 1 {
		t.Errorf("the deposed hook ran %d times; want once", told.Load())
	}
	reg := prometheus.NewPedanticRegistry()
	if err := a.RegisterMetrics(reg); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(a.deposed); got != 1 {
		t.Errorf("courier_store_deposed_total = %v; want 1", got)
	}
	if err := b.AppendAudit(t.Context(), auditEntry("the new leader", t0)); err != nil {
		t.Errorf("the new leader's write: %v", err)
	}
	if _, err := a.GetEvent(t.Context(), "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a deposed store's read = %v; want reads unfenced", err)
	}
}

// TestWriteTransactionSetsLockTimeoutAfterTheCheck: inside a write transaction lock_timeout is the short one, and
// only there: the pool's connection reads its own setting again once the transaction ends.
func TestWriteTransactionSetsLockTimeoutAfterTheCheck(t *testing.T) {
	s, _ := openFresh(t)
	if err := s.inTx(t.Context(), func(tx pgx.Tx) error {
		var got string
		if err := tx.QueryRow(t.Context(), `SHOW lock_timeout`).Scan(&got); err != nil {
			return err
		}
		if got != "3s" {
			t.Errorf("lock_timeout inside a write transaction = %q; want 3s", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := show(t, s, "lock_timeout"); got != "0" {
		t.Errorf("lock_timeout outside a write transaction = %q; want the server's 0: it is SET LOCAL", got)
	}
}

// TestDeposedWriteIsRefusedAtOnceWhereItWouldWaitForALock: the check comes before any lock the write takes, so a
// deposed leader learns it at once even when the lock it needs is held, instead of waiting for the lock.
func TestDeposedWriteIsRefusedAtOnceWhereItWouldWaitForALock(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	a, b := openAt(t, dsn), openStandbyAt(t, dsn)
	if _, err := b.Lead(t.Context(), "pod-b"); err != nil {
		t.Fatal(err)
	}
	holdLock(t, lockAudit, "") // the lock AppendAudit takes

	start := time.Now()
	err := a.AppendAudit(t.Context(), auditEntry("deposed", t0))

	if !errors.Is(err, store.ErrDeposed) {
		t.Errorf("AppendAudit = %v; want store.ErrDeposed, and not a wait for the lock", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("the deposed write took %v: it waited for the lock before it checked the epoch", waited)
	}
}

// TestDeposedLeaderBlockedOnALockDoesNotStallTheTakeover: a leader whose write waits for a lock holds a share on the
// epoch row, which the next leader's update needs. The waiting write gives up at lock_timeout, so the takeover is
// held up for a few seconds and not for the statement timeout (30 s), and the write that gave up changed nothing.
func TestDeposedLeaderBlockedOnALockDoesNotStallTheTakeover(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	a, b := openAt(t, dsn), openStandbyAt(t, dsn)
	release := holdLock(t, lockAudit, "")
	blocked := make(chan error, 1)
	go func() { blocked <- a.AppendAudit(t.Context(), auditEntry("blocked", t0)) }()
	waitForWaiter(t, advisoryWaiters) // A's write has passed the check and waits for the lock, holding its share

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	epoch, err := leadWithRetry(ctx, b, "pod-b")

	if err != nil {
		t.Fatalf("the takeover did not complete: %v", err)
	}
	if took := time.Since(start); took > 6*time.Second {
		t.Errorf("the takeover took %v behind a write waiting for a lock; want a few seconds (lock_timeout 3 s)", took)
	}
	if epoch != 2 {
		t.Errorf("epoch = %d; want 2", epoch)
	}
	select {
	case err := <-blocked:
		if err == nil {
			t.Error("the write that waited for the lock committed after the takeover")
		}
	case <-time.After(5 * time.Second):
		t.Error("the write that waited for the lock is still waiting")
	}
	release()
	if err := a.AppendAudit(t.Context(), auditEntry("after", t0)); !errors.Is(err, store.ErrDeposed) {
		t.Errorf("the deposed leader's next write = %v; want ErrDeposed", err)
	}
	if n := auditRows(t, dsn); n != 0 {
		t.Errorf("%d audit rows; want none from the deposed leader", n)
	}
}

// TestNoWriteOfTheDeposedLeaderCommitsAfterTheTakeover: A writes as fast as it can, from several goroutines, while B
// takes over. B gets its epoch within a few seconds, and from the moment it has it A's rows stop: the count of rows
// does not move again, and every later call of A is refused.
func TestNoWriteOfTheDeposedLeaderCommitsAfterTheTakeover(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	a, b := openAt(t, dsn), openStandbyAt(t, dsn)
	stop := make(chan struct{})
	var writers sync.WaitGroup
	var lastErr atomic.Value
	for i := range 4 {
		writers.Go(func() {
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := a.AppendAudit(t.Context(), auditEntry("hammer", t0.Add(time.Duration(i*1000000+n)*time.Second))); err != nil {
					lastErr.Store(err)
				}
			}
		})
	}
	time.Sleep(300 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := leadWithRetry(ctx, b, "pod-b")
	took := time.Since(start)
	after := auditRows(t, dsn)
	time.Sleep(500 * time.Millisecond)
	close(stop)
	writers.Wait()

	if err != nil {
		t.Fatalf("the takeover did not complete under load: %v", err)
	}
	if took > 6*time.Second {
		t.Errorf("the takeover took %v under a leader that kept writing; want a few seconds", took)
	}
	if after == 0 {
		t.Error("the leader wrote nothing before the takeover: the test did not exercise a leader that writes")
	}
	if final := auditRows(t, dsn); final != after {
		t.Errorf("%d audit rows after the takeover, %d when the new leader had its epoch: a deposed write committed", final, after)
	}
	if err, _ := lastErr.Load().(error); !errors.Is(err, store.ErrDeposed) {
		t.Errorf("the last error of the deposed leader's writes = %v; want ErrDeposed", err)
	}
}

// TestAWriteThatPassedTheCheckCommitsBeforeTheEpochMoves is the invariant the fence rests on: the check holds a share on
// the epoch row until the transaction ends, so the next Lead waits for it. A's write passes the check and waits for a
// lock; B's Lead must then wait too (it is seen waiting for the leadership table), and only when A's write has committed
// does B get epoch 2. Without the share (a plain SELECT) Lead would not wait, and A's row would commit at epoch 1 after
// epoch 2 existed.
func TestAWriteThatPassedTheCheckCommitsBeforeTheEpochMoves(t *testing.T) {
	dsn, schema, _ := freshSchema(t)
	a, b := openAt(t, dsn), openStandbyAt(t, dsn)
	release := holdLock(t, lockAudit, "")
	written := make(chan error, 1)
	go func() { written <- a.AppendAudit(t.Context(), auditEntry("checked at epoch 1", t0)) }()
	waitForWaiter(t, advisoryWaiters)

	led := make(chan error, 1)
	go func() { _, err := b.Lead(t.Context(), "pod-b"); led <- err }()
	waitForWaiter(t, `SELECT count(*) FROM pg_locks l JOIN pg_class c ON c.oid = l.relation JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE NOT l.granted AND c.relname = 'leadership' AND n.nspname = $1`, schema)
	if epoch, _, _ := leadershipRow(t, dsn); epoch != 1 {
		t.Fatalf("the epoch is %d while a write that passed the check at epoch 1 is still open; want 1", epoch)
	}
	release()

	if err := <-written; err != nil {
		t.Fatalf("the write that had passed the check: %v", err)
	}
	if err := <-led; err != nil {
		t.Fatalf("the takeover: %v", err)
	}
	if n := auditRows(t, dsn); n != 1 {
		t.Errorf("%d audit rows; want the one that passed the check at epoch 1", n)
	}
	// The commit order is proved above, not here: B was seen waiting on the leadership table, with the epoch still 1, while
	// A's transaction was open, and B cannot be granted that lock until A's transaction ends. (A transaction ID shows when a
	// transaction began, not when it committed, so comparing them would hold whichever committed first.)
	if err := a.AppendAudit(t.Context(), auditEntry("after", t0)); !errors.Is(err, store.ErrDeposed) {
		t.Errorf("the next write of A = %v; want ErrDeposed", err)
	}
}
