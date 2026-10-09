package e2e

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const exposition = `# HELP agentkourier_approval_outcomes_total Tool approvals decided, by outcome.
# TYPE agentkourier_approval_outcomes_total counter
agentkourier_approval_outcomes_total{outcome="denied"} 3
agentkourier_approval_outcomes_total{outcome="approved"} 1
agentkourier_outbox_abandoned_total{reason="msg_too_long",channel="C1"} 2
agentkourier_outbox_abandoned_total{reason="not_in_channel",channel="C1"} 5

# TYPE go_goroutines gauge
go_goroutines 12
agentkourier_odd_total{path="a\\b\"c\nd"} 7
agentkourier_stamped_total{a="b"} 5 1700000000000
agentkourier_big_total 1.5e+03
agentkourier_inf_total +Inf
agentkourier_approval_outcomes_total_bucket{le="1"} 99
`

func metricsServer(t *testing.T, body string) *AgentKourier {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewAgentKourier(srv.URL + "/") // a trailing slash in the injected URL is harmless
}

func TestCounterFindsASampleByNameAndLabels(t *testing.T) {
	c := metricsServer(t, exposition)
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"agentkourier_approval_outcomes_total", map[string]string{"outcome": "denied"}, 3},
		{"agentkourier_approval_outcomes_total", map[string]string{"outcome": "approved"}, 1},
		{"agentkourier_outbox_abandoned_total", map[string]string{"reason": "not_in_channel"}, 5},                // a subset of its labels
		{"agentkourier_outbox_abandoned_total", map[string]string{"reason": "msg_too_long", "channel": "C1"}, 2}, // all of them
		{"go_goroutines", nil, 12}, // no labels
		{"agentkourier_odd_total", map[string]string{"path": "a\\b\"c\nd"}, 7}, // escapes undone
		{"agentkourier_stamped_total", map[string]string{"a": "b"}, 5},         // a timestamp is ignored
		{"agentkourier_big_total", nil, 1500},                                  // an exponent
		{"agentkourier_inf_total", nil, math.Inf(1)},                           // as Prometheus writes it
	} {
		got, err := c.Counter(t.Context(), tc.name, tc.labels)
		if err != nil || got != tc.want {
			t.Errorf("Counter(%s, %v) = %v, %v; want %v", tc.name, tc.labels, got, err, tc.want)
		}
	}
}

func TestCounterSaysWhyThereIsNoSingleSample(t *testing.T) {
	c := metricsServer(t, exposition)
	for _, tc := range []struct {
		name      string
		labels    map[string]string
		wantNone  bool   // ErrNoSample
		wantInErr string // otherwise this text
	}{
		{"agentkourier_missing_total", nil, true, ""},
		{"agentkourier_approval_outcomes_total", map[string]string{"outcome": "timed_out"}, true, ""},
		{"agentkourier_approval_outcomes_total", map[string]string{"outcome": "denied", "extra": "x"}, true, ""}, // every label must be there
		{"agentkourier_approval_outcomes", map[string]string{"outcome": "denied"}, true, ""},                     // the name is whole, not a prefix
		{"agentkourier_approval_outcomes_total", nil, false, "2 samples"},                                        // which one is not said: ambiguous
		{"agentkourier_outbox_abandoned_total", map[string]string{"channel": "C1"}, false, "2 samples"},
	} {
		_, err := c.Counter(t.Context(), tc.name, tc.labels)
		switch {
		case err == nil:
			t.Errorf("Counter(%s, %v) succeeded", tc.name, tc.labels)
		case errors.Is(err, ErrNoSample) != tc.wantNone:
			t.Errorf("Counter(%s, %v) = %v; ErrNoSample is %v, want %v", tc.name, tc.labels, err, !tc.wantNone, tc.wantNone)
		case tc.wantInErr != "" && !strings.Contains(err.Error(), tc.wantInErr):
			t.Errorf("Counter(%s, %v) = %v; want %q in it", tc.name, tc.labels, err, tc.wantInErr)
		}
	}
}

func TestCounterRefusesWhatItCannotRead(t *testing.T) {
	for _, body := range []string{
		"agentkourier_x_total{outcome=\"denied\" 1\n",    // a label set that does not close
		"agentkourier_x_total{outcome=denied} 1\n",       // a value with no quotes
		"agentkourier_x_total{outcome=\"denied\"} one\n", // a value that is no number
		"agentkourier_x_total\n",                         // no value
		"agentkourier_x_total{o=\"a\\q\"} 1\n",           // an escape Prometheus does not have
		"agentkourier_x_total{a=\"1\" b=\"2\"} 1\n",      // pairs with no comma between
		"agentkourier_x_total{a=\"1\",a=\"2\"} 1\n",      // a label twice, which would silently take the last
		"agentkourier_x_total{1a=\"1\"} 1\n",             // a name that starts with a digit
		"agentkourier_x_total{a-b=\"1\"} 1\n",            // a name with a character a label name has not
		"agentkourier_x_total{,a=\"1\"} 1\n",             // a comma with nothing before it
		"agentkourier_x_total{a=\"1\",,b=\"2\"} 1\n",     // a comma with nothing between
		"agentkourier_x_total{a=\"1\"b=\"2\"} 1\n",       // a value that runs into the next name
		"agentkourier_x_total 1 2 3\n",                   // more than a value and a timestamp
	} {
		if _, err := metricsServer(t, body).Counter(t.Context(), "agentkourier_x_total", nil); err == nil || errors.Is(err, ErrNoSample) {
			t.Errorf("Counter over %q = %v; want a parse error, not a missing sample", body, err)
		}
	}
	huge := "# " + strings.Repeat("x", maxMetrics) + "\n" // over what is read
	if _, err := metricsServer(t, huge).Counter(t.Context(), "agentkourier_x_total", nil); !errors.Is(err, errTooLarge) {
		t.Errorf("Counter over a body of %d bytes = %v; want errTooLarge", len(huge), err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no metrics", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := NewAgentKourier(srv.URL).Counter(t.Context(), "x", nil); statusOf(err) != http.StatusInternalServerError {
		t.Errorf("Counter against a failing Agent Kourier = %v; want its 500", err)
	}
}

func TestWaitReadyPollsUntilReadyzAnswers200(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		if polls.Add(1) < 3 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	if err := NewAgentKourier(srv.URL).WaitReady(t.Context(), 5*time.Second); err != nil || polls.Load() != 3 {
		t.Fatalf("WaitReady = %v after %d polls; want it ready on the third", err, polls.Load())
	}
}

func TestWaitReadyTimesOutOnAnAgentKourierThatIsNeverReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "starting", http.StatusServiceUnavailable) }))
	defer srv.Close()
	err := NewAgentKourier(srv.URL).WaitReady(t.Context(), 150*time.Millisecond)
	if !errors.Is(err, ErrTimeout) || statusOf(err) != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "starting") {
		t.Fatalf("WaitReady = %v; want a timeout wrapping the 503 and what Agent Kourier said", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := NewAgentKourier(srv.URL).WaitReady(ctx, time.Minute); !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("WaitReady with a cancelled context = %v; want the context's error", err)
	}
}

// TestCounterReadsTheLabelFormsPrometheusWrites: spaces and a trailing comma are fine, as are empty
// braces and every legal name; what the strictness refuses is only what is not the format.
func TestCounterReadsTheLabelFormsPrometheusWrites(t *testing.T) {
	c := metricsServer(t, "agentkourier_a_total{} 1\n"+
		"agentkourier_b_total{a=\"1\", b=\"2\" } 2\n"+
		"agentkourier_c_total{a=\"1\",} 3\n"+
		"agentkourier_d_total{_a9=\"x\",A_b=\"y\"} 4\n")
	for name, want := range map[string]float64{"agentkourier_a_total": 1, "agentkourier_b_total": 2, "agentkourier_c_total": 3, "agentkourier_d_total": 4} {
		if got, err := c.Counter(t.Context(), name, nil); err != nil || got != want {
			t.Errorf("Counter(%s) = %v, %v; want %v", name, got, err, want)
		}
	}
	if got, err := c.Counter(t.Context(), "agentkourier_b_total", map[string]string{"b": "2"}); err != nil || got != 2 {
		t.Errorf("Counter by the second label = %v, %v", got, err)
	}
}
