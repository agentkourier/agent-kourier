package anthropic

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/ssestream"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/agentkourier/agent-kourier/addons/ax/telemetry"
)

// scriptedClient opens each stream with err, or, without one, a stream of no events.
type scriptedClient struct{ err error }

func (c scriptedClient) NewStreaming(context.Context, sdk.MessageNewParams, ...option.RequestOption) *ssestream.Stream[sdk.MessageStreamEventUnion] {
	if c.err != nil {
		return ssestream.NewStream[sdk.MessageStreamEventUnion](nil, c.err)
	}
	res := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(""))}
	return ssestream.NewStream[sdk.MessageStreamEventUnion](ssestream.NewDecoder(res), nil)
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ net.Error = timeoutError{}

// metricsText is what tel serves at /metrics.
func metricsText(t *testing.T, tel *telemetry.Telemetry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// Each stream is timed while it opens, under messages_stream and the class of its outcome, never the error's text, and traced as a
// child of the span in its context.
func TestObserveTimesAndTracesEachStream(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tel, err := telemetry.New(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
	}{
		{"success", t.Context(), nil},
		{"cancelled", cancelled, context.Canceled},
		{"deadline", t.Context(), context.DeadlineExceeded},
		{"network timeout", t.Context(), timeoutError{}},
		{"other", t.Context(), errors.New("secret-canary in a response body")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Observe(scriptedClient{err: tc.err}, tel).NewStreaming(tc.ctx, sdk.MessageNewParams{})
			if !errors.Is(s.Err(), tc.err) {
				t.Fatalf("the stream's error = %v, want the client's %v", s.Err(), tc.err)
			}
		})
	}
	body := metricsText(t, tel)
	for _, series := range []string{
		`agentkourier_ax_operation_duration_seconds_count{operation="messages_stream",outcome="success"} 1`,
		`agentkourier_ax_operation_duration_seconds_count{operation="messages_stream",outcome="cancelled"} 1`,
		`agentkourier_ax_operation_duration_seconds_count{operation="messages_stream",outcome="timeout"} 2`,
		`agentkourier_ax_operation_duration_seconds_count{operation="messages_stream",outcome="error"} 1`,
	} {
		if !strings.Contains(body, series+"\n") {
			t.Errorf("/metrics has no line %s", series)
		}
	}
	if strings.Contains(body, "canary") {
		t.Error("an error's text reached a label")
	}
	names := map[string]int{}
	for _, s := range rec.Ended() {
		names[s.Name()]++
	}
	if names["agentkourier.ax.model.messages_stream"] != 5 {
		t.Fatalf("spans = %v", names)
	}
}

// A stream the API refused with an HTTP status is counted under that status, after the SDK's retries.
func TestObserveCountsTheAPIsStatus(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After-Ms", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	})
	tel, err := telemetry.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	m := &Model{ID: "claude-haiku-4-5", Client: Observe(NewClient(option.WithBaseURL(api.srv.URL), option.WithAPIKey(testKey)), tel)}
	if _, _, err := collect(t.Context(), m, userText("go"), true); err == nil {
		t.Fatal("the turn succeeded against a 429")
	}
	if body := metricsText(t, tel); !strings.Contains(body, `agentkourier_ax_operation_duration_seconds_count{operation="messages_stream",outcome="429"} 1`) {
		t.Fatalf("/metrics has no messages_stream 429:\n%s", body)
	}
}

// Without telemetry, Observe passes each stream through.
func TestObserveWithoutTelemetry(t *testing.T) {
	boom := errors.New("boom")
	if s := Observe(scriptedClient{err: boom}, nil).NewStreaming(t.Context(), sdk.MessageNewParams{}); !errors.Is(s.Err(), boom) {
		t.Fatalf("stream error = %v", s.Err())
	}
}
