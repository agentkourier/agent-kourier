package postgres

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"testing"
	"time"
)

func show(t *testing.T, s *Store, setting string) string {
	t.Helper()
	var v string
	if err := s.a.pool.QueryRow(t.Context(), `SHOW `+setting).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestTimeoutsAreSetOnEveryConnection: the pool's connections start with the idle-in-transaction and
// statement timeouts, a DSN that sets one has the last word, and an option changes the default.
func TestTimeoutsAreSetOnEveryConnection(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	s := openAt(t, dsn)
	if got := show(t, s, "idle_in_transaction_session_timeout"); got != "10s" {
		t.Errorf("idle_in_transaction_session_timeout = %q; want 10s", got)
	}
	if got := show(t, s, "statement_timeout"); got != "30s" {
		t.Errorf("statement_timeout = %q; want 30s", got)
	}

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("the test DSN must be a URL")
	}
	q := u.Query()
	q.Set("statement_timeout", "7000")
	u.RawQuery = q.Encode()
	s2 := openAt(t, u.String(), WithIdleInTxTimeout(3*time.Second), WithStatementTimeout(5*time.Second))
	if got := show(t, s2, "statement_timeout"); got != "7s" {
		t.Errorf("statement_timeout with the DSN setting it = %q; want the DSN's 7s", got)
	}
	if got := show(t, s2, "idle_in_transaction_session_timeout"); got != "3s" {
		t.Errorf("idle_in_transaction_session_timeout with the option = %q; want 3s", got)
	}
}

// TestIdleTransactionIsEndedAndItsLockReleased: a transaction that holds the log's lock and then goes
// silent, as a stalled or cut-off leader's does, is ended by the server, and a second writer goes on.
func TestIdleTransactionIsEndedAndItsLockReleased(t *testing.T) {
	s, _ := openFresh(t, WithIdleInTxTimeout(time.Second))
	ctx := t.Context()
	zombie, err := s.a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock(ctx, zombie, lockAudit, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zombie.Rollback(context.WithoutCancel(ctx)) })
	// The writer's own deadline, so that a server that does not end the zombie fails the test and does not hang it.
	writerCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- s.AppendAudit(writerCtx, auditEntry("after the zombie", t0)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a writer still waits for the lock of a transaction that was idle past the timeout")
	}
	if waited := time.Since(start); waited < 500*time.Millisecond {
		t.Errorf("the writer went on after %v, before the zombie could have been ended: was the lock taken?", waited)
	}
	if err := zombie.Commit(ctx); err == nil {
		t.Error("the idle transaction committed after the server should have ended it")
	}
	if l, err := s.ListAudit(ctx, time.Time{}, 0, 10); err != nil || len(l) != 1 {
		t.Errorf("audit = %+v, %v; want only the second writer's entry", l, err)
	}
}

// TestStatementTimeoutEndsAWaitForALock: a writer that waits for a lock that is never released fails
// when the statement timeout is reached, and not after TCP gives up. Migrations are not subject to it:
// an Open that waits for the migration lock longer than the timeout goes on when it is released.
func TestStatementTimeoutEndsAWaitForALock(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	s := openAt(t, dsn, WithStatementTimeout(500*time.Millisecond))
	holdLock(t, lockAudit, "")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second) // so that a missing timeout fails and does not hang
	defer cancel()
	start := time.Now()
	err := s.AppendAudit(ctx, auditEntry("stuck", t0))
	if err == nil {
		t.Fatal("an append that waited for a lock nobody releases succeeded")
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Errorf("the append failed after %v; want about the 500 ms of the timeout", waited)
	}
	if ctx.Err() != nil {
		t.Error("the append ended with its context, not with the statement timeout")
	}
}

// TestMigrationWaitsForItsLock: Open on an empty database waits while another holds the migration lock,
// longer than the statement timeout, and then applies the migrations.
func TestMigrationWaitsForItsLock(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	release := holdLock(t, lockMigrate, "")
	opened := make(chan error, 1)
	go func() {
		s, err := Open(t.Context(), dsn, WithStatementTimeout(200*time.Millisecond))
		if err == nil {
			t.Cleanup(func() { _ = s.Close() })
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		t.Fatalf("Open finished while the migration lock was held: %v", err)
	case <-time.After(800 * time.Millisecond): // past the statement timeout
	}
	release()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("Open after the lock was released: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Open did not go on when the migration lock was released")
	}
}

// TestNewerSchemaIsWarnedAbout: a database that records a migration this build does not have opens, with a
// warning that names the versions; a rollback must keep working.
func TestNewerSchemaIsWarnedAbout(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	openAt(t, dsn)
	if _, err := admin(t).Exec(t.Context(), `INSERT INTO `+schemaOf(t, dsn)+`.schema_migrations (version) VALUES (9999)`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	openAt(t, dsn, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	for _, want := range []string{"level=WARN", "newest_in_database=9999", "newest_known_migration=25"} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("log = %q; want it to hold %q", buf.String(), want)
		}
	}

	var quiet bytes.Buffer
	dsn2, _, _ := freshSchema(t)
	openAt(t, dsn2, WithLogger(slog.New(slog.NewTextHandler(&quiet, nil))))
	if quiet.Len() != 0 {
		t.Errorf("a schema that is current logged %q", quiet.String())
	}
}

// schemaOf returns the schema that a test DSN's search_path names.
func schemaOf(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("the test DSN must be a URL")
	}
	return u.Query().Get("search_path")
}
