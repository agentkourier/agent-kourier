package fakeslack

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestControlErrorStatusAndPartialMessage(t *testing.T) {
	stored := UserMessage{Channel: "C1", TS: "1.2", Envelopes: []string{"env-000001"}}
	tests := []struct {
		name    string
		err     error
		partial any
		want    int
		message bool
	}{
		{"no client", ErrNoConnection, nil, http.StatusConflict, false},
		{"not found", ErrNotFound, nil, http.StatusNotFound, false},
		{"invalid", invalid("no"), nil, http.StatusBadRequest, false},
		{"anything else", errors.New("boom"), nil, http.StatusInternalServerError, false},
		{"lost event after storing", ErrNoConnection, stored, http.StatusConflict, true},
		{"nothing stored", ErrNoConnection, UserMessage{}, http.StatusConflict, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeControlError(rec, tc.err, tc.partial)
			var body struct{ Message *UserMessage }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", rec.Body, err)
			}
			if rec.Code != tc.want || (body.Message != nil) != tc.message || (tc.message && body.Message.TS != "1.2") {
				t.Fatalf("status %d, body %s; want %d, message present %v", rec.Code, rec.Body, tc.want, tc.message)
			}
		})
	}
}

// TestCloseEndsAckWaits holds an ack poll open, waits until its handler is in
// WaitAck, and checks that Close ends it. It is internal so that it can see the
// waiters count: a sleep or a "request written" signal cannot show that the
// handler is blocked, and Close winning that race passes without the fix.
func TestCloseEndsAckWaits(t *testing.T) {
	s := New()
	ws, resp, err := websocket.DefaultDialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(s.URL(), "http")+socketPath, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = resp.Body.Close()
	defer func() { _ = ws.Close() }()
	if _, _, err := ws.ReadMessage(); err != nil { // hello
		t.Fatalf("read hello: %v", err)
	}
	id, err := s.SendEnvelope("slash_commands", nil) // never acked
	if err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}

	polled := make(chan struct{})
	go func() {
		defer close(polled)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL()+"/_control/acks/"+id+"?timeout=30s", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	for deadline := time.Now().Add(5 * time.Second); s.sock.waiters.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the ack poll never reached WaitAck")
		}
	}
	start := time.Now()
	s.Close()
	<-polled
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v behind a 30s ack poll", d)
	}
}
