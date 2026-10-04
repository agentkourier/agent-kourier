package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type rejectedExporter struct{}

func (rejectedExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("collector echoed secret-credential-canary")
}
func (rejectedExporter) Shutdown(context.Context) error {
	return errors.New("collector echoed secret-credential-canary")
}
func TestCollectorErrorDetailsDoNotEnterSDKDiagnostics(t *testing.T) {
	e := safeExporter{next: rejectedExporter{}}
	for _, err := range []error{e.ExportSpans(t.Context(), nil), e.Shutdown(t.Context())} {
		if err == nil || strings.Contains(err.Error(), "canary") {
			t.Fatal("collector error was not scrubbed")
		}
	}
}
