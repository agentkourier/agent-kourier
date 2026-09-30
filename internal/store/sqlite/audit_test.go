package sqlite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func newAudit(subject string, at time.Time) store.AuditEntry {
	return store.AuditEntry{
		At: at, Kind: store.AuditInteractionDecision, User: "U1", Email: "u1@example.com", Identity: "sa/investigator",
		Binding: "ns/b", Subject: subject, Outcome: "approved",
	}
}

func TestAuditAppendAndList(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	// Appended out of time order: listing follows append order.
	for i, e := range []store.AuditEntry{newAudit("a", t0.Add(2*time.Hour)), newAudit("b", t0), newAudit("c", t0.Add(time.Hour))} {
		e.Sequence = 99 // ignored
		if err := s.AppendAudit(ctx, e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	subjects := func(since time.Time, after int64, limit int) (out []string) {
		t.Helper()
		l, err := s.ListAudit(ctx, since, after, limit)
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		for _, e := range l {
			out = append(out, e.Subject)
		}
		return out
	}
	all, err := s.ListAudit(ctx, time.Time{}, 0, 10)
	want := newAudit("a", t0.Add(2*time.Hour))
	want.Sequence = 1
	if err != nil || len(all) != 3 || !reflect.DeepEqual(all[0], want) || all[1].Sequence != 2 || all[2].Sequence != 3 {
		t.Fatalf("list = %+v, %v; want a, b, c with sequences 1 to 3", all, err)
	}
	for _, c := range []struct {
		name  string
		since time.Time
		after int64
		limit int
		want  []string
	}{
		{"since filters by time", t0.Add(time.Hour), 0, 10, []string{"a", "c"}},
		{"page 1", time.Time{}, 0, 2, []string{"a", "b"}},
		{"page 2", time.Time{}, 2, 2, []string{"c"}},
		{"past the end", time.Time{}, 3, 2, nil},
	} {
		if got := subjects(c.since, c.after, c.limit); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %v; want %v", c.name, got, c.want)
		}
	}
	for _, limit := range []int{0, 1001} {
		if _, err := s.ListAudit(ctx, time.Time{}, 0, limit); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("limit %d = %v; want ErrInvalid", limit, err)
		}
	}
}

func TestAuditAppendValidation(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	for name, mutate := range map[string]func(*store.AuditEntry){
		"unknown kind":  func(e *store.AuditEntry) { e.Kind = "bogus" },
		"zero time":     func(e *store.AuditEntry) { e.At = time.Time{} },
		"empty subject": func(e *store.AuditEntry) { e.Subject = "" },
		"empty outcome": func(e *store.AuditEntry) { e.Outcome = "" },
		"long outcome":  func(e *store.AuditEntry) { e.Outcome = strings.Repeat("x", store.MaxAuditOutcome+1) },
	} {
		bad := newAudit("s", t0)
		mutate(&bad)
		if err := s.AppendAudit(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: append = %v; want ErrInvalid", name, err)
		}
	}
	ok := newAudit("s", t0)
	ok.User, ok.Email, ok.Identity, ok.Binding = "", "", "", "" // actor and binding are optional
	ok.Outcome = strings.Repeat("x", store.MaxAuditOutcome)
	if err := s.AppendAudit(ctx, ok); err != nil {
		t.Errorf("append at the outcome limit: %v", err)
	}
}

func TestAuditIsAppendOnlyInTheDatabase(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if err := s.AppendAudit(ctx, newAudit("a", t0)); err != nil {
		t.Fatalf("append: %v", err)
	}
	for _, q := range []string{`UPDATE audit SET outcome = 'edited'`, `DELETE FROM audit`} {
		if _, err := s.write.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q = %v; want the append-only error", q, err)
		}
	}
	if l, err := s.ListAudit(ctx, time.Time{}, 0, 10); err != nil || len(l) != 1 || l[0].Outcome != "approved" {
		t.Fatalf("audit after tampering attempts = %+v, %v", l, err)
	}
}

func TestAuditRedactsSecretsBeforeStoring(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	e := newAudit("call with Bearer FAKE-TOKEN", t0)
	e.Outcome = "denied xoxb-FAKE-TOKEN"
	if err := s.AppendAudit(ctx, e); err != nil {
		t.Fatalf("append: %v", err)
	}
	l, err := s.ListAudit(ctx, time.Time{}, 0, 10)
	if err != nil || len(l) != 1 || l[0].Subject != "call with [redacted]" || l[0].Outcome != "denied [redacted]" {
		t.Fatalf("stored = %+v, %v; want secrets redacted", l, err)
	}
}
