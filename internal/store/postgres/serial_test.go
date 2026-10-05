package postgres

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var t0 = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func reply(id string, k store.ConversationKey) store.Reply {
	return store.Reply{ID: id, Key: k, UserID: "U1", MessageRef: "1700.01", Text: "hello", EnqueuedAt: t0}
}

func sessionKey(thread string) store.ConversationKey {
	return store.ConversationKey{Connection: "slack", Channel: "C1", Thread: thread}
}

// holdLock takes a transaction-level advisory lock on a connection of the test's own and returns the
// function that releases it. It is how a test stands in for a writer that is in the middle of its
// transaction.
func holdLock(t *testing.T, class int32, key string) (release func()) {
	t.Helper()
	conn := admin(t)
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock(t.Context(), tx, class, key); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }) }
	t.Cleanup(release)
	return release
}

// completes reports whether done is closed within d.
func completes(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestReplyEnqueuesOfASessionWaitForEachOther: the enqueue of a session's reply takes the session's
// advisory lock before it inserts, so it cannot take a Sequence while another enqueue of the session is
// between taking its own and committing. Another session's enqueue does not wait.
func TestReplyEnqueuesOfASessionWaitForEachOther(t *testing.T) {
	s, _ := openFresh(t)
	busy, other := sessionKey("1.1"), sessionKey("2.2")
	release := holdLock(t, lockReplyQueue, sessionLockKey(busy))

	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		if _, err := s.EnqueueReply(t.Context(), reply("waits", busy)); err != nil {
			t.Errorf("EnqueueReply of the busy session: %v", err)
		}
	}()
	free := make(chan struct{})
	go func() {
		defer close(free)
		if _, err := s.EnqueueReply(t.Context(), reply("free", other)); err != nil {
			t.Errorf("EnqueueReply of another session: %v", err)
		}
	}()
	if !completes(free, 10*time.Second) {
		t.Fatal("an enqueue to another session waited for the busy session's lock")
	}
	if completes(blocked, 500*time.Millisecond) {
		t.Fatal("an enqueue to the session finished while another enqueue held its lock")
	}
	release()
	if !completes(blocked, 10*time.Second) {
		t.Fatal("the waiting enqueue did not go on when the lock was released")
	}
	if has, err := s.HasReply(t.Context(), "waits"); err != nil || !has {
		t.Errorf("HasReply(waits) = %v, %v; want the reply queued", has, err)
	}
}

// TestAuditAppendsWaitForEachOther is the same for the audit log, which has one lock for all of it.
func TestAuditAppendsWaitForEachOther(t *testing.T) {
	s, _ := openFresh(t)
	release := holdLock(t, lockAudit, "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.AppendAudit(t.Context(), auditEntry("waits", t0)); err != nil {
			t.Errorf("AppendAudit: %v", err)
		}
	}()
	if completes(done, 500*time.Millisecond) {
		t.Fatal("an append finished while another held the log's lock")
	}
	release()
	if !completes(done, 10*time.Second) {
		t.Fatal("the waiting append did not go on when the lock was released")
	}
}

// TestCursorReadersNeverMissAReply: many goroutines enqueue into a few sessions while a reader of each
// session walks its queue with NextReplyAfter, as session turns do. Every reader must see every reply
// of its session, in increasing Sequence. A Sequence taken at the insert and shown at the commit lets a
// reader see 6 before 5 commit and skip 5; the per-session lock is what prevents it.
func TestCursorReadersNeverMissAReply(t *testing.T) {
	const sessions, writersPerSession, perWriter = 3, 8, 40
	s, _ := openFresh(t, WithMaxConns(32))
	var finished atomic.Bool
	seen := make([]map[string]int64, sessions)
	var readers sync.WaitGroup
	for sn := range sessions {
		seen[sn] = map[string]int64{}
		readers.Go(func() {
			k := sessionKey(fmt.Sprint("s", sn))
			var cursor int64
			for {
				done := finished.Load() // read before the query: every enqueue is committed if it is true
				r, err := s.NextReplyAfter(t.Context(), k, cursor)
				switch {
				case err == nil:
					if r.Sequence <= cursor {
						t.Errorf("session %d: NextReplyAfter(%d) returned sequence %d", sn, cursor, r.Sequence)
						return
					}
					seen[sn][r.ID] = r.Sequence
					cursor = r.Sequence
				case !isNotFound(err):
					t.Errorf("session %d: NextReplyAfter: %v", sn, err)
					return
				case done:
					return
				default:
					runtime.Gosched()
				}
			}
		})
	}
	var writers sync.WaitGroup
	for sn := range sessions {
		for w := range writersPerSession {
			writers.Go(func() {
				k := sessionKey(fmt.Sprint("s", sn))
				for i := range perWriter {
					if _, err := s.EnqueueReply(t.Context(), reply(fmt.Sprintf("s%d-w%d-%02d", sn, w, i), k)); err != nil {
						t.Errorf("enqueue: %v", err)
						return
					}
				}
			})
		}
	}
	writers.Wait()
	finished.Store(true)
	readers.Wait()
	for sn := range sessions {
		if want := writersPerSession * perWriter; len(seen[sn]) != want {
			t.Errorf("the reader of session %d saw %d of %d replies: one that committed after a later one was skipped", sn, len(seen[sn]), want)
		}
	}
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// TestAuditPagersNeverMissAnEntry: the same for ListAudit, which pages by sequence over the whole log.
func TestAuditPagersNeverMissAnEntry(t *testing.T) {
	const writers, perWriter, page = 16, 40, 25
	s, _ := openFresh(t, WithMaxConns(32))
	var finished atomic.Bool
	seen := map[string]bool{}
	pagerDone := make(chan struct{})
	go func() {
		defer close(pagerDone)
		var cursor int64
		for {
			done := finished.Load()
			l, err := s.ListAudit(t.Context(), time.Time{}, cursor, page)
			if err != nil {
				t.Errorf("ListAudit: %v", err)
				return
			}
			for _, e := range l {
				if e.Sequence <= cursor {
					t.Errorf("ListAudit after %d returned sequence %d", cursor, e.Sequence)
					return
				}
				seen[e.Subject] = true
				cursor = e.Sequence
			}
			if len(l) == 0 {
				if done {
					return
				}
				runtime.Gosched()
			}
		}
	}()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				if err := s.AppendAudit(t.Context(), auditEntry(fmt.Sprintf("w%d-%02d", w, i), t0)); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	finished.Store(true)
	<-pagerDone
	if want := writers * perWriter; len(seen) != want {
		t.Errorf("the pager saw %d of %d entries", len(seen), want)
	}
}

// TestMigrationLockIsNotTheStoresLock: the migration lock and the store's locks are different locks, so
// a replica that migrates does not stop another's enqueues.
func TestMigrationLockIsNotTheStoresLock(t *testing.T) {
	s, _ := openFresh(t)
	release := holdLock(t, lockMigrate, "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.EnqueueReply(t.Context(), reply("r", sessionKey("1.1"))); err != nil {
			t.Errorf("EnqueueReply: %v", err)
		}
	}()
	if !completes(done, 10*time.Second) {
		t.Error("an enqueue waited for the migration lock")
	}
	release()
}

func isInvalid(err error) bool { return errors.Is(err, store.ErrInvalid) }
