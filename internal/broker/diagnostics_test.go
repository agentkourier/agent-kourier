package broker

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// quietOTel clears OpenTelemetry's process-wide logger and error handler now, so that no earlier
// test's install is in place, and again when the test ends. It has no getter to restore from.
func quietOTel(t *testing.T) {
	t.Helper()
	reset := func() {
		otel.SetLogger(logr.Discard())
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	}
	reset()
	t.Cleanup(reset)
}

// With an exporter configured, Run installs the diagnostics before it builds the exporter: the
// exporter's complaint about a variable it cannot parse reaches Run's logger, with no part of the
// value, and so does a later error the SDK hands to its handler.
func TestRunRoutesOpenTelemetryDiagnosticsToItsLogger(t *testing.T) {
	quietOTel(t)
	const canary = "LEAKCANARY"
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", canary) // the exporter parses this itself, and logs the input
	out := &logBuffer{}
	c := startConfig(t)
	c.Logger = slog.New(slog.NewTextHandler(out, nil))
	c.Telemetry = telemetry.Config{Endpoint: "https://collector.example/v1/traces", ServiceName: "test", SampleRatio: 0}
	r := start(t, c)
	r.until("/readyz", http.StatusOK)

	if !strings.Contains(out.String(), "otel: ") {
		t.Fatalf("the exporter's complaint did not reach the broker's logger: %q", out.String())
	}
	otel.Handle(errors.New("sdk error " + canary))
	if !strings.Contains(out.String(), "otel: OpenTelemetry error") {
		t.Errorf("the SDK's error handler is not the broker's: %q", out.String())
	}
	if strings.Contains(out.String(), canary) {
		t.Errorf("a canary reached the log: %q", out.String())
	}
}

// With tracing off there is no exporter, and Run leaves OpenTelemetry's process-wide hooks alone.
func TestRunLeavesOpenTelemetryDiagnosticsAloneWithoutAnEndpoint(t *testing.T) {
	quietOTel(t)
	out := &logBuffer{}
	c := startConfig(t)
	c.Logger = slog.New(slog.NewTextHandler(out, nil))
	r := start(t, c)
	r.until("/readyz", http.StatusOK)

	otel.Handle(errors.New("sdk error"))

	if strings.Contains(out.String(), "otel: ") {
		t.Errorf("Run installed the diagnostics with no exporter configured: %q", out.String())
	}
}
