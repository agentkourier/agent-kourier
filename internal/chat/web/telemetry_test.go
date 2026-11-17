package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// runningObserved runs p on sk with the broker's instruments in Run's context, as the broker runs a connection, and returns the registry
// they record in. stop ends the run and waits for it.
func runningObserved(t *testing.T, p *web.Platform, sk *sink) (reg *prometheus.Registry, stop func()) {
	t.Helper()
	reg = prometheus.NewRegistry()
	obs, err := telemetry.New(reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(telemetry.WithObserver(t.Context(), obs))
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, sk) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); _ = <-done }) }
	t.Cleanup(stop)
	testutil.Eventually(t, "the platform to take requests", p.Running)
	return reg, stop
}

// requests is the series of agentkourier_web_requests_total in reg, as "outcome profile connection" and its count.
func requests(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "agentkourier_web_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			out[labels["outcome"]+" "+labels["profile"]+" "+labels["connection"]] = m.GetCounter().GetValue()
		}
	}
	return out
}

// Each answer of the handler is counted once, by its outcome, with the connection's profile and name, on Run's context: what the
// broker's operators read to tell a wrong secret (unverified) from a sender that sends what the profile does not take (malformed).
func TestEveryAnswerIsCountedByItsOutcome(t *testing.T) {
	prof := newTestProfile(false)
	prof.maxBody = 256
	p := newPlatform(t, prof, "", nil)
	p.SetAckWait(20 * time.Millisecond)
	sk := &sink{}
	reg, stop := runningObserved(t, p, sk)

	for _, tc := range []struct {
		name    string
		body    string
		signed  bool
		setup   func()
		outcome string
		code    int
	}{
		{"an event the sink takes", event, true, nil, "accepted", http.StatusAccepted},
		{"nothing to take", `{"Ignore":true}`, true, nil, "ignored", http.StatusAccepted},
		{"not verified", event, false, nil, "unverified", http.StatusUnauthorized},
		{"not the schema", `{"Channel":"ch-1","URL":"x"}`, true, nil, "malformed", http.StatusBadRequest},
		{"an event the platform cannot take", `{"ID":"m-1","User":"u-1"}`, true, nil, "malformed", http.StatusBadRequest},
		{"over the profile's bound", `{"Text":"` + strings.Repeat("a", 300) + `"}`, true, nil, "too_large", http.StatusRequestEntityTooLarge},
		{"a sink that fails", event, true, func() { sk.mu.Lock(); sk.fail = errors.New("the store is busy"); sk.mu.Unlock() }, "withheld", http.StatusServiceUnavailable},
		{"a sink that does not answer", event, true, func() { sk.mu.Lock(); sk.fail, sk.block = nil, true; sk.mu.Unlock() }, "timeout", http.StatusServiceUnavailable},
	} {
		if tc.setup != nil {
			tc.setup()
		}
		series := tc.outcome + " kourier-v1 ns/web"
		before := requests(t, reg)
		if w := serve(t, p, request(http.MethodPost, tc.body, tc.signed)); w.Code != tc.code {
			t.Errorf("%s: status %d, want %d", tc.name, w.Code, tc.code)
		}
		after := requests(t, reg)
		if got := after[series] - before[series]; got != 1 {
			t.Errorf("%s: %q counted %v more, want 1 (series %v)", tc.name, series, got, after)
		}
	}

	h, _ := p.HTTPHandler() // the broker may serve the route a moment after the run ends, or between two runs: answered 503, as not_running
	stop()
	before := requests(t, reg)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request(http.MethodPost, event, true))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("a request after the run ended: status %d, want 503", w.Code)
	}
	if got := requests(t, reg)["not_running kourier-v1 ns/web"] - before["not_running kourier-v1 ns/web"]; got != 1 {
		t.Errorf("a request after the run ended counted %v more as not_running, want 1: the last run's context still carries the instruments", got)
	}
}

// The connection is up while a Run takes requests: Config.OnConnection is told true when it starts to, and false once it stops.
func TestOnConnectionFollowsTheRun(t *testing.T) {
	var mu sync.Mutex
	var told []bool
	p, err := web.New(web.Config{Connection: "ns/web", Profile: newTestProfile(false), Logger: testutil.DiscardLogger(),
		OnConnection: func(up bool) { mu.Lock(); told = append(told, up); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	stop := running(t, p, &sink{})
	mu.Lock()
	got := slices.Clone(told)
	mu.Unlock()
	if !slices.Equal(got, []bool{true}) {
		t.Errorf("told %v while the run takes requests, want [true]", got)
	}
	stop()
	mu.Lock()
	got = slices.Clone(told)
	mu.Unlock()
	if !slices.Equal(got, []bool{true, false}) {
		t.Errorf("told %v once the run ended, want [true false]", got)
	}
}
