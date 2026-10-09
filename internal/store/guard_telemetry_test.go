package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
)

// operationCount returns how many observations agentkourier_operation_duration_seconds holds for the operation and outcome.
func operationCount(t *testing.T, h *telemetrytest.Harness, operation, outcome string) uint64 {
	t.Helper()
	families, err := h.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, f := range families {
		if f.GetName() != "agentkourier_operation_duration_seconds" {
			continue
		}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["operation"] == operation && labels["outcome"] == outcome {
				n += m.Histogram.GetSampleCount()
			}
		}
	}
	return n
}

// inTrace returns a context that is inside a span: the store makes a span only inside a trace, and always observes its
// duration.
func inTrace(t *testing.T, h *telemetrytest.Harness) context.Context {
	t.Helper()
	ctx, root := telemetry.Start(h.Ctx, "agentkourier.test.root")
	t.Cleanup(func() { root.Finish(nil) })
	return ctx
}

// TestEveryOperationRecordsOneSpanAndOneObservation: the telemetry of the store is the guard's, one operation named
// agentkourier.store.<Method> per call, with the outcome "success". The names are the ones the dashboards and the
// observability guide use; a method that drops its telemetry, or renames it, fails here.
func TestEveryOperationRecordsOneSpanAndOneObservation(t *testing.T) {
	for _, sp := range specs() {
		t.Run(sp.method, func(t *testing.T) {
			h := telemetrytest.New(t)
			if err := sp.valid(inTrace(t, h), store.Guard(&fakeAdapter{})); err != nil {
				t.Fatal(err)
			}
			name := "agentkourier.store." + sp.method
			h.Span(t, name) // exactly one
			if n := operationCount(t, h, name, "success"); n != 1 {
				t.Errorf("%s observed %d times as success; want 1", name, n)
			}
			var spans []string
			for _, s := range h.Exporter.GetSpans() {
				if strings.HasPrefix(s.Name, "agentkourier.store.") {
					spans = append(spans, s.Name)
				}
			}
			if !slices.Equal(spans, []string{name}) {
				t.Errorf("store spans = %v; want only %s", spans, name)
			}
		})
	}
}

// TestARefusedCallIsAnOperationWithTheOutcomeInvalid: the checks run inside the operation, so a call that is refused is
// observed as "invalid", as it was when each store did its own.
func TestARefusedCallIsAnOperationWithTheOutcomeInvalid(t *testing.T) {
	h := telemetrytest.New(t)
	a := &fakeAdapter{}
	if err := completeEvent("", 1)(inTrace(t, h), store.Guard(a)); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("CompleteEvent with no ID = %v", err)
	}
	h.Span(t, "agentkourier.store.CompleteEvent")
	if n := operationCount(t, h, "agentkourier.store.CompleteEvent", "invalid"); n != 1 {
		t.Errorf("observed %d times as invalid; want 1", n)
	}
}

// TestTheOutcomeOfAnOperationIsClassifiedOnce: each error the store knows has its own bounded outcome, a missing row
// from a driver counts as not found when the backend names it, and any other error is classified as every operation's is.
func TestTheOutcomeOfAnOperationIsClassifiedOnce(t *testing.T) {
	wrap := func(e error) error { return fmt.Errorf("get event: %w", e) }
	for _, c := range []struct {
		err     error
		outcome string
	}{
		{nil, "success"},
		{wrap(store.ErrNotFound), "not_found"},
		{wrap(sql.ErrNoRows), "not_found"},
		{wrap(store.ErrConflict), "conflict"},
		{wrap(store.ErrExpired), "expired"},
		{wrap(store.ErrLimit), "limited"},
		{wrap(store.ErrInvalid), "invalid"},
		{wrap(store.ErrDeposed), "error"},
		{wrap(context.Canceled), "cancelled"},
		{wrap(context.DeadlineExceeded), "timeout"},
		{errors.New("disk on fire"), "error"},
	} {
		t.Run(c.outcome, func(t *testing.T) {
			h := telemetrytest.New(t)
			g := store.Guard(&fakeAdapter{err: c.err}, store.WithNoRows(sql.ErrNoRows))
			_, err := g.GetEvent(inTrace(t, h), "e1")
			if !errors.Is(err, c.err) {
				t.Fatalf("GetEvent = %v; want %v", err, c.err)
			}
			if n := operationCount(t, h, "agentkourier.store.GetEvent", c.outcome); n != 1 {
				t.Errorf("observed %d times as %s; want 1", n, c.outcome)
			}
		})
	}
}

// TestADriverErrorIsNotNotFoundUnlessTheBackendSaysSo: WithNoRows is what turns a driver's error into the outcome.
func TestADriverErrorIsNotNotFoundUnlessTheBackendSaysSo(t *testing.T) {
	h := telemetrytest.New(t)
	g := store.Guard(&fakeAdapter{err: sql.ErrNoRows})
	_, _ = g.GetEvent(inTrace(t, h), "e1")
	if n := operationCount(t, h, "agentkourier.store.GetEvent", "not_found"); n != 0 {
		t.Errorf("a driver's error counted as not found without WithNoRows")
	}
}

// TestScrubbedErrorsAreWhatIsRecordedAndReturned: the backend's scrub runs before the outcome is recorded and before the
// error leaves, on an operation's error and on a claim's, and errors.Is still sees through it.
func TestScrubbedErrorsAreWhatIsRecordedAndReturned(t *testing.T) {
	const secret = "pw-FAKE-1234"
	leak := fmt.Errorf("connect %s: %w", secret, store.ErrConflict)
	scrub := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("%s: %w", strings.ReplaceAll(err.Error(), secret, "[redacted]"), errors.Unwrap(err))
	}
	h := telemetrytest.New(t)
	g := store.Guard(&fakeAdapter{err: leak}, store.WithScrub(scrub))
	_, err := g.GetEvent(inTrace(t, h), "e1")
	if err == nil || strings.Contains(err.Error(), secret) || !errors.Is(err, store.ErrConflict) {
		t.Errorf("GetEvent = %v; want the error without the secret, still a conflict", err)
	}
	if err = g.ClaimReplyInteraction(t.Context(), "r1", "q1"); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("ClaimReplyInteraction = %v; want the error without the secret", err)
	}
	if err = g.SetEventStatus(t.Context(), "e1", "bogus", ""); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("SetEventStatus = %v; want a refusal to be scrubbed and still ErrInvalid", err)
	}
	h.RequireNone(t, secret)
}

// TestARowKeepsTheTraceTheCallerGaveAndOtherwiseTakesTheCalls: a row that carries a trace keeps it; one that carries
// none takes the trace of the call's context. An event takes the producer's, outside the span of the call, so that the
// queue worker continues the producer's trace; the other rows take the span of the call.
func TestARowKeepsTheTraceTheCallerGaveAndOtherwiseTakesTheCalls(t *testing.T) {
	rows := []struct {
		method string
		call   func(ctx context.Context, s store.Store, parent, state string) error
		// got returns the trace the adapter was given in the row.
		got func(row any) (parent, state string)
		// ownSpan is true when the row takes the span of the store call rather than the caller's.
		ownSpan bool
	}{
		{"AppendEvent", func(ctx context.Context, s store.Store, p, st string) error {
			return appendEvent(func(e *store.Event) { e.TraceParent, e.TraceState = p, st })(ctx, s)
		}, func(r any) (string, string) { e := r.(store.Event); return e.TraceParent, e.TraceState }, false},
		{"CreateInteraction", func(ctx context.Context, s store.Store, p, st string) error {
			return createInteraction(func(i *store.Interaction) { i.TraceParent, i.TraceState = p, st })(ctx, s)
		}, func(r any) (string, string) { i := r.(store.Interaction); return i.TraceParent, i.TraceState }, true},
		{"CreateOutbox", func(ctx context.Context, s store.Store, p, st string) error {
			return createOutbox(func(o *store.OutboxEntry) { o.TraceParent, o.TraceState = p, st })(ctx, s)
		}, func(r any) (string, string) { o := r.(store.OutboxEntry); return o.TraceParent, o.TraceState }, true},
		{"RecordPress", func(ctx context.Context, s store.Store, p, st string) error {
			return recordPress(func(r *store.Press) { r.TraceParent, r.TraceState = p, st }, okLimits)(ctx, s)
		}, func(r any) (string, string) { p := r.(store.Press); return p.TraceParent, p.TraceState }, true},
		{"EnqueueReply", func(ctx context.Context, s store.Store, p, st string) error {
			return enqueueReply(func(r *store.Reply) { r.TraceParent, r.TraceState = p, st })(ctx, s)
		}, func(r any) (string, string) { e := r.(store.Reply); return e.TraceParent, e.TraceState }, true},
		{"UpsertSession", func(ctx context.Context, s store.Store, p, st string) error {
			return upsertSession(func(r *store.Session) { r.TraceParent, r.TraceState = p, st })(ctx, s)
		}, func(r any) (string, string) { e := r.(store.Session); return e.TraceParent, e.TraceState }, true},
	}
	for _, c := range rows {
		t.Run(c.method, func(t *testing.T) {
			a := &fakeAdapter{}
			if err := c.call(inTrace(t, telemetrytest.New(t)), store.Guard(a), "00-given-01", "k=v"); err != nil {
				t.Fatal(err)
			}
			if p, st := c.got(a.row); p != "00-given-01" || st != "k=v" {
				t.Errorf("a row with a trace was given %q, %q; want its own kept", p, st)
			}

			h := telemetrytest.New(t)
			ctx := inTrace(t, h)
			caller := trace.SpanContextFromContext(ctx)
			a = &fakeAdapter{}
			if err := c.call(ctx, store.Guard(a), "", ""); err != nil {
				t.Fatal(err)
			}
			p, _ := c.got(a.row)
			stored := h.Span(t, "agentkourier.store."+c.method)
			want := caller.SpanID()
			if c.ownSpan {
				want = stored.SpanContext.SpanID()
			}
			if !strings.Contains(p, want.String()) {
				t.Errorf("a row with no trace was given %q; want the span %s (the %s)", p, want, map[bool]string{true: "store call's", false: "caller's"}[c.ownSpan])
			}
		})
	}
}
