package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func TestOutboxPayload(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "courier.db")
	s := openAt(t, path)
	if _, err := s.GetOutbox(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetOutbox missing = %v; want ErrNotFound", err)
	}
	o := store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", ThreadTS: "1.1", CreatedAt: t0, Payload: `{"text":"hi"}`}
	if c, err := s.CreateOutbox(ctx, o); err != nil || !c {
		t.Fatalf("CreateOutbox = %v, %v", c, err)
	}
	dup := o
	dup.Payload = `{"text":"changed"}`
	if c, err := s.CreateOutbox(ctx, dup); err != nil || c {
		t.Fatalf("duplicate CreateOutbox = %v, %v; want false, nil", c, err)
	}
	// A row written before the payload column existed gets the empty payload.
	if _, err := s.write.ExecContext(ctx, `INSERT INTO outbox (id, event_id, role, channel, thread_ts, created_at)
		VALUES ('old', 'e', 'root', 'C1', '', 1)`); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if got, err := s.GetOutbox(ctx, "old"); err != nil || got.Payload != "" {
		t.Fatalf("GetOutbox(old) = %+v, %v; want an empty payload", got, err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s = openAt(t, path)
	if got, err := s.GetOutbox(ctx, o.ID); err != nil || got != o {
		t.Fatalf("GetOutbox after reopen = %+v, %v; want %+v", got, err, o)
	}
	if l, err := s.ListUnposted(ctx); err != nil || len(l) != 2 || l[1] != o { // "old" is older
		t.Fatalf("ListUnposted = %+v, %v; want the payload kept", l, err)
	}

	// Recording the ref clears the payload: the text lives in Slack, not in the database too.
	for range 2 { // the second record is the same-ref no-op, and still leaves it empty
		if err := s.RecordMessageRef(ctx, o.ID, "1700.01"); err != nil {
			t.Fatalf("RecordMessageRef: %v", err)
		}
		want := o
		want.Payload, want.MessageRef = "", "1700.01"
		if got, err := s.GetOutbox(ctx, o.ID); err != nil || got != want {
			t.Fatalf("GetOutbox after record = %+v, %v; want %+v", got, err, want)
		}
	}
	if err := s.RecordMessageRef(ctx, o.ID, "1700.99"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a different ref = %v; want ErrConflict", err)
	}
}
