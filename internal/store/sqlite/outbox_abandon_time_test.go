package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestAbandonRefusesATimeThatReadsAsNotAbandoned: a stored 0 means "not abandoned", so a time at or
// before the epoch would clear the payload and leave the row as unposted work with nothing to post.
func TestAbandonRefusesATimeThatReadsAsNotAbandoned(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	addOutbox(t, s, "a", t0.Add(-8*24*time.Hour), `{"text":"hi"}`)
	for _, bad := range []time.Time{{}, time.Unix(0, 0), time.Unix(-5, 0)} {
		if _, err := s.AbandonOutbox(ctx, "a", bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("AbandonOutbox at %v = %v; want ErrInvalid", bad, err)
		}
		if _, err := s.AbandonOutboxBefore(ctx, t0, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("AbandonOutboxBefore as of %v = %v; want ErrInvalid", bad, err)
		}
		if _, err := s.AbandonOutboxBefore(ctx, bad, t0); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("AbandonOutboxBefore before %v = %v; want ErrInvalid", bad, err)
		}
	}
	if got, _ := s.GetOutbox(ctx, "a"); got.Payload == "" || !got.AbandonedAt.IsZero() {
		t.Fatalf("row after the refusals = %+v; want it untouched", got)
	}
}

// TestRecordMessageRefWorksOnAnAbandonedRow, by design: a late lookup that finds the message must
// be able to adopt it.
func TestRecordMessageRefWorksOnAnAbandonedRow(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	addOutbox(t, s, "a", t0, `{"text":"hi"}`)
	if _, err := s.AbandonOutbox(ctx, "a", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMessageRef(ctx, "a", "5.5"); err != nil {
		t.Fatalf("RecordMessageRef on an abandoned row = %v; want it to work", err)
	}
	if got, _ := s.GetOutbox(ctx, "a"); got.MessageRef != "5.5" || got.AbandonedAt.IsZero() {
		t.Fatalf("row = %+v; want the ref recorded and the abandon kept", got)
	}
}
