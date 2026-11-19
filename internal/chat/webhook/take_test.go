package webhook_test

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// The wait for the Sink counts from when the handler is called, not from when the event is ready to hand on: a handler held past the
// wait before it reaches the Sink answers timeout, and hands the event to nothing, though the Sink would take it at once.
func TestTheAckWaitCountsFromWhenTheHandlerIsCalled(t *testing.T) {
	rec := &recorder{}
	p := newPlatform(t, config(rec, nil))
	const wait = 20 * time.Millisecond
	p.SetAckWait(wait)
	p.SetAfterLoad(func() { time.Sleep(2 * wait) }) // the time the handler takes before the take: reading, verifying, decoding
	sk := &sink{}
	running(t, p, sk)

	before := rec.snapshot()
	if w := serve(t, p, request(event, true)); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", w.Code)
	}
	checkOneMore(t, "timeout none", before, rec.snapshot())
	if got := sk.messages(); len(got) != 0 {
		t.Errorf("the sink got %d events after the wait was over, want none", len(got))
	}
}

// A Run that ends while its Sink holds an event answers the event not_running, once, and not timeout: no Run takes it now, and the
// sender sends it again, to the replica that runs the connection next.
func TestARunThatEndsWhileTheSinkHoldsTheEventIsNotRunning(t *testing.T) {
	rec := &recorder{}
	p := newPlatform(t, config(rec, nil))
	p.SetAckWait(time.Hour) // only the Run's end can end the take
	sk := &sink{block: true}
	stop := running(t, p, sk)

	h, _ := p.HTTPHandler()
	w := httptest.NewRecorder()
	answered := make(chan struct{})
	go func() { defer close(answered); h.ServeHTTP(w, request(event, true)) }()
	testutil.Eventually(t, "the sink to hold the event", func() bool { return sk.in.Load() == 1 })
	before := rec.snapshot()
	stop()
	<-answered

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", w.Code)
	}
	checkOneMore(t, "not_running none", before, rec.snapshot())
}

// The line that says a delivery was answered does not carry the Sink's error: the intake logs its own failures, and an error from
// further down may quote a value.
func TestTheAnswerLineCarriesNoSinkError(t *testing.T) {
	var buf syncBuffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := newPlatform(t, config(nil, log))
	running(t, p, &sink{fail: errors.New("store: zq-value-from-below")})

	if w := serve(t, p, request(event, true)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", w.Code)
	}
	lines := linesOf(logLines(t, buf.String()), "webhook: delivery answered")
	if len(lines) != 1 || lines[0]["outcome"] != "withheld" {
		t.Fatalf("the answer lines are %v, want one of withheld", lines)
	}
	if _, ok := lines[0]["err"]; ok {
		t.Errorf("the answer line carries err: %v", lines[0])
	}
	if strings.Contains(buf.String(), "zq-value-from-below") {
		t.Errorf("the log holds the Sink's error:\n%s", buf.String())
	}
}
