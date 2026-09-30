// Package sink is a webhook receiver for the fast E2E tier (spec 12.1). Alertmanager routes
// every test alert to it as well as to courier, so a test can assert that alerts still arrive
// while courier is down (D22). It answers 200 to any write, keeps what it was sent in order,
// and shows that log to tests:
//
//	POST, PUT or PATCH anywhere but /requests: recorded, 200
//	GET /requests:    the recorded requests as a JSON list, oldest first
//	DELETE /requests: clears the log, 204
//
// It has no authentication and keeps no headers. It is for tests inside a cluster.
package sink

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
)

// MaxBody is the largest request body the sink records; a larger one is refused with 413.
const MaxBody = 1 << 20

// logPath is where tests read and clear the log.
const logPath = "/requests"

// Request is one recorded request.
type Request struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query,omitempty"`
	Body   string `json:"body"`
}

// Server is the sink. It is an http.Handler and is safe for concurrent use.
type Server struct {
	mu   sync.Mutex
	reqs []Request // guarded by mu
}

// NewHandler returns an empty sink.
func NewHandler() *Server { return &Server{} }

// ServeHTTP records a write, or serves the log to a test.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == logPath && r.Method == http.MethodGet:
		s.mu.Lock()
		out := append([]Request{}, s.reqs...) // never null, even when empty
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	case r.URL.Path == logPath && r.Method == http.MethodDelete:
		s.mu.Lock()
		s.reqs = nil
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == logPath:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "use GET or DELETE", http.StatusMethodNotAllowed)
	case r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch:
		s.record(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) record(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body)})
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}
