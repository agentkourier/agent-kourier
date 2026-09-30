package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

var (
	t0    = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	who   = Actor{User: "U1", Email: "u1@example.com", Identity: "sa/investigator", Binding: "ns/b"}
	fakes = []string{"xoxb-canary-0000-fake", "Bearer canary.fake.token", "eyJhbGciOiJub25lIn0.eyJjYW5hcnkiOjF9.sig"}
)

func newWriter(t *testing.T) (*Writer, *sqlite.Store) {
	t.Helper()
	s, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s, Options{Now: func() time.Time { return t0 }}), s
}

func rows(t *testing.T, s *sqlite.Store) []store.AuditEntry {
	t.Helper()
	got, err := s.ListAudit(context.Background(), time.Time{}, 0, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	return got
}

func only(t *testing.T, s *sqlite.Store) store.AuditEntry {
	t.Helper()
	got := rows(t, s)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got), got)
	}
	return got[0]
}

func TestEvent(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		src     Source
		outcome EventOutcome
		want    string
	}{
		{SourceAlert, EventAccepted, "alert: accepted"},
		{SourceAlert, EventRateLimited, "alert: rate_limited"},
		{SourceSlack, EventDenied, "slack_message: denied"},
	} {
		w, s := newWriter(t)
		if err := w.Event(ctx, who, c.src, "evt-1", c.outcome); err != nil {
			t.Fatalf("Event: %v", err)
		}
		want := store.AuditEntry{Sequence: 1, At: t0, Kind: store.AuditEvent, User: "U1", Email: "u1@example.com",
			Identity: "sa/investigator", Binding: "ns/b", Subject: "evt-1", Outcome: c.want}
		if got := only(t, s); got != want {
			t.Errorf("%s: row = %+v, want %+v", c.want, got, want)
		}
	}
	w, s := newWriter(t)
	noBinding := Actor{User: "U1"}
	if err := w.Event(ctx, noBinding, SourceAlert, "evt-1", EventAccepted); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("Event without a Binding = %v, want ErrInvalid", err)
	}
	if err := w.Event(ctx, who, SourceAlert, "evt-1", "bogus"); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("Event with an unknown outcome = %v, want ErrInvalid", err)
	}
	if got := rows(t, s); len(got) != 0 {
		t.Errorf("invalid events left rows: %+v", got)
	}
}

func TestUnusablePause(t *testing.T) {
	w, s := newWriter(t)
	ctx := context.Background()
	cause := fmt.Errorf("parse: %w: no request id", dialect.ErrUnusablePause)
	if err := w.UnusablePause(ctx, who, "t1", "c1", cause); err != nil {
		t.Fatalf("UnusablePause: %v", err)
	}
	got := only(t, s)
	if got.Kind != store.AuditAgentCall || got.Subject != "task=t1 context=c1" || got.User != "U1" || got.Binding != "ns/b" ||
		got.Outcome != "unusable_pause: parse: unusable pause: no request id" {
		t.Errorf("row = %+v, want the pause with its reason", got)
	}
	if err := w.UnusablePause(ctx, who, "", "", cause); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("UnusablePause naming no task or context = %v, want ErrInvalid", err)
	}
}

func TestNoSecretsInStoredRows(t *testing.T) {
	w, s := newWriter(t)
	ctx := context.Background()
	canary := strings.Join(fakes, " ")
	leaky := Actor{User: canary, Email: canary, Identity: canary, Binding: canary}
	_ = w.Event(ctx, leaky, SourceSlack, canary, EventAccepted)
	_ = w.UnusablePause(ctx, leaky, a2a.TaskID(canary), canary, fmt.Errorf("%w: %s", dialect.ErrUnusablePause, canary))
	got := rows(t, s)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	for _, e := range got {
		all := fmt.Sprintf("%+v", e)
		for _, secret := range append(fakes, "canary.fake.token") {
			if strings.Contains(all, secret) {
				t.Errorf("%s row holds %q: %+v", e.Kind, secret, e)
			}
		}
	}
}

func TestAgentTextIsCapped(t *testing.T) {
	w, s := newWriter(t)
	long := strings.Repeat("é", 500)
	cause := fmt.Errorf("%w: line one\nline\x00 two %s", dialect.ErrUnusablePause, long)
	if err := w.UnusablePause(context.Background(), who, a2a.TaskID(long), "c1", cause); err != nil {
		t.Fatalf("UnusablePause: %v", err)
	}
	got := only(t, s)
	if len(got.Outcome) > store.MaxAuditOutcome || len(got.Subject) > 2*maxField || !strings.HasSuffix(got.Outcome, "...") ||
		strings.ContainsAny(got.Outcome, "\n\x00") {
		t.Errorf("outcome (%d bytes) = %q, want it capped, single-line and marked truncated", len(got.Outcome), got.Outcome)
	}
}

type failing struct{ store.Audit }

func (failing) AppendAudit(context.Context, store.AuditEntry) error {
	return errors.New("disk full " + fakes[0])
}

func TestWriteFailureIsReturnedLoggedAndMetered(t *testing.T) {
	var logs bytes.Buffer
	var metered []store.AuditKind
	w := New(failing{}, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil)), OnFailure: func(k store.AuditKind) { metered = append(metered, k) }})
	err := w.Event(context.Background(), who, SourceAlert, "evt-1", EventAccepted)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Event = %v, want the store's error wrapped", err)
	}
	if len(metered) != 1 || metered[0] != store.AuditEvent {
		t.Errorf("metered = %v, want one event failure", metered)
	}
	if out := logs.String(); !strings.Contains(out, "audit write failed") || strings.Contains(out, fakes[0]) || strings.Contains(out, "evt-1") {
		t.Errorf("log = %q, want a failure line with neither the entry nor a secret", out)
	}
}

func TestWriteSurvivesCancelledContext(t *testing.T) {
	w, s := newWriter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a request cancelled mid-flight still needs its record
	if err := w.Event(ctx, who, SourceAlert, "evt-1", EventAccepted); err != nil {
		t.Fatalf("Event on a cancelled context: %v", err)
	}
	if got := only(t, s); got.Outcome != "alert: accepted" {
		t.Errorf("row = %+v", got)
	}
}
