package fakeslack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// postText posts a message as the bot over plain HTTP and returns when the
// response is in.
func postText(t *testing.T, s *Server, text string) {
	t.Helper()
	form := url.Values{"token": {"xoxb-test-token"}, "channel": {"C1"}, "text": {text}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+"/chat.postMessage", strings.NewReader(form.Encode()))
	if err != nil {
		t.Errorf("new request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("post %q: %v", text, err)
		return
	}
	_ = resp.Body.Close()
}

// TestOwnEventsAreNotSentByAnotherRequest stops one request between queueing its
// event and sending it, and checks that a second request cannot get past: if it
// could, it would send the first one's event and answer before the first had.
// The wait for the second request is one-sided: a request that is blocked never
// finishes, so the test cannot fail on a slow machine, and only fails when the
// second request is not held back.
func TestOwnEventsAreNotSentByAnotherRequest(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	ws, resp, err := websocket.DefaultDialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(s.URL(), "http")+socketPath, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = resp.Body.Close()
	defer func() { _ = ws.Close() }()
	if _, _, err := ws.ReadMessage(); err != nil { // hello
		t.Fatalf("read hello: %v", err)
	}

	queued, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool // only the first request stops; sync.Once would stop the second too
	s.afterDispatch = func() {
		if first.CompareAndSwap(false, true) {
			close(queued)
			<-release
		}
	}

	aDone, bDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(aDone); postText(t, s, "A") }()
	<-queued // A has queued its event and is stopped before sending it
	go func() { defer close(bDone); postText(t, s, "B") }()
	select {
	case <-bDone:
		t.Error("a second request finished while the first was between queueing and sending")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-aDone
	<-bDone

	// Each request sent its own event, A's first.
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for _, want := range []string{"A", "B"} {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read event for %q: %v", want, err)
		}
		var env struct {
			Payload struct{ Event struct{ Text string } }
		}
		if err := json.Unmarshal(raw, &env); err != nil || env.Payload.Event.Text != want {
			t.Fatalf("event = %s, %v; want the one for %q", raw, err, want)
		}
	}
}

// firstWrite runs check at the first WriteHeader or Write of a response, which is when the client
// could first see it.
type firstWrite struct {
	http.ResponseWriter
	once  sync.Once
	check func()
}

func (w *firstWrite) WriteHeader(code int) { w.once.Do(w.check); w.ResponseWriter.WriteHeader(code) }
func (w *firstWrite) Write(b []byte) (int, error) {
	w.once.Do(w.check)
	return w.ResponseWriter.Write(b)
}

// An own post's event envelope is recorded before its response begins, so a test that has the
// response may read OwnEventEnvelopes and wait on the ack with nothing left to race. Handled
// directly through ServeHTTP, with a writer that asserts at the first byte of the response, so a
// recording that came after it fails every time, not on a slow machine.
func TestOwnEventEnvelopesAreRecordedBeforeTheResponse(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	ws, resp, err := websocket.DefaultDialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(s.URL(), "http")+socketPath, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = resp.Body.Close()
	defer func() { _ = ws.Close() }()
	if _, _, err := ws.ReadMessage(); err != nil { // hello
		t.Fatalf("read hello: %v", err)
	}
	call := func(method string, form url.Values, wantEnvelopes int) {
		t.Helper()
		form.Set("token", "xoxb-test-token")
		form.Set("channel", "C1")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/"+method, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		checked := false
		s.ServeHTTP(&firstWrite{ResponseWriter: rec, check: func() {
			checked = true
			msgs := s.Messages("C1")
			if len(msgs) != 1 {
				t.Errorf("%s: %d messages at the first byte of the response; want 1", method, len(msgs))
				return
			}
			if got := len(s.OwnEventEnvelopes("C1", msgs[0].TS)); got != wantEnvelopes {
				t.Errorf("%s: %d envelopes recorded at the first byte of the response; want %d", method, got, wantEnvelopes)
			}
		}}, req)
		if !checked || !strings.Contains(rec.Body.String(), `"ok":true`) {
			t.Fatalf("%s: checked %t, response %s", method, checked, rec.Body.String())
		}
	}
	call("chat.postMessage", url.Values{"text": {"first"}}, 1)
	call("chat.update", url.Values{"text": {"changed"}, "ts": {s.Messages("C1")[0].TS}}, 2)
}
