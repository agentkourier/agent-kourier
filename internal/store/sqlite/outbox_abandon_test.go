package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func addOutbox(t *testing.T, s *Store, id string, created time.Time, payload string) {
	t.Helper()
	o := store.OutboxEntry{ID: id, EventID: id, Role: store.OutboxReply, Channel: "C1", CreatedAt: created, Payload: payload}
	if _, err := s.CreateOutbox(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonOutbox(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	addOutbox(t, s, "a", t0, `{"text":"hi"}`)
	addOutbox(t, s, "posted", t0, `{"text":"hi"}`)
	if err := s.RecordMessageRef(ctx, "posted", "5.5"); err != nil {
		t.Fatal(err)
	}
	at := t0.Add(time.Hour)
	if ok, err := s.AbandonOutbox(ctx, "a", at); err != nil || !ok {
		t.Fatalf("AbandonOutbox = %v, %v; want true", ok, err)
	}
	if got, _ := s.GetOutbox(ctx, "a"); !got.AbandonedAt.Equal(at) || got.Payload != "" || got.MessageRef != "" {
		t.Fatalf("abandoned row = %+v; want the time kept, the payload cleared, no ref", got)
	}
	if l, _ := s.ListUnposted(ctx); len(l) != 0 {
		t.Fatalf("ListUnposted = %+v; an abandoned row is not unposted work", l)
	}
	if ok, err := s.AbandonOutbox(ctx, "a", at.Add(time.Hour)); err != nil || ok { // the second is not news, and keeps the first time
		t.Fatalf("second AbandonOutbox = %v, %v; want false, nil", ok, err)
	}
	if got, _ := s.GetOutbox(ctx, "a"); !got.AbandonedAt.Equal(at) {
		t.Fatalf("abandoned at %v after a second call; want %v", got.AbandonedAt, at)
	}
	if _, err := s.AbandonOutbox(ctx, "nope", at); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing row = %v; want ErrNotFound", err)
	}
	if _, err := s.AbandonOutbox(ctx, "posted", at); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a posted row = %v; want ErrConflict", err)
	}
}

// TestAbandonOutboxBefore: the retention purge takes only a row that is old, unrecorded, not yet
// abandoned and has a payload.
func TestAbandonOutboxBefore(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	cut := t0.Add(-7 * 24 * time.Hour)
	addOutbox(t, s, "old", cut.Add(-time.Second), `{"text":"hi"}`)
	addOutbox(t, s, "boundary", cut, `{"text":"hi"}`)              // not older than the cut
	addOutbox(t, s, "recent", t0.Add(-time.Hour), `{"text":"hi"}`) // young
	addOutbox(t, s, "stream", cut.Add(-time.Hour), "")             // no payload to purge
	addOutbox(t, s, "done", cut.Add(-time.Hour), `{"text":"hi"}`)  // recorded, though a payload is forced back in below
	addOutbox(t, s, "gone", cut.Add(-time.Hour), `{"text":"hi"}`)
	if err := s.RecordMessageRef(ctx, "done", "5.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.write.ExecContext(ctx, `UPDATE outbox SET payload = 'x' WHERE id = 'done'`); err != nil {
		t.Fatal(err)
	}
	early := t0.Add(-time.Hour)
	if _, err := s.AbandonOutbox(ctx, "gone", early); err != nil {
		t.Fatal(err)
	}
	ids, err := s.AbandonOutboxBefore(ctx, cut, t0)
	if err != nil || !slices.Equal(ids, []string{"old"}) {
		t.Fatalf("AbandonOutboxBefore = %v, %v; want only old", ids, err)
	}
	for id, want := range map[string]time.Time{"old": t0, "gone": early, "boundary": {}, "recent": {}, "stream": {}, "done": {}} {
		if got, _ := s.GetOutbox(ctx, id); !got.AbandonedAt.Equal(want) {
			t.Errorf("%s abandoned at %v; want %v", id, got.AbandonedAt, want)
		}
	}
	if got, _ := s.GetOutbox(ctx, "old"); got.Payload != "" {
		t.Errorf("the purged row kept its payload %q", got.Payload)
	}
	if ids, err := s.AbandonOutboxBefore(ctx, cut, t0); err != nil || len(ids) != 0 {
		t.Fatalf("second purge = %v, %v; want nothing", ids, err)
	}
}

// TestMigration0008KeepsExistingRows opens a database migrated to 0007: the rows are kept, as not
// abandoned, and the unposted one is still work.
func TestMigration0008KeepsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "courier.db")
	databaseAt(t, path, 7)
	s := openAt(t, path) // applies 0008
	ctx := context.Background()
	got, err := s.GetOutbox(ctx, "unposted")
	if err != nil || !got.AbandonedAt.IsZero() {
		t.Fatalf("row after 0008 = %+v, %v; want it kept, not abandoned", got, err)
	}
	if l, err := s.ListUnposted(ctx); err != nil || len(l) != 1 || l[0].ID != "unposted" {
		t.Fatalf("ListUnposted = %+v, %v; want the unposted row", l, err)
	}
	if got, err := s.GetOutbox(ctx, "posted"); err != nil || got.MessageRef != "5.5" {
		t.Fatalf("posted row after 0008 = %+v, %v", got, err)
	}
}
