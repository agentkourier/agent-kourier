package interact_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
)

// A press recorded in one trace is processed, after a restart, in that trace: the processing span
// descends from the span of the store call that recorded the press, and the processor's own
// context carries that trace, though the worker that takes it up has no span of its own.
func TestPressProcessingContinuesTheTraceOfThePressAcrossARestart(t *testing.T) {
	h := telemetrytest.New(t)
	path := sqlitetest.Path(t)
	st, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	key := store.ConversationKey{Connection: "ns/conn", Channel: "C1", Thread: "1.0"}
	pressCtx, press := telemetry.Start(h.Ctx, "courier.interaction.press")
	if _, err = st.CreateInteraction(pressCtx, store.Interaction{ID: "pause-1", Key: key, TaskID: "task-1", Kind: store.InteractionAskUser, CreatedAt: d0, Deadline: d0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RecordPress(pressCtx, store.Press{InteractionID: "pause-1", User: "UALICE1", Channel: "C1", MessageRef: "100.1", PressedAt: d0}, store.PressLimits{Total: 10, PerUser: 5}); err != nil {
		t.Fatal(err)
	}
	press.Finish(nil)
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err = sqlite.Open(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	presses, err := st.ListPresses(t.Context())
	if err != nil || len(presses) != 1 {
		t.Fatalf("presses = %v, %v; want the one that was recorded", presses, err)
	}

	seen := make(chan trace.SpanContext, 1)
	proc := &fakeProc{handle: func(ctx context.Context, _ store.Press) (interact.Result, error) {
		seen <- trace.SpanContextFromContext(ctx)
		return interact.Done, nil
	}}
	w, err := interact.NewWorker(interact.WorkerConfig{Processor: proc, Now: func() time.Time { return d0 }, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.Ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	defer cancel()
	w.Submit(presses[0])
	var processing trace.SpanContext
	select {
	case processing = <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the press was not processed")
	}
	cancel()
	<-done
	root, write := h.Span(t, "courier.interaction.press"), h.Span(t, "courier.store.RecordPress")
	process := h.Span(t, "courier.interaction.process")
	if processing.TraceID() != root.SpanContext.TraceID() || process.SpanContext.TraceID() != root.SpanContext.TraceID() {
		t.Fatalf("processed in trace %s, span in %s; want the press's trace %s", processing.TraceID(), process.SpanContext.TraceID(), root.SpanContext.TraceID())
	}
	if process.Parent.SpanID() != write.SpanContext.SpanID() {
		t.Fatalf("processing parent %s; want the span %s that recorded the press", process.Parent.SpanID(), write.SpanContext.SpanID())
	}
}
