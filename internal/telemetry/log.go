package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// LogHandler adds trace and span IDs to contextual logs with valid trace context.
type LogHandler struct{ Handler slog.Handler }

// Enabled delegates to the wrapped handler.
func (h LogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Handler.Enabled(ctx, level)
}

// Handle adds IDs without altering the caller's record.
func (h LogHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs preserves the tracing handler on derived loggers.
func (h LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return LogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup preserves the tracing handler on grouped loggers.
func (h LogHandler) WithGroup(name string) slog.Handler {
	return LogHandler{Handler: h.Handler.WithGroup(name)}
}
