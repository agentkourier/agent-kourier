package telemetry

import (
	"context"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// flushOne ends one span carrying attrs through Config.Provider and shuts the provider down,
// which flushes it.
func flushOne(t *testing.T, c Config, attrs ...attribute.KeyValue) {
	t.Helper()
	provider, shutdown, err := c.Provider(t.Context(), "test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	obs, err := New(prometheus.NewRegistry(), provider)
	if err != nil {
		t.Fatal(err)
	}
	_, op := Start(WithObserver(t.Context(), obs), "courier.intake", attrs...)
	op.Finish(nil)
	flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(flush); err != nil {
		t.Fatal(err)
	}
}

func TestSpanAttributeValuesAreBounded(t *testing.T) {
	received := make(chan *collectortrace.ExportTraceServiceRequest, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := &collectortrace.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(raw, req); err != nil {
			t.Error(err)
		}
		received <- req
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer collector.Close()
	flushOne(t, Config{Endpoint: collector.URL + "/v1/traces", ServiceName: "test", SampleRatio: 1},
		attribute.String("courier.task.id", strings.Repeat("x", 4096)), attribute.String("courier.context.id", "short"))
	got := map[string]string{}
	for _, a := range (<-received).ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes {
		got[a.Key] = a.Value.GetStringValue()
	}
	if len(got["courier.task.id"]) != 256 || got["courier.context.id"] != "short" {
		t.Fatalf("task id has %d bytes, want 256; context id %q", len(got["courier.task.id"]), got["courier.context.id"])
	}
}

// The exporter honours these variables itself, and docs/observability.md says so. The test pins
// the documentation to the behaviour, and the endpoint's scheme to the last word on transport
// security.
func TestDocumentedExporterEnvironment(t *testing.T) {
	received := make(chan struct{}, 1)
	collector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer collector.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: collector.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", ca)
	// Honoured over the https:// scheme, this would send plain HTTP to a TLS port, and fail.
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "true")
	flushOne(t, Config{Endpoint: collector.URL + "/v1/traces", ServiceName: "test", SampleRatio: 1})
	select {
	case <-received:
	default:
		t.Fatal("the export did not reach the TLS collector, trusting OTEL_EXPORTER_OTLP_CERTIFICATE")
	}
}
