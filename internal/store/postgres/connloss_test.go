package postgres

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// terminateBackends kills the server side of every connection that the application name owns, as a
// failover, a restart of the server or a network device that drops idle connections does.
func terminateBackends(t *testing.T, application string) int {
	t.Helper()
	rows, err := admin(t).Query(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name = $1 AND pid <> pg_backend_pid()`, application)
	if err != nil {
		t.Fatal(err)
	}
	n, err := pgx.CollectRows(rows, pgx.RowTo[bool])
	if err != nil {
		t.Fatal(err)
	}
	return len(n)
}

// TestPoolReconnectsAfterConnectionLoss: when the server drops the pool's connections, the store goes
// on working on new ones, with no restart. The first call that meets a dead connection may fail (a
// write is not retried: whether the dead connection ran it is not known, and callers retry what is
// idempotent); the calls after it do not.
func TestPoolReconnectsAfterConnectionLoss(t *testing.T) {
	dsn, _, application := freshSchema(t)
	s := openAt(t, dsn)
	if err := s.AppendAudit(t.Context(), auditEntry("before", t0)); err != nil {
		t.Fatal(err)
	}
	if n := terminateBackends(t, application); n == 0 {
		t.Fatal("no backend of the store was found to terminate")
	}

	deadline := time.Now().Add(15 * time.Second)
	var failures int
	for i := 0; ; i++ {
		err := s.AppendAudit(t.Context(), auditEntry(fmt.Sprint("after-", i), t0))
		if err == nil {
			break
		}
		failures++
		if time.Now().After(deadline) {
			t.Fatalf("the store did not recover from the loss of its connections: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if failures > 2 { // one per connection that was idle in the pool and died; the pool here has made one or two
		t.Errorf("%d calls failed before the store recovered", failures)
	}
	// What was written before the loss is there, and what is written after goes on from it.
	l, err := s.ListAudit(t.Context(), time.Time{}, 0, 100)
	if err != nil || len(l) < 2 || l[0].Subject != "before" {
		t.Errorf("audit after the reconnect = %+v, %v; want the entry from before the loss, then the new one", l, err)
	}
}

// TestInFlightWriteFailsCleanly: a write whose connection is lost while it waits fails with an error, at
// once and not after a timeout, and leaves nothing behind; the pool then serves the next write.
func TestInFlightWriteFailsCleanly(t *testing.T) {
	dsn, _, application := freshSchema(t)
	s := openAt(t, dsn)
	release := holdLock(t, lockAudit, "") // the append will wait for the log's lock, in its transaction

	result := make(chan error, 1)
	go func() { result <- s.AppendAudit(t.Context(), auditEntry("lost", t0)) }()

	// Wait until the append is in its transaction, waiting for the lock, then cut its connection.
	waiting := time.After(10 * time.Second)
	for {
		var n int
		if err := admin(t).QueryRow(t.Context(), `SELECT COUNT(*) FROM pg_stat_activity
			WHERE application_name = $1 AND wait_event_type = 'Lock' AND wait_event = 'advisory'`, application).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		select {
		case <-waiting:
			t.Fatal("the append never waited for the lock")
		case err := <-result:
			t.Fatalf("the append finished while the lock was held: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	terminateBackends(t, application)

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("a write whose connection was cut reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a write whose connection was cut did not fail")
	}
	release()

	// Nothing of the lost write is there, and the store writes again on a new connection.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := s.AppendAudit(t.Context(), auditEntry("again", t0)); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the store did not write again: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	l, err := s.ListAudit(t.Context(), time.Time{}, 0, 10)
	if err != nil || len(l) != 1 || l[0].Subject != "again" {
		t.Errorf("audit = %+v, %v; want only the write made after the loss", l, err)
	}
}
