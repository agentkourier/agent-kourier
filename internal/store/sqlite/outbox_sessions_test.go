package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var (
	key = store.ConversationKey{Connection: "slack", Channel: "C1", ThreadTS: "1.1"}
	ttl = 90 * 24 * time.Hour
)

func TestOutbox(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	list := func() []store.OutboxEntry {
		t.Helper()
		l, err := s.ListUnposted(ctx)
		if err != nil {
			t.Fatalf("ListUnposted: %v", err)
		}
		return l
	}
	if l := list(); len(l) != 0 {
		t.Fatalf("empty outbox lists %d rows", len(l))
	}
	a := store.OutboxEntry{ID: "ev1/root", EventID: "ev1", Role: "root", Channel: "C1", CreatedAt: t0}
	b := store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: "reply", Channel: "C1", ThreadTS: "1.1", CreatedAt: t0.Add(time.Second)}
	base := store.OutboxEntry{ID: "x", EventID: "e", Role: store.OutboxNotice, Channel: "C", CreatedAt: t0}
	for _, mutate := range []func(*store.OutboxEntry){
		func(o *store.OutboxEntry) { o.ID = "" }, func(o *store.OutboxEntry) { o.EventID = "" },
		func(o *store.OutboxEntry) { o.Role = "bogus" }, func(o *store.OutboxEntry) { o.Channel = "" },
		func(o *store.OutboxEntry) { o.CreatedAt = time.Time{} },
	} {
		bad := base
		mutate(&bad)
		if _, err := s.CreateOutbox(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("CreateOutbox(%+v) = %v; want ErrInvalid", bad, err)
		}
	}
	for _, o := range []store.OutboxEntry{b, a} {
		if c, err := s.CreateOutbox(ctx, o); err != nil || !c {
			t.Fatalf("create %s = %v, %v", o.ID, c, err)
		}
	}
	if c, err := s.CreateOutbox(ctx, a); err != nil || c {
		t.Fatalf("duplicate create = %v, %v; want false, nil", c, err)
	}
	// The database guards the role set even when Go validation is bypassed.
	if _, err := s.write.ExecContext(ctx, `INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at)
		VALUES ('raw', 'e', 'bogus', 'C', '', 1)`); err == nil || !strings.Contains(err.Error(), "invalid outbox role") {
		t.Fatalf("insert with an invalid outbox role = %v; want the trigger's error", err)
	}
	if _, err := s.write.ExecContext(ctx, `UPDATE outbox SET role = 'bogus' WHERE id = ?`, a.ID); err == nil || !strings.Contains(err.Error(), "invalid outbox role") {
		t.Fatalf("update to an invalid outbox role = %v; want the trigger's error", err)
	}
	if l := list(); len(l) != 2 || l[0] != a || l[1] != b {
		t.Fatalf("unposted = %+v; want a then b", l)
	}

	if err := s.RecordMessageRef(ctx, a.ID, "1700.01"); err != nil {
		t.Fatalf("RecordMessageRef: %v", err)
	}
	if err := s.RecordMessageRef(ctx, a.ID, "1700.01"); err != nil {
		t.Fatalf("recording the same ref again = %v; want no-op", err)
	}
	if err := s.RecordMessageRef(ctx, a.ID, "1700.99"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("recording a different ref = %v; want ErrConflict", err)
	}
	if err := s.RecordMessageRef(ctx, "nope", "1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RecordMessageRef missing = %v; want ErrNotFound", err)
	}
	if err := s.RecordMessageRef(ctx, b.ID, ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("RecordMessageRef empty ref = %v; want ErrInvalid", err)
	}
	if l := list(); len(l) != 1 || l[0] != b {
		t.Fatalf("unposted after record = %+v; want only b", l)
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if _, err := s.GetSession(ctx, key, ttl, t0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSession missing = %v; want ErrNotFound", err)
	}
	sess := store.Session{Key: key, ContextID: "ctx1", TaskID: "task1", Binding: "ns/b", LastActivity: t0}
	noBinding, noKey := sess, sess
	noBinding.Binding = ""
	noKey.Key.ThreadTS = ""
	for _, b := range []store.Session{{Key: key, Binding: "ns/b"}, noBinding, noKey} {
		if err := s.UpsertSession(ctx, b); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("UpsertSession(%+v) = %v; want ErrInvalid", b, err)
		}
	}
	if _, err := s.GetSession(ctx, key, 0, t0); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("GetSession with zero ttl = %v; want ErrInvalid", err)
	}
	noCtx := sess
	noCtx.ContextID, noCtx.TaskID = "", ""
	if err := s.UpsertSession(ctx, noCtx); err != nil { // ContextID and TaskID are optional
		t.Fatalf("UpsertSession without ContextID: %v", err)
	}
	for _, task := range []string{"task1", "task2"} { // second call replaces the row
		sess.TaskID = task
		if err := s.UpsertSession(ctx, sess); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
	}
	if got, err := s.GetSession(ctx, key, ttl, t0); err != nil || got != sess {
		t.Fatalf("GetSession = %+v, %v; want %+v", got, err, sess)
	}

	// Exactly ttl old is live; one nanosecond older is expired, and reading never deletes.
	if _, err := s.GetSession(ctx, key, ttl, t0.Add(ttl)); err != nil {
		t.Fatalf("GetSession at the boundary = %v; want live", err)
	}
	for range 2 {
		if got, err := s.GetSession(ctx, key, ttl, t0.Add(ttl+1)); !errors.Is(err, store.ErrExpired) || got != sess {
			t.Fatalf("GetSession expired = %+v, %v; want the row and ErrExpired", got, err)
		}
	}
	// Touching extends the window.
	if err := s.TouchSession(ctx, key, t0.Add(ttl)); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	if _, err := s.GetSession(ctx, key, ttl, t0.Add(2*ttl)); err != nil {
		t.Fatalf("GetSession after touch = %v; want live", err)
	}
	if err := s.TouchSession(ctx, store.ConversationKey{Connection: "slack", Channel: "C1", ThreadTS: "9.9"}, t0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("TouchSession missing = %v; want ErrNotFound", err)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "courier.db")
	s := openAt(t, path)
	sess := store.Session{Key: key, ContextID: "ctx1", TaskID: "task1", Binding: "ns/b", LastActivity: t0}
	if _, err := s.AppendEvent(ctx, store.Event{ID: "ev1", Kind: "alert", ReceivedAt: t0}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if _, err := s.CreateOutbox(ctx, store.OutboxEntry{ID: "o1", EventID: "ev1", Role: store.OutboxRoot, Channel: "C1", CreatedAt: t0}); err != nil {
		t.Fatalf("CreateOutbox: %v", err)
	}
	if err := s.UpsertSession(ctx, sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s = openAt(t, path)
	if _, err := s.GetEvent(ctx, "ev1"); err != nil {
		t.Fatalf("event lost across reopen: %v", err)
	}
	if l, err := s.ListUnposted(ctx); err != nil || len(l) != 1 || l[0].ID != "o1" {
		t.Fatalf("outbox after reopen = %+v, %v", l, err)
	}
	if got, err := s.GetSession(ctx, key, ttl, t0); err != nil || got != sess {
		t.Fatalf("session after reopen = %+v, %v", got, err)
	}
	var n int
	if err := s.read.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil || n != 9 {
		t.Fatalf("schema_migrations rows after reopen = %d, %v; want 9", n, err)
	}
}

func TestConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	const workers, perWorker = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker*3)
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				id := fmt.Sprintf("w%d-%d", w, i)
				_, err := s.AppendEvent(ctx, store.Event{ID: id, Kind: "alert", ReceivedAt: t0})
				errs <- err
				_, err = s.CreateOutbox(ctx, store.OutboxEntry{ID: id, EventID: id, Role: store.OutboxRoot, Channel: "C1", CreatedAt: t0})
				errs <- err
				errs <- s.UpsertSession(ctx, store.Session{Key: store.ConversationKey{Connection: "c", Channel: "C", ThreadTS: id}, Binding: "ns/b", LastActivity: t0})
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write failed: %v", err)
		}
	}
	for _, table := range []string{"events", "outbox", "sessions"} {
		var n int
		if err := s.read.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != workers*perWorker {
			t.Fatalf("%s rows = %d, %v; want %d", table, n, err, workers*perWorker)
		}
	}
}

func TestInTxRollsBackOnPanic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := openTemp(t)
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = s.inTx(ctx, func(*sql.Tx) error { panic("boom") })
	}()
	if recovered == nil {
		t.Fatal("inTx swallowed the panic")
	}
	// A leaked Tx would hold the only write connection and time this out.
	if _, err := s.AppendEvent(ctx, store.Event{ID: "after-panic", Kind: "alert", ReceivedAt: t0}); err != nil {
		t.Fatalf("write after a panicking transaction: %v", err)
	}
}
