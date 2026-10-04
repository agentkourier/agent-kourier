package telemetry

import (
	"context"
	"errors"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// safeExporter keeps collector response bodies and authentication details out of SDK diagnostics.
type safeExporter struct{ next sdktrace.SpanExporter }

func (e safeExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := e.next.ExportSpans(ctx, spans); err != nil {
		return errors.New("OTLP trace export failed: " + ErrorClass(err))
	}
	return nil
}
func (e safeExporter) Shutdown(ctx context.Context) error {
	if err := e.next.Shutdown(ctx); err != nil {
		return errors.New("OTLP exporter shutdown failed: " + ErrorClass(err))
	}
	return nil
}
