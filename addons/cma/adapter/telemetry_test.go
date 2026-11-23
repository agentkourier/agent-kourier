package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/anthropics/anthropic-sdk-go/v2"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
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

// observedHandler is a handler over fake, observed by tel, with the stall timeout given.
func observedHandler(t *testing.T, fake *fakeBackend, tel *telemetry.Telemetry, stall time.Duration) (*Executor, a2asrv.RequestHandler) {
	t.Helper()
	exec := &Executor{Backend: Observe(fake, tel), Store: newStore(t, nil), Keepalive: time.Hour, ToolResultTimeout: stall, Telemetry: tel}
	var err error
	if exec.Metrics, err = NewMetrics(tel, exec.Contexts); err != nil {
		t.Fatal(err)
	}
	return exec, NewRequestHandler(exec, asKourier{})
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

// Settle counts each task it fails, one whose stored record does not decode too; Restore counts the sessions it archives as superseded and those CMA no longer lists
// as gone; the reaper counts the idle ones.
func TestTakeoverAndReaperAreCounted(t *testing.T) {
	fake := newFake(nil)
	clk := newClock()
	path := filepath.Join(t.TempDir(), "cma.db")
	st := openStore(t, path, clk.now)
	ctx := kourier(context.Background())
	for _, tk := range []*a2a.Task{
		{ID: "working", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
		{ID: "unreadable", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
	} {
		if _, err := st.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	corrupt(t, st, "unreadable")
	for _, s := range []store.Session{{ContextID: "c1", SessionID: "sesn_c1"}, {ContextID: "gone", SessionID: "sesn_gone"}} {
		s.LastActive = clk.now()
		if err := st.PutSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	fake.seed("sesn_c1", "c1", clk.now())
	fake.seed("sesn_c1_extra", "c1", clk.now().Add(-time.Hour))
	tel := newTelemetry(t, nil)
	exec := &Executor{Backend: Observe(fake, tel), Store: st, IdleTTL: time.Hour, Telemetry: tel, now: clk.now}
	var err error
	if exec.Metrics, err = NewMetrics(tel, exec.Contexts); err != nil {
		t.Fatal(err)
	}
	if err := exec.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if err := exec.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	clk.advance(2 * time.Hour)
	exec.reap(ctx)
	wantSeries(t, tel, map[string]string{
		`agentkourier_cma_tasks_interrupted_total`:                                                       "2",
		`agentkourier_cma_turns_total{outcome="interrupted"}`:                                            "2",
		`agentkourier_cma_sessions_archived_total{reason="superseded"}`:                                  "1",
		`agentkourier_cma_sessions_archived_total{reason="gone"}`:                                        "1",
		`agentkourier_cma_sessions_archived_total{reason="idle"}`:                                        "1",
		`agentkourier_cma_operation_duration_seconds_count{operation="list_sessions",outcome="success"}`: "1",
		`agentkourier_cma_operation_duration_seconds_count{operation="archive",outcome="success"}`:       "2",
	})
}

// The turn's span is a child of the caller's, and each call to the API a child of the turn.
func TestTurnSpanIsTheCallersChild(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	_, h := observedHandler(t, newFake(script(say("ok"), idle(stopEndTurn))), tel, 0)
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	caller := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true})
	ctx, cancel := context.WithTimeout(trace.ContextWithRemoteSpanContext(context.Background(), caller), 5*time.Second)
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
