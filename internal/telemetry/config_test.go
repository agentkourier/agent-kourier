package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

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
		{"credentials", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://user:canary@collector"}, "", true},
		{"query", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector/?token=canary"}, "", true},
		{"sampler", map[string]string{"OTEL_TRACES_SAMPLER": "unsupported"}, "", true},
		{"nan", map[string]string{"OTEL_TRACES_SAMPLER_ARG": "NaN"}, "", true},
		{"ratio", map[string]string{"OTEL_TRACES_SAMPLER_ARG": "1.1"}, "", true},
		{"headers", map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "secret-header-no-equals-canary"}, "", true},
		// The exporter parses both variables of a pair, so a bad one is refused even when the other wins.
		{"bad base endpoint under a good specific one", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://user:canary@collector", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://specific/traces"}, "", true},
		{"unparsable base endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://[canary", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://specific/traces"}, "", true},
		{"bad specific endpoint under a good base one", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://base", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "ftp://canary/traces"}, "", true},
		{"bad escape in general headers, specific set", map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer%zzcanary", "OTEL_EXPORTER_OTLP_TRACES_HEADERS": "X-Key=ok"}, "", true},
		{"bad escape in specific headers", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_HEADERS": "Authorization=Bearer%zzcanary"}, "", true},
		{"missing equals in general headers, specific set", map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "canary", "OTEL_EXPORTER_OTLP_TRACES_HEADERS": "X-Key=ok"}, "", true},
		{"header name that is not a token", map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "bad name canary=x"}, "", true},
		{"http endpoint with headers", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer%20fake"}, "", true},
		{"http trace endpoint with headers", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector/t", "OTEL_EXPORTER_OTLP_TRACES_HEADERS": "Authorization=x"}, "", true},
		{"http endpoint with headers, opted in", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=x", "COURIER_OTLP_ALLOW_INSECURE_HEADERS": "true"}, "http://collector:4318/v1/traces", false},
		{"http endpoint with headers, opt-in false", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=x", "COURIER_OTLP_ALLOW_INSECURE_HEADERS": "false"}, "", true},
		{"opt-in is only exactly true", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=x", "COURIER_OTLP_ALLOW_INSECURE_HEADERS": "canary"}, "", true},
		{"http endpoint without headers", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, "http://collector:4318/v1/traces", false},
		{"https endpoint with headers", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=x"}, "https://collector/v1/traces", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := LoadConfig(func(k string) string { return tc.env[k] })
			if (err != nil) != tc.bad {
				t.Fatalf("config=%+v err=%v", c, err)
			}
			if err != nil && strings.Contains(err.Error(), "canary") {
				t.Fatal("configuration error exposed a secret")
			}
			if err == nil && c.Endpoint != tc.endpoint {
				t.Fatalf("endpoint=%s", c.Endpoint)
			}
		})
	}
}

func TestLoadConfigHeaders(t *testing.T) {
	env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector", "OTEL_EXPORTER_OTLP_HEADERS": "General=g", "OTEL_EXPORTER_OTLP_TRACES_HEADERS": "Authorization=Bearer%20fake, X-Key = a%2Cb "}
	c, err := LoadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Headers) != 2 || c.Headers["Authorization"] != "Bearer fake" || c.Headers["X-Key"] != "a,b" {
		t.Fatalf("trace headers must win and be decoded: %v", c.Headers)
	}
	delete(env, "OTEL_EXPORTER_OTLP_TRACES_HEADERS")
	if c, err = LoadConfig(func(k string) string { return env[k] }); err != nil || c.Headers["General"] != "g" || len(c.Headers) != 1 {
		t.Fatalf("general headers = %v, err %v", c.Headers, err)
	}
}

func TestOTLPHTTPExportAndBoundedShutdown(t *testing.T) {
	received := make(chan *collectortrace.ExportTraceServiceRequest, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Authorization") != "Bearer fake" {
			t.Error("export endpoint or authentication wrong")
		}
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
	c := Config{Endpoint: collector.URL + "/v1/traces", ServiceName: "courier-test", Environment: "test", SampleRatio: 1, Headers: map[string]string{"Authorization": "Bearer fake"}}
	provider, shutdown, err := c.Provider(t.Context(), "test-version", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	o, err := New(prometheus.NewRegistry(), provider)
	if err != nil {
		t.Fatal(err)
	}
	_, op := Start(WithObserver(t.Context(), o), "courier.intake")
	op.Finish(nil)
	flush, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdown(flush); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-received:
		if len(req.ResourceSpans) != 1 || len(req.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
			t.Fatal("span missing from OTLP batch")
		}
		attrs := map[string]string{}
		for _, a := range req.ResourceSpans[0].Resource.Attributes {
			attrs[a.Key] = a.Value.GetStringValue()
		}
		if attrs["service.name"] != "courier-test" || attrs["service.version"] != "test-version" || attrs["deployment.environment.name"] != "test" {
			t.Fatalf("resource=%v", attrs)
		}
	default:
		t.Fatal("shutdown did not flush")
	}
}

func TestUnsampledRootsKeepMetricsAndRespectSampledParents(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Content-Type", "application/x-protobuf") }))
	defer collector.Close()
	provider, shutdown, err := (Config{Endpoint: collector.URL + "/v1/traces", ServiceName: "test", SampleRatio: 0}).Provider(t.Context(), "test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()
	reg := prometheus.NewRegistry()
	obs, err := New(reg, provider)
	if err != nil {
		t.Fatal(err)
	}
	base := WithObserver(t.Context(), obs)
	root, op := Start(base, "courier.intake")
	if trace.SpanFromContext(root).IsRecording() {
		t.Fatal("zero root sampling ratio recorded a root")
	}
	op.Finish(nil)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range families {
		if f.GetName() == "courier_operation_duration_seconds" {
			found = f.Metric[0].Histogram.GetSampleCount() == 1
		}
	}
	if !found {
		t.Fatal("unsampled operation lost its metric")
	}
	parent := Restore(base, "00-11111111111111111111111111111111-2222222222222222-01", "")
	child, childOp := Start(parent, "courier.intake")
	if !trace.SpanFromContext(child).IsRecording() {
		t.Fatal("sampled remote parent was ignored")
	}
	childOp.Finish(nil)
}
