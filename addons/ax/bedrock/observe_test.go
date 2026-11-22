package bedrock

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go/auth/bearer"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/agentkourier/agent-kourier/addons/ax/telemetry"
)

// scriptedClient answers each call with err.
type scriptedClient struct{ err error }

func (c scriptedClient) Converse(context.Context, *bedrockruntime.ConverseInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	if c.err != nil {
		return nil, c.err
	}
	return &bedrockruntime.ConverseOutput{}, nil
}

func (c scriptedClient) ConverseStream(context.Context, *bedrockruntime.ConverseStreamInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	if c.err != nil {
		return nil, c.err
	}
	return &bedrockruntime.ConverseStreamOutput{}, nil
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

// Each call is timed under its operation and the class of its outcome, never the error's text, and traced as a child of
// the span in its context.
func TestObserveTimesAndTracesEachCall(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tel, err := telemetry.New(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		err     error
		stream  bool
		outcome string
	}{
		{"success", t.Context(), nil, false, "success"},
		{"stream opened", t.Context(), nil, true, "success"},
		{"cancelled", cancelled, context.Canceled, true, "cancelled"},
		{"deadline", t.Context(), context.DeadlineExceeded, false, "timeout"},
		{"network timeout", t.Context(), timeoutError{}, false, "timeout"},
		{"other", t.Context(), errors.New("secret-canary in a response body"), false, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Observe(scriptedClient{err: tc.err}, tel)
			if tc.stream {
				_, err = c.ConverseStream(tc.ctx, &bedrockruntime.ConverseStreamInput{})
			} else {
				_, err = c.Converse(tc.ctx, &bedrockruntime.ConverseInput{})
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("the call's error = %v, want the client's %v", err, tc.err)
			}
		})
	}
	body := metricsText(t, tel)
	for _, series := range []string{
		`agentkourier_ax_operation_duration_seconds_count{operation="converse",outcome="success"} 1`,
		`agentkourier_ax_operation_duration_seconds_count{operation="converse_stream",outcome="success"} 1`,
		`agentkourier_ax_operation_duration_seconds_count{operation="converse_stream",outcome="cancelled"} 1`,
		`agentkourier_ax_operation_duration_seconds_count{operation="converse",outcome="timeout"} 2`,
		`agentkourier_ax_operation_duration_seconds_count{operation="converse",outcome="error"} 1`,
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
	if names["agentkourier.ax.model.converse"] != 4 || names["agentkourier.ax.model.converse_stream"] != 2 {
		t.Fatalf("spans = %v", names)
	}
}

// An error Bedrock answered with an HTTP status is counted under that status, as the SDK reports it.
func TestObserveCountsBedrocksStatus(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Amzn-Errortype", "ThrottlingException")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"Too many requests"}`))
	}))
	defer srv.Close()
	tel, err := telemetry.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := bedrockruntime.New(bedrockruntime.Options{Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), HTTPClient: srv.Client(),
		RetryMaxAttempts: 1, BearerAuthTokenProvider: bearer.StaticTokenProvider{Token: bearer.Token{Value: "test-key"}}})
	if _, err := Observe(client, tel).Converse(t.Context(), &bedrockruntime.ConverseInput{ModelId: aws.String("m")}); err == nil {
		t.Fatal("Converse succeeded against a 429")
	}
	if body := metricsText(t, tel); !strings.Contains(body, `agentkourier_ax_operation_duration_seconds_count{operation="converse",outcome="429"} 1`) {
		t.Fatalf("/metrics has no converse 429:\n%s", body)
	}
}

// Without telemetry, Observe passes each call through.
func TestObserveWithoutTelemetry(t *testing.T) {
	boom := errors.New("boom")
	if _, err := Observe(scriptedClient{err: boom}, nil).Converse(t.Context(), &bedrockruntime.ConverseInput{}); !errors.Is(err, boom) {
		t.Fatalf("Converse = %v", err)
	}
}
