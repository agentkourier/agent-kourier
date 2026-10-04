package queue

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
)

// An event appended in one trace is handled, after a restart, in that trace: the processing span
// descends from the span that appended the event, and the handler's context carries the
// trace, though the process that claims the event has no span.
func TestEventHandlingContinuesTheTraceOfTheEventAcrossARestart(t *testing.T) {
	h := telemetrytest.New(t)
	path := filepath.Join(t.TempDir(), "courier.db")
	st, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	appendCtx, intake := telemetry.Start(h.Ctx, "courier.intake")
	if _, err = st.AppendEvent(appendCtx, store.Event{ID: "ev", Kind: "alert", BindingKey: "ns/b", Payload: []byte("private-payload-canary"), ReceivedAt: t0}); err != nil {
		t.Fatal(err)
	}
	intake.Finish(nil)
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err = sqlite.Open(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var handling trace.SpanContext
	w, err := New(Config{Queue: st, Lease: lease, MaxAttempts: maxAttempts, Now: func() time.Time { return t0 },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handler: HandlerFunc(func(ctx context.Context, _ store.Event) error {
			handling = trace.SpanContextFromContext(ctx)
			return nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	if handled, err := w.RunOnce(h.Ctx); err != nil || !handled {
		t.Fatalf("RunOnce = %v, %v; want the event handled", handled, err)
	}
	intakeSpan := h.Span(t, "courier.intake")
	process, handle := h.Span(t, "courier.queue.process"), h.Span(t, "courier.queue.handle")
	if process.SpanContext.TraceID() != intakeSpan.SpanContext.TraceID() || process.Parent.SpanID() != intakeSpan.SpanContext.SpanID() {
		t.Fatalf("processing: trace %s parent %s; want trace %s, parent %s: the span that appended the event",
			process.SpanContext.TraceID(), process.Parent.SpanID(), intakeSpan.SpanContext.TraceID(), intakeSpan.SpanContext.SpanID())
	}
	if handle.Parent.SpanID() != process.SpanContext.SpanID() || handling.TraceID() != intakeSpan.SpanContext.TraceID() {
		t.Fatalf("the handler ran in trace %s; want the event's %s", handling.TraceID(), intakeSpan.SpanContext.TraceID())
	}
	h.RequireNone(t, "private-payload-canary")
}
