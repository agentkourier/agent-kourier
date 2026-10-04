package outbox_test

import (
	"path/filepath"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
)

// A row written in one trace is delivered, after a restart, in that trace: the delivery span is
// the child of the store call that wrote the row (whose context the carrier is taken from), in the
// trace of the span that enqueued it, though the process that delivers it has no span.
func TestDeliveryContinuesTheTraceOfTheRowAcrossARestart(t *testing.T) {
	h := telemetrytest.New(t)
	path := filepath.Join(t.TempDir(), "c.db")
	st := openStore(t, path)
	enqueueCtx, enqueue := telemetry.Start(h.Ctx, "courier.turn.attempt")
	if _, err := newOutbox(t, st, &poster{}).Enqueue(enqueueCtx, reply("ev-1")); err != nil {
		t.Fatal(err)
	}
	enqueue.Finish(nil)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = openStore(t, path)
	if err := newOutbox(t, st, &poster{}).Reconcile(h.Ctx); err != nil {
		t.Fatal(err)
	}
	turn, write, deliver := h.Span(t, "courier.turn.attempt"), h.Span(t, "courier.store.CreateOutbox"), h.Span(t, "courier.outbox.deliver")
	if deliver.SpanContext.TraceID() != turn.SpanContext.TraceID() || deliver.Parent.SpanID() != write.SpanContext.SpanID() {
		t.Fatalf("delivery: trace %s parent %s; want trace %s parent %s, the span that wrote the row",
			deliver.SpanContext.TraceID(), deliver.Parent.SpanID(), turn.SpanContext.TraceID(), write.SpanContext.SpanID())
	}
	if row, err := st.GetOutbox(t.Context(), "ev-1/reply"); err != nil || row.MessageRef == "" {
		t.Fatalf("the row was not delivered: %+v, %v", row, err)
	}
}
