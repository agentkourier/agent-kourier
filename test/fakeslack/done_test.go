package fakeslack_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// waitDone reports whether the fake will send every envelope no more within d.
func waitDone(t *testing.T, fake *fakeslack.Server, d time.Duration, ids ...string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	err := fake.WaitDone(ctx, ids...)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitDone: %v", err)
	}
	return err == nil
}

// A request that is to be retried is not done while a retry is to come, and is done once it is answered 200 or the retries are
// over: what a test waits for before it counts what the app did with every delivery of it.
func TestARequestIsDoneOnceItIsAnswered200OrItsRetriesAreOver(t *testing.T) {
	rc := newReceiver(t, func(n int, _ received, w http.ResponseWriter) {
		if n == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	fake := httpFake(t, rc.URL, fakeslack.WithHTTPRetries(300*time.Millisecond))
	m, err := fake.Say("C1", "U1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	rc.wait(t, 1)
	if waitDone(t, fake, 100*time.Millisecond, m.Envelopes...) {
		t.Fatal("a request that failed is done before its retry")
	}
	if !waitDone(t, fake, 5*time.Second, m.Envelopes...) {
		t.Fatal("a request answered 200 on its retry is not done")
	}
	if n := rc.count(); n != 2 {
		t.Errorf("the receiver got %d requests; want the first and its retry", n)
	}

	gone := newReceiver(t, func(_ int, _ received, w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) })
	fake = httpFake(t, gone.URL, fakeslack.WithHTTPRetries(5*time.Millisecond, 5*time.Millisecond))
	m, err = fake.Say("C1", "U1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !waitDone(t, fake, 5*time.Second, m.Envelopes...) || gone.count() != 3 {
		t.Errorf("a request whose retries ran out is not done after %d attempts; want done after 3", gone.count())
	}
}

// The fake never sends a Socket Mode envelope again, so one is done as it is sent, acked or not; an ID it never issued is
// ErrNotFound. The control surface answers the same.
func TestASocketModeEnvelopeIsDoneAsItIsSent(t *testing.T) {
	fake, _ := connected(t)
	id, err := fake.SendEnvelope("slash_commands", slashCommand)
	if err != nil {
		t.Fatal(err)
	}
	if !waitDone(t, fake, time.Second, id) {
		t.Error("a Socket Mode envelope that is not acked is not done")
	}
	if err := fake.WaitDone(t.Context(), "env-nope"); !errors.Is(err, fakeslack.ErrNotFound) {
		t.Errorf("WaitDone of an ID never issued = %v; want ErrNotFound", err)
	}
	var done struct{ Done bool }
	if s := control(t, fake, "GET", "/_control/done/"+id+"?timeout=1s", nil, &done); s != 200 || !done.Done {
		t.Errorf("GET /_control/done/%s = %d, %+v; want done", id, s, done)
	}
	for path, want := range map[string]int{"/_control/done/env-nope": 404, "/_control/done/" + id + "?timeout=1h": 400} {
		if s := control(t, fake, "GET", path, nil, nil); s != want {
			t.Errorf("GET %s = %d; want %d", path, s, want)
		}
	}
}
