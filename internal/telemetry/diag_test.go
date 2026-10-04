package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
)

const leakCanary = "LEAKCANARY"

// captureDiagnostics installs the courier diagnostics over a buffer-backed logger and puts
// OpenTelemetry's globals back when the test ends.
func captureDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	t.Cleanup(func() { // OpenTelemetry offers no getter, so the default is not restored
		otel.SetLogger(logr.Discard())
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	})
	var buf bytes.Buffer
	InstallDiagnostics(slog.New(slog.NewJSONHandler(&buf, nil)))
	return &buf
}

// The exporter parses its own OTEL_EXPORTER_OTLP_* variables as it is built, and on bad input its
// parsers log the input, or an error that holds it, through OpenTelemetry's global logger.
// LoadConfig refuses most of these, but Provider must be safe on its own.
func TestExporterDiagnosticsNeverCarryEnvironmentValues(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer%zz" + leakCanary},
		{"OTEL_EXPORTER_OTLP_TRACES_HEADERS", "Authorization=Bearer%zz" + leakCanary},
		{"OTEL_EXPORTER_OTLP_HEADERS", "Bearer" + leakCanary},
		{"OTEL_EXPORTER_OTLP_TRACES_HEADERS", "bad name " + leakCanary + "=x"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "http://user:" + leakCanary + "@[collector"},
		{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://[" + leakCanary},
		{"OTEL_EXPORTER_OTLP_TIMEOUT", leakCanary},
		{"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", leakCanary},
		{"OTEL_EXPORTER_OTLP_CERTIFICATE", "/nonexistent/" + leakCanary},
		{"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", "/nonexistent/" + leakCanary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureDiagnostics(t)
			t.Setenv(tc.name, tc.value)
			_, shutdown, err := (Config{Endpoint: "https://collector.example/v1/traces", SampleRatio: 1}).Provider(t.Context(), "test", slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = shutdown(context.Background()) }()
			if !strings.Contains(buf.String(), `"msg":"otel: `) {
				t.Fatalf("the exporter's complaint did not reach the courier logger: %q", buf.String())
			}
			if strings.Contains(buf.String(), leakCanary) {
				t.Fatalf("a variable's value reached the log: %s", buf.String())
			}
		})
	}
}

func TestSDKErrorHandlerDropsErrorText(t *testing.T) {
	buf := captureDiagnostics(t)
	for range 3 {
		otel.Handle(errors.New("collector said " + leakCanary))
	}
	got := logRecords(t, buf)
	if len(got) != 1 || got[0]["level"] != "WARN" || got[0]["msg"] != "otel: OpenTelemetry error" || got[0]["error_type"] != "error" {
		t.Fatalf("log = %v, want one WARN for three identical errors", got)
	}
	if strings.Contains(buf.String(), leakCanary) {
		t.Fatalf("error text reached the log: %s", buf.String())
	}
}

func TestSDKErrorHandlerLeavesExportFailuresToTheExporter(t *testing.T) {
	buf := captureDiagnostics(t)
	otel.Handle(errExportFailed)
	otel.Handle(errExporterShutdown)
	if buf.Len() != 0 {
		t.Fatalf("an export failure was logged twice: %s", buf.String())
	}
}

func TestDiagnosticsKeepOnlyErrorsAndWarnings(t *testing.T) {
	var buf bytes.Buffer
	l := diagLogger(slog.New(slog.NewJSONHandler(&buf, nil)), newWarnLimiter(time.Now, warnEvery))
	l.V(4).Info("info-level " + leakCanary)
	l.V(8).Info("debug-level " + leakCanary)
	if buf.Len() != 0 {
		t.Fatalf("verbose diagnostics were logged: %s", buf.String())
	}
	l.V(1).Info("warning", "value", leakCanary)
	l.WithValues("secret", leakCanary).WithName("x").Error(errors.New(leakCanary), "failure")
	got := logRecords(t, &buf)
	if len(got) != 2 || strings.Contains(buf.String(), leakCanary) {
		t.Fatalf("log = %s", buf.String())
	}
	if _, has := got[0]["error_type"]; has {
		t.Fatal("a warning without an error carries an error class")
	}
}

func TestDiagnosticsRedactTheirMessage(t *testing.T) {
	var buf bytes.Buffer
	diagLogger(slog.New(slog.NewJSONHandler(&buf, nil)), newWarnLimiter(time.Now, warnEvery)).Error(nil, "refused Authorization: Bearer "+leakCanary)
	if strings.Contains(buf.String(), leakCanary) || buf.Len() == 0 {
		t.Fatalf("log = %q", buf.String())
	}
}

func TestDiagnosticsWarnAgainAfterTheInterval(t *testing.T) {
	var buf bytes.Buffer
	clock := &fakeClock{t: time.Unix(1000, 0)}
	limit := newWarnLimiter(clock.now, warnEvery)
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	diagnostic(log, limit, "same", nil)
	diagnostic(log, limit, "same", nil)
	clock.t = clock.t.Add(warnEvery)
	diagnostic(log, limit, "same", nil)
	got := logRecords(t, &buf)
	if len(got) != 2 || got[1]["suppressed"] != float64(1) {
		t.Fatalf("log = %v", got)
	}
}
