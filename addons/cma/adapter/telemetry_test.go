package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/anthropics/anthropic-sdk-go/v2"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

func newTelemetry(t *testing.T, provider trace.TracerProvider) *telemetry.Telemetry {
	t.Helper()
	tel, err := telemetry.New(TelemetrySpec(), provider)
	if err != nil {
		t.Fatal(err)
	}
	return tel
}

// series returns the value of one series, as /metrics prints it, or "" when it is not served.
func series(t *testing.T, tel *telemetry.Telemetry, name string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, name+" "); ok {
			return v
		}
	}
	return ""
}

// wantSeries waits for each series to have its value: a turn is counted when its Execute returns, which can be after
// the caller has read the turn's last event.
func wantSeries(t *testing.T, tel *telemetry.Telemetry, want map[string]string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for name, v := range want {
		got := series(t, tel, name)
		for got != v && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			got = series(t, tel, name)
		}
		if got != v {
			t.Errorf("%s = %q, want %q", name, got, v)
		}
	}
}

// observedHandler is a client of an adapter over fake, observed by tel, with the stall timeout given.
func observedHandler(t *testing.T, fake *fakeBackend, tel *telemetry.Telemetry, stall time.Duration) (*Executor, *a2aclient.Client) {
	t.Helper()
	st := newStore(t, nil)
	exec := &Executor{Backend: Observe(fake, tel), Store: st, Keepalive: time.Hour, ToolResultTimeout: stall}
	var err error
	if exec.Metrics, err = NewMetrics(tel, exec.Contexts); err != nil {
		t.Fatal(err)
	}
	return exec, serve(t, exec, st, tel)
}

// Each way a turn ends is counted under its outcome, and the calls it made under theirs.
func TestTurnOutcomesAreCounted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		respond func(string, Outbound) []Event
		want    telemetry.TurnOutcome
	}{
		{"completed", script(say("ok"), idle(stopEndTurn)), telemetry.TurnCompleted},
		{"input required", approvalScript, telemetry.TurnInputRequired},
		{"failed", script(idle("retries_exhausted")), telemetry.TurnFailed},
		{"timeout", script(toolUse("sevt_read", "read", nil, permissionAllow)), turnTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := newTelemetry(t, nil)
			_, h := observedHandler(t, newFake(tc.respond), tel, 100*time.Millisecond)
			turn(t, h, userText("", "go"))
			wantSeries(t, tel, map[string]string{
				`agentkourier_cma_turns_total{outcome="` + string(tc.want) + `"}`:                                 "1",
				`agentkourier_cma_operation_duration_seconds_count{operation="create_session",outcome="success"}`: "1",
				`agentkourier_cma_operation_duration_seconds_count{operation="stream",outcome="success"}`:         "1",
				`agentkourier_cma_sessions`: "1",
			})
			if got := series(t, tel, `agentkourier_cma_operation_duration_seconds_count{operation="send",outcome="success"}`); got == "" {
				t.Error("the send is not timed")
			}
		})
	}
}

// A new message the adapter cannot send to a session (a file, or no text) is refused as invalid before any turn: CMA is
// never called, and no turn is counted or traced, so the failed turns count the agent's failures alone.
func TestARefusedMessageIsNoTurn(t *testing.T) {
	for name, parts := range map[string][]*a2a.Part{
		"a file":  {a2a.NewFileURLPart("https://example.test/report.pdf", "application/pdf")},
		"no text": {a2a.NewTextPart("  ")},
	} {
		t.Run(name, func(t *testing.T) {
			rec := tracetest.NewSpanRecorder()
			tel := newTelemetry(t, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
			fake := newFake(script(say("ok"), idle(stopEndTurn)))
			_, h := observedHandler(t, fake, tel, 0)
			_, err := h.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, parts...)})
			if !errors.Is(err, a2a.ErrInvalidParams) {
				t.Fatalf("SendMessage = %v, want invalid params", err)
			}
			turn(t, h, userText("", "a good one")) // its count shows the refused one's would have been in by now
			wantSeries(t, tel, map[string]string{`agentkourier_cma_turns_total{outcome="completed"}`: "1"})
			if got := series(t, tel, `agentkourier_cma_turns_total{outcome="failed"}`); got != "" {
				t.Errorf("failed turns = %s, want none", got)
			}
			if n := fake.sessionCount(); n != 1 {
				t.Errorf("sessions created = %d, want the good message's alone", n)
			}
			spans := 0
			for _, s := range rec.Ended() {
				if s.Name() == "agentkourier.cma.turn" {
					spans++
				}
			}
			if spans != 1 {
				t.Errorf("turn spans = %d, want the good message's alone", spans)
			}
		})
	}
}

// A turn that waited too long for a sandbox tool's result fails its span, as a failed turn does: timeout is one of the
// spec's ErrorTurns.
func TestTimedOutTurnFailsItsSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	_, h := observedHandler(t, newFake(script(toolUse("sevt_read", "read", nil, permissionAllow))), tel, 100*time.Millisecond)
	turn(t, h, userText("", "go"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, s := range rec.Ended() {
			if s.Name() == "agentkourier.cma.turn" {
				if s.Status().Code != codes.Error || s.Status().Description != string(turnTimeout) {
					t.Fatalf("the timed-out turn's span status = %+v, want Error %q", s.Status(), turnTimeout)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn's span did not end within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A cancel ends the running turn as canceled.
func TestCanceledTurnIsCounted(t *testing.T) {
	tel := newTelemetry(t, nil)
	_, h := observedHandler(t, newFake(func(_ string, out Outbound) []Event {
		switch {
		case out.Message != nil:
			return []Event{{Type: evUserMessage, ProcessedAt: processed()}, running(), say("working on it")}
		case out.Interrupt:
			return []Event{idle(stopEndTurn)}
		}
		return nil
	}), tel, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id a2a.TaskID
	for ev, err := range h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: userText("", "long job")}) {
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok {
			id = ev.TaskInfo().TaskID
			break
		}
	}
	if _, err := h.CancelTask(ctx, &a2a.CancelTaskRequest{ID: id}); err != nil {
		t.Fatal(err)
	}
	wantSeries(t, tel, map[string]string{`agentkourier_cma_turns_total{outcome="canceled"}`: "1"})
	if got := series(t, tel, `agentkourier_cma_turns_total{outcome="completed"}`); got != "" {
		t.Fatalf("the canceled turn also counted as completed: %s", got)
	}
}

// A failed call to the API is timed under the HTTP status it came back with.
func TestFailedCallIsTimedByItsClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "timeout"},
		{errors.New("boom"), "error"},
		{nil, "success"},
		{&anthropic.Error{StatusCode: 429}, "429"},
		{fmt.Errorf("send: %w", &anthropic.Error{StatusCode: 503}), "503"},
	} {
		if got := telemetry.Outcome(tc.err, statusOf); got != tc.want {
			t.Errorf("Outcome(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	tel := newTelemetry(t, nil)
	fake := newFake(nil)
	fake.sendErr = errors.New("refused")
	_, h := observedHandler(t, fake, tel, 0)
	for _, err := range h.SendStreamingMessage(context.Background(), &a2a.SendMessageRequest{Message: userText("", "go")}) {
		_ = err
	}
	wantSeries(t, tel, map[string]string{
		`agentkourier_cma_operation_duration_seconds_count{operation="send",outcome="error"}`: "1",
		`agentkourier_cma_turns_total{outcome="failed"}`:                                      "1",
	})
}

// A resent message that joins its task is counted as a replay, not as a turn.
func TestReplayIsCounted(t *testing.T) {
	tel := newTelemetry(t, nil)
	_, h := observedHandler(t, newFake(script(say("once"), idle(stopEndTurn))), tel, 0)
	msg := userText("", "hi")
	first, _ := turn(t, h, msg)
	again := *msg
	again.ContextID = first.ContextID
	turn(t, h, &again)
	wantSeries(t, tel, map[string]string{
		`agentkourier_cma_message_replays_total`:            "1",
		`agentkourier_cma_turns_total{outcome="completed"}`: "1",
	})
}

// The turn's span is a child of the caller's, whose trace context the request carries, and each call to the API a
// child of the turn.
func TestTurnSpanIsTheCallersChild(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	st := newStore(t, nil)
	exec := &Executor{Backend: Observe(newFake(script(say("ok"), idle(stopEndTurn))), tel), Store: st}
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	h := dial(t, serveURL(t, exec, st, tel), traceparent{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var task a2a.TaskID
	for ev, err := range h.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: userText("", "go")}) {
		if err != nil {
			t.Fatal(err)
		}
		task = ev.TaskInfo().TaskID
	}
	var turnSpan sdktrace.ReadOnlySpan
	deadline := time.Now().Add(5 * time.Second)
	for turnSpan == nil {
		for _, s := range rec.Ended() {
			if s.Name() == "agentkourier.cma.turn" {
				turnSpan = s
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no turn span ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if turnSpan.Parent().SpanID() != spanID || turnSpan.SpanContext().TraceID() != traceID {
		t.Fatalf("turn parent %v, want the caller's span", turnSpan.Parent())
	}
	attrs := map[string]string{}
	for _, a := range turnSpan.Attributes() {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs[telemetry.AttrTaskID] != string(task) || attrs[telemetry.AttrContextID] == "" || attrs[telemetry.AttrOutcome] != "completed" {
		t.Fatalf("turn attributes %v", attrs)
	}
	calls := 0
	for _, s := range rec.Ended() {
		if strings.HasPrefix(s.Name(), "agentkourier.cma.backend.") {
			calls++
			if s.Parent().SpanID() != turnSpan.SpanContext().SpanID() {
				t.Errorf("%s is not the turn's child", s.Name())
			}
		}
	}
	if calls < 3 {
		t.Fatalf("%d call spans, want create_session, stream and send", calls)
	}
}

// traceparent sets a W3C trace context on each request, as a traced caller does.
type traceparent struct{ value string }

// RoundTrip implements http.RoundTripper.
func (p traceparent) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("traceparent", p.value)
	return http.DefaultTransport.RoundTrip(r)
}
