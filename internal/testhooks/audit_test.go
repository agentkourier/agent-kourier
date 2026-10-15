//go:build testhooks

package testhooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

type auditStore struct{ entries []store.AuditEntry }

func (s auditStore) AppendAudit(context.Context, store.AuditEntry) error { return nil }

func (s auditStore) ListAudit(_ context.Context, _ time.Time, afterSeq int64, limit int) ([]store.AuditEntry, error) {
	var page []store.AuditEntry
	for _, entry := range s.entries {
		if entry.Sequence <= afterSeq {
			continue
		}
		page = append(page, entry)
		if len(page) == limit {
			break
		}
	}
	return page, nil
}

func TestAuditEndpointReturnsPersistedRows(t *testing.T) {
	want := []store.AuditEntry{
		{Sequence: 6, Kind: store.AuditEvent, Subject: "event", Outcome: "done"},
		{Sequence: 7, Kind: store.AuditInteractionDecision, User: "US19", Binding: "agent-kourier/fast-s19", Subject: "interaction=i task=t", Outcome: "answered approved=1 denied=0 approved_tools=[\"add_numbers\"]"},
	}
	e := NewAuditEndpoint()
	e.Set(auditStore{entries: want})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/__testhooks/audit", nil))

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("audit endpoint = %d %q %q; want 200 and JSON", rec.Code, rec.Body, rec.Header().Get("Content-Type"))
	}
	var got auditPageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode audit response %q: %v", rec.Body, err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatalf("decode audit response envelope %q: %v", rec.Body, err)
	}
	if _, ok := wire["paged"]; ok {
		t.Fatal("audit response retains the obsolete pagination marker")
	}
	if len(got.Entries) != len(want) || got.Entries[0] != want[0] || got.Entries[1] != want[1] {
		t.Fatalf("audit rows = %+v; want %+v", got, want)
	}
}

func TestAuditEndpointPagesEveryRowWithASequenceCursor(t *testing.T) {
	entries := make([]store.AuditEntry, auditPageSize+2)
	for i := range entries {
		entries[i] = store.AuditEntry{Sequence: int64(i + 1), Kind: store.AuditEvent, Subject: "event", Outcome: "done"}
	}
	e := NewAuditEndpoint()
	e.Set(auditStore{entries: entries})

	first := httptest.NewRecorder()
	e.ServeHTTP(first, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/__testhooks/audit", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first audit page = %d %q; want 200", first.Code, first.Body)
	}
	if got := first.Body.Len(); got >= 1<<20 {
		t.Fatalf("first audit page is %d bytes; want less than the 1 MiB e2e client limit", got)
	}
	var gotFirst auditPageResponse
	if err := json.Unmarshal(first.Body.Bytes(), &gotFirst); err != nil {
		t.Fatalf("decode first audit page %q: %v", first.Body, err)
	}
	if len(gotFirst.Entries) != auditPageSize || gotFirst.Entries[0].Sequence != 1 || gotFirst.Entries[len(gotFirst.Entries)-1].Sequence != int64(auditPageSize) {
		t.Fatalf("first audit page = sequences %d..%d (%d rows); want 1..%d (%d rows)", gotFirst.Entries[0].Sequence, gotFirst.Entries[len(gotFirst.Entries)-1].Sequence, len(gotFirst.Entries), auditPageSize, auditPageSize)
	}

	second := httptest.NewRecorder()
	path := "/__testhooks/audit?after_seq=" + strconv.FormatInt(gotFirst.Entries[len(gotFirst.Entries)-1].Sequence, 10)
	e.ServeHTTP(second, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	var gotSecond auditPageResponse
	if err := json.Unmarshal(second.Body.Bytes(), &gotSecond); err != nil {
		t.Fatalf("decode second audit page %q: %v", second.Body, err)
	}
	if len(gotSecond.Entries) != 2 || gotSecond.Entries[0].Sequence != int64(auditPageSize+1) || gotSecond.Entries[1].Sequence != int64(auditPageSize+2) {
		t.Fatalf("second audit page = %+v; want the two rows after sequence %d", gotSecond, auditPageSize)
	}
}

func TestAuditEndpointKeepsWorstCasePagesBelowTheClientLimit(t *testing.T) {
	outcome := strings.Repeat("\"", store.MaxDecisionOutcome)
	entries := make([]store.AuditEntry, auditPageSize)
	for i := range entries {
		entries[i] = store.AuditEntry{
			Sequence: int64(i + 1),
			At:       time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
			Kind:     store.AuditInteractionDecision,
			User:     strings.Repeat("u", 256),
			Email:    strings.Repeat("e", 256),
			Identity: strings.Repeat("i", 256),
			Binding:  strings.Repeat("b", 256),
			Subject:  strings.Repeat("s", 256),
			Outcome:  outcome,
		}
	}
	e := NewAuditEndpoint()
	e.Set(auditStore{entries: entries})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/__testhooks/audit", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("worst-case audit page = %d %q; want 200", rec.Code, rec.Body)
	}
	if got := rec.Body.Len(); got >= 1<<20 {
		t.Fatalf("worst-case audit page is %d bytes; want less than the 1 MiB e2e client limit", got)
	}
}

func TestAuditEndpointRejectsMalformedSequenceCursor(t *testing.T) {
	e := NewAuditEndpoint()
	e.Set(auditStore{})
	for _, cursor := range []string{"nope", "-1", "1.5", "1&after_seq=2"} {
		t.Run(cursor, func(t *testing.T) {
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/__testhooks/audit?after_seq="+cursor, nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("cursor %q = %d %q; want 400", cursor, rec.Code, rec.Body)
			}
		})
	}
}

func TestAuditEndpointFailsUntilTheStoreIsSet(t *testing.T) {
	rec := httptest.NewRecorder()
	NewAuditEndpoint().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/__testhooks/audit", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unconnected audit endpoint = %d %q; want 503", rec.Code, rec.Body)
	}
}
