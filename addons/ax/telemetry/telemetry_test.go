package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/agentkourier/agent-kourier/addons/ax/auth"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The broker's rules, with the agent's default service name. No value of a variable appears in an error.
func TestLoadConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		endpoint string
		bad      bool
	}{
		{"disabled", nil, "", false},
		{"base", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, "http://collector:4318/v1/traces", false},
		{"specific", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://base:4318", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://specific/traces"}, "https://specific/traces", false},
		{"none", map[string]string{"OTEL_TRACES_EXPORTER": "none", "OTEL_EXPORTER_OTLP_ENDPOINT": "bad"}, "", false},
		{"other exporter", map[string]string{"OTEL_TRACES_EXPORTER": "zipkin"}, "", true},
		{"credentials", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://user:canary@collector"}, "", true},
		{"query", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector/?token=canary"}, "", true},
		{"grpc", map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, "", true},
		{"sampler", map[string]string{"OTEL_TRACES_SAMPLER": "always_on"}, "", true},
		{"ratio", map[string]string{"OTEL_TRACES_SAMPLER_ARG": "1.1"}, "", true},
		{"headers", map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "secret-header-no-equals-canary"}, "", true},
		{"bad base endpoint under a good specific one", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://user:canary@collector", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://specific/traces"}, "", true},
		{"http endpoint with headers", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer%20canary"}, "", true},
		{"upper-case HTTP endpoint with headers", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "HTTP://collector:4318", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=x"}, "", true},
		{"http endpoint with headers, opted in", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=x", "AGENTKOURIER_OTLP_ALLOW_INSECURE_HEADERS": "true"}, "http://collector:4318/v1/traces", false},
		{"opt-in is only exactly true", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=x", "AGENTKOURIER_OTLP_ALLOW_INSECURE_HEADERS": "canary"}, "", true},
		{"https endpoint with headers", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=x"}, "https://collector/v1/traces", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := LoadConfig(func(k string) string { return tc.env[k] })
			if (err != nil) != tc.bad {
				t.Fatalf("config=%+v err=%v", c, err)
			}
			if err != nil && strings.Contains(err.Error(), "canary") {
				t.Fatal("a configuration error shows a value")
			}
			if err == nil && c.Endpoint != tc.endpoint {
				t.Fatalf("endpoint=%s", c.Endpoint)
			}
		})
	}
	c, err := LoadConfig(func(string) string { return "" })
	if err != nil || c.ServiceName != "agent-kourier-ax" || c.SampleRatio != 1 {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	c, err = LoadConfig(func(k string) string { return map[string]string{"OTEL_SERVICE_NAME": "ax-prod"}[k] })
	if err != nil || c.ServiceName != "ax-prod" {
		t.Fatalf("OTEL_SERVICE_NAME = %+v, %v", c, err)
	}
}

// Spans go to the collector over OTLP/HTTP with the resource's service name and version.
func TestProviderExportsWithTheVersion(t *testing.T) {
	received := make(chan *collectortrace.ExportTraceServiceRequest, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		req := &collectortrace.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(raw, req); err != nil {
			t.Error(err)
			return
		}
		received <- req
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer collector.Close()
	c, err := LoadConfig(func(k string) string { return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": collector.URL}[k] })
	if err != nil {
		t.Fatal(err)
	}
	provider, shutdown, err := c.Provider(t.Context(), "v9.8.7", discard())
	if err != nil {
		t.Fatal(err)
	}
	tel, err := New(provider)
	if err != nil {
		t.Fatal(err)
	}
	_, turn := tel.StartTurn(t.Context(), "task-1", "ctx-1")
	turn.End(TurnCompleted)
	flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(flush); err != nil {
		t.Fatal(err)
	}
	req := <-received
	attrs := map[string]string{}
	for _, a := range req.ResourceSpans[0].Resource.Attributes {
		attrs[a.Key] = a.Value.GetStringValue()
	}
	if attrs["service.name"] != "agent-kourier-ax" || attrs["service.version"] != "v9.8.7" {
		t.Fatalf("resource = %v", attrs)
	}
	if name := req.ResourceSpans[0].ScopeSpans[0].Spans[0].Name; name != "agentkourier.ax.turn" {
		t.Fatalf("span = %s", name)
	}
}

type rejectedExporter struct{}

func (rejectedExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("collector echoed secret-credential-canary")
}
func (rejectedExporter) Shutdown(context.Context) error { return nil }

// What a collector answers never reaches the SDK or the log.
func TestExportFailureKeepsTheCollectorsText(t *testing.T) {
	var logs bytes.Buffer
	e := newSafeExporter(rejectedExporter{}, slog.New(slog.NewJSONHandler(&logs, nil)), time.Now)
	err := e.ExportSpans(t.Context(), nil)
	if !errors.Is(err, errExportFailed) || strings.Contains(err.Error(), "canary") {
		t.Fatalf("the SDK was told %v", err)
	}
	if logs.Len() == 0 || strings.Contains(logs.String(), "canary") {
		t.Fatalf("log = %q", logs.String())
	}
}

// Every metric is registered under its name, and each recorder moves it.
func TestMetricsAreRegisteredAndCount(t *testing.T) {
	tel, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, turn := tel.StartTurn(t.Context(), "t", "c")
	turn.End(TurnInputRequired)
	_, call := tel.StartCall(t.Context(), OpConverseStream)
	call.End("429")
	tel.AuthRefused(AuthUnavailable)
	tel.SessionsDeleted(DeleteIdle, 2)
	tel.SessionsDeleted(DeleteIdle, 0)
	tel.MessageReplay()
	tel.TasksInterrupted(3)
	tel.TasksInterrupted(0)
	tel.SetLeader(true)
	tel.Takeover()
	tel.Forward(ForwardNoLeader)

	for name, want := range map[string]float64{
		`agentkourier_ax_turns_total{outcome="input_required"}`:                                       1,
		`agentkourier_ax_turns_total{outcome="interrupted"}`:                                          3,
		`agentkourier_ax_auth_refused_total{reason="unavailable"}`:                                    1,
		`agentkourier_ax_sessions_deleted_total{reason="idle"}`:                                       2,
		`agentkourier_ax_message_replays_total`:                                                       1,
		`agentkourier_ax_tasks_interrupted_total`:                                                     3,
		`agentkourier_ax_leader`:                                                                      1,
		`agentkourier_ax_leader_takeovers_total`:                                                      1,
		`agentkourier_ax_forwarded_requests_total{outcome="no_leader"}`:                               1,
		`agentkourier_ax_operation_duration_seconds_count{operation="converse_stream",outcome="429"}`: 1,
	} {
		if got := gauge(t, tel, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if n := testutil.CollectAndCount(tel.duration, "agentkourier_ax_operation_duration_seconds"); n != 1 {
		t.Errorf("operation_duration_seconds series = %d, want 1", n)
	}
	families, err := tel.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, name := range []string{"go_goroutines", "process_start_time_seconds", "agentkourier_ax_operation_duration_seconds"} {
		if !seen[name] {
			t.Errorf("%s is not gathered", name)
		}
	}
	tel.SetLeader(false)
	if got := gauge(t, tel, "agentkourier_ax_leader"); got != 0 {
		t.Fatalf("leader after SetLeader(false) = %v", got)
	}
}

// The labels are the auth package's reasons: a refusal the check reports is counted under its own name.
func TestAuthReasonsAreTheChecks(t *testing.T) {
	for want, got := range map[string]AuthReason{auth.ReasonMissing: AuthMissing, auth.ReasonInvalid: AuthInvalid, auth.ReasonUnavailable: AuthUnavailable} {
		if string(got) != want {
			t.Errorf("the label %q is not package auth's reason %q", got, want)
		}
	}
}

// A nil Telemetry records nothing, and nothing it hands out fails.
func TestNilTelemetryRecordsNothing(t *testing.T) {
	var tel *Telemetry
	_, turn := tel.StartTurn(t.Context(), "t", "c")
	turn.End(TurnCompleted)
	_, call := tel.StartCall(t.Context(), OpConverse)
	call.End("success")
	tel.AuthRefused(AuthInvalid)
	tel.SessionsDeleted(DeleteIdle, 1)
	tel.MessageReplay()
	tel.TasksInterrupted(1)
	tel.SetLeader(true)
	tel.Takeover()
	tel.Forward(Forwarded)
	rec := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a nil Telemetry's handler = %d, want 404", rec.Code)
	}
}

// gauge reads the one series of a metric, named as Prometheus prints it, from the registry.
func gauge(t *testing.T, tel *Telemetry, series string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	tel.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			return f
		}
	}
	t.Fatalf("%s is not served", series)
	return 0
}

// The broker's traceparent makes the turn's span its child.
func TestExtractMakesTheTurnAChildOfTheCaller(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tel, err := New(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	if err != nil {
		t.Fatal(err)
	}
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	h := Extract(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The turn runs in a context the request's values reach, cancellation aside, as a2a-go's executor's does.
		ctx, turn := tel.StartTurn(context.WithoutCancel(r.Context()), "task-1", "ctx-1")
		_, call := tel.StartCall(ctx, OpConverse)
		call.End("success")
		turn.End(TurnCompleted)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/a2a", nil)
	req.Header.Set("traceparent", parent)
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want 2", len(spans))
	}
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}
	turn, call := byName["agentkourier.ax.turn"], byName["agentkourier.ax.model.converse"]
	if turn == nil || call == nil {
		t.Fatalf("spans = %v", byName)
	}
	if got := turn.Parent().TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" || turn.Parent().SpanID().String() != "00f067aa0ba902b7" || !turn.Parent().IsRemote() {
		t.Fatalf("turn parent = %+v, want the caller's span", turn.Parent())
	}
	if turn.SpanKind() != trace.SpanKindServer || call.Parent().SpanID() != turn.SpanContext().SpanID() {
		t.Fatalf("the call is not the turn's child: %+v", call.Parent())
	}
	attrs := map[string]string{}
	for _, a := range turn.Attributes() {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs[AttrTaskID] != "task-1" || attrs[AttrContextID] != "ctx-1" || attrs[AttrOutcome] != "completed" {
		t.Fatalf("turn attributes = %v", attrs)
	}
}
