//go:build testhooks

package testhooks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

var errAuditUnavailable = errors.New("testhooks: audit store is not ready")

// auditPageSize keeps the JSON response well below the e2e client's 1 MiB
// limit. An answered approval can store a 6144-byte outcome, and JSON may
// double every quote or backslash; sixteen rows leaves room for the other
// audit fields and their encoding while still making a large log cheap to
// walk.
const auditPageSize = 16

type auditPageResponse struct {
	Entries []store.AuditEntry `json:"entries"`
}

// AuditEndpoint is the test image's read-only view of persisted audit rows. The store is set
// after the health server starts, so a request made during startup cannot race a store assignment.
// The endpoint implementation and its route exist only in the testhooks build.
type AuditEndpoint struct {
	mu    sync.RWMutex
	store store.Audit
}

// NewAuditEndpoint returns an endpoint whose store can be connected after Run opens it.
func NewAuditEndpoint() *AuditEndpoint { return &AuditEndpoint{} }

// Set connects the endpoint to the opened store. A nil store leaves it unavailable.
func (e *AuditEndpoint) Set(s store.Audit) {
	e.mu.Lock()
	e.store = s
	e.mu.Unlock()
}

func (e *AuditEndpoint) list(ctx context.Context, afterSeq int64) ([]store.AuditEntry, error) {
	e.mu.RLock()
	s := e.store
	e.mu.RUnlock()
	if s == nil {
		return nil, errAuditUnavailable
	}
	return s.ListAudit(ctx, time.Time{}, afterSeq, auditPageSize)
}

func auditCursor(r *http.Request) (int64, error) {
	values, ok := r.URL.Query()["after_seq"]
	if !ok {
		return 0, nil
	}
	if len(values) != 1 || values[0] == "" {
		return 0, errors.New("after_seq must be one non-negative integer")
	}
	seq, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || seq < 0 {
		return 0, errors.New("after_seq must be one non-negative integer")
	}
	return seq, nil
}

// ServeHTTP returns one bounded page of persisted audit rows to the in-cluster
// E2E runner. Pass after_seq to continue after the last row from the previous
// page. The route is installed only by a testhooks build and is never part of
// the production health server.
func (e *AuditEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	afterSeq, err := auditCursor(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	entries, err := e.list(r.Context(), afterSeq)
	if err != nil {
		http.Error(w, "audit store is not ready", http.StatusServiceUnavailable)
		return
	}
	if len(entries) > auditPageSize {
		entries = entries[:auditPageSize]
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(auditPageResponse{Entries: entries}); err != nil {
		return
	}
}
