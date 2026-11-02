package broker

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// refusingBackend is a backend that answers every request with code, and counts the requests.
func refusingBackend(t *testing.T, code int) (url string, requests *atomic.Int32) {
	t.Helper()
	requests = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "refused", code)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, requests
}

// A Binding whose agent serves its card passes, and the probe sent the Binding's own token, read afresh.
func TestCheckAgentPassesAgainstAServingBackend(t *testing.T) {
	f := newFixture(t)
	if err := f.b.CheckAgent(t.Context(), "sre/oncall"); err != nil {
		t.Fatalf("CheckAgent = %v, want nil", err)
	}
	if got, want := f.lastAuth(), "Bearer "+jwt("a@example.test"); got != want {
		t.Errorf("Authorization = %q, want the Binding's token", got)
	}
	f.reader.set("sre/creds/token", jwt("b@example.test")) // rotated: the next probe sends the new one
	if err := f.b.CheckAgent(t.Context(), "sre/oncall"); err != nil {
		t.Fatalf("CheckAgent after a rotation = %v, want nil", err)
	}
	if got, want := f.lastAuth(), "Bearer "+jwt("b@example.test"); got != want {
		t.Errorf("Authorization after the rotation = %q, want the new token", got)
	}
}

// A backend that refuses the token is the probe's finding, not a rejected call: the error is the
// card fetch's own, and nothing is audited or counted, however often it is probed.
func TestCheckAgentOnARejectedTokenWritesNoAuditRowAndNoMetric(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			f := newFixture(t)
			url, requests := refusingBackend(t, code)
			f.snaps.p.Store(snapshot(t, url, v1alpha1.DialectKagentV1, time.Hour))
			for range 3 {
				err := f.b.CheckAgent(t.Context(), "sre/oncall")
				var hs *dialect.HTTPStatusError
				if !errors.As(err, &hs) || hs.Code != code {
					t.Fatalf("CheckAgent = %v, want an HTTPStatusError %d", err, code)
				}
				if errors.Is(err, agent.ErrCredentialsRejected) {
					t.Errorf("CheckAgent = %v: labelled a rejected credential", err)
				}
			}
			if requests.Load() != 3 {
				t.Errorf("%d requests, want one card fetch per probe", requests.Load())
			}
			f.audit.mu.Lock()
			rows := len(f.audit.calls)
			f.audit.mu.Unlock()
			if rows != 0 || len(f.metrics) != 0 {
				t.Errorf("%d audit rows and %d rejected-credential counts, want none", rows, len(f.metrics))
			}
		})
	}
}

func TestCheckAgentOnAnUnknownBindingFails(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"sre/nope", "nope/oncall", "oncall", ""} {
		if err := f.b.CheckAgent(t.Context(), name); !errors.Is(err, session.ErrUnknownBinding) {
			t.Errorf("CheckAgent(%q) = %v, want an error wrapping ErrUnknownBinding", name, err)
		}
	}
	f.snaps.p.Store(nil)
	if err := f.b.CheckAgent(t.Context(), "sre/oncall"); err == nil || errors.Is(err, session.ErrUnknownBinding) {
		t.Errorf("CheckAgent before the first load = %v, want an error that is not ErrUnknownBinding", err)
	}
}

// A token that cannot be read fails with the sentinel, and nothing reaches the backend.
func TestCheckAgentWithAnUnreadableTokenSendsNothing(t *testing.T) {
	f := newFixture(t)
	url, requests := refusingBackend(t, http.StatusOK)
	f.snaps.p.Store(snapshot(t, url, v1alpha1.DialectKagentV1, time.Hour))
	f.reader.set("sre/creds/token", "")
	if err := f.b.CheckAgent(t.Context(), "sre/oncall"); !errors.Is(err, agent.ErrTokenUnreadable) {
		t.Errorf("CheckAgent = %v, want ErrTokenUnreadable", err)
	}
	if requests.Load() != 0 {
		t.Errorf("%d requests, want none", requests.Load())
	}
}

// The probe works on both dialects' pools.
func TestCheckAgentUsesTheBindingsDialect(t *testing.T) {
	f := newFixture(t)
	url, requests := refusingBackend(t, http.StatusNotFound)
	f.snaps.p.Store(snapshot(t, url, v1alpha1.DialectA2A, time.Hour))
	var hs *dialect.HTTPStatusError
	if err := f.b.CheckAgent(t.Context(), "sre/oncall"); !errors.As(err, &hs) || hs.Code != http.StatusNotFound || requests.Load() != 1 {
		t.Errorf("CheckAgent = %v after %d requests, want an HTTPStatusError 404 after one", err, requests.Load())
	}
}

// A leader whose lease is past its renew deadline is fenced for the calls that change the agent. The
// probe changes nothing, so it still reaches the backend.
func TestCheckAgentIsNotFenced(t *testing.T) {
	agentStub, url := newSendCounter(t)
	f := newFixture(t)
	f.snaps.p.Store(snapshot(t, url, v1alpha1.DialectKagentV1, time.Hour))
	clock := newStepClock()
	lease := newLeaseClock(clock.now)
	lease.renewed(clock.now())
	var stops []string
	b, err := NewBindings(f.snaps, f.reader, f.audit, Options{OnRejected: func(string) {}, fence: &leaseFence{clock: lease, limit: time.Second, stop: func(r string) { stops = append(stops, r) }}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	clock.advance(time.Minute) // the leader was frozen
	if err := b.CheckAgent(t.Context(), "sre/oncall"); err != nil {
		t.Errorf("CheckAgent past the renew deadline = %v, want nil", err)
	}
	if agentStub.other.Load() == 0 || len(stops) != 0 {
		t.Errorf("%d requests reached the agent, fail-stops %v; want the probe to reach it and stop nothing", agentStub.other.Load(), stops)
	}
}
