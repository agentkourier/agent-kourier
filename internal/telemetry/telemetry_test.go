package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func traced(t *testing.T) (context.Context, *tracetest.InMemoryExporter, *prometheus.Registry) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	p := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	reg := prometheus.NewRegistry()
	o, err := New(reg, p)
	if err != nil {
		t.Fatal(err)
	}
	return WithObserver(t.Context(), o), exporter, reg
}

func TestDurableContextAndIsolation(t *testing.T) {
	ctx, exporter, reg := traced(t)
	root, op := Start(ctx, "courier.intake")
	parent, state := Capture(root)
	op.Finish(nil)
	worker := Restore(ctx, parent, state)
	child, attempt := Start(worker, "courier.turn.attempt")
	if trace.SpanContextFromContext(child).TraceID() != trace.SpanContextFromContext(root).TraceID() {
		t.Fatal("durable work started a different trace")
	}
	attempt.Finish(nil)
	attempt.Finish(errors.New("should have no effect"))
	spans := exporter.GetSpans()
	if len(spans) != 2 || spans[1].Parent.SpanID() != spans[0].SpanContext.SpanID() {
		t.Fatalf("span ancestry = %+v", spans)
	}
	_, other, otherReg := traced(t)
	Count(ctx, "courier_intake_total", "message", "accepted")
	if len(other.GetSpans()) != 0 {
		t.Fatal("broker instances shared a provider")
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range families {
		if m.GetName() == "courier_operation_duration_seconds" {
			for _, series := range m.Metric {
				if series.Histogram.GetSampleCount() != 1 {
					t.Fatal("Finish counted twice")
				}
			}
		}
	}
	families, err = otherReg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range families {
		if m.GetName() == "courier_intake_total" {
			t.Fatal("metric crossed broker instances")
		}
	}
	if got := trace.SpanContextFromContext(Restore(root, "bad header", "secret")); !got.Equal(trace.SpanContextFromContext(root)) {
		t.Fatal("malformed carrier changed ancestry")
	}
}

func TestErrorTelemetryAndLogsExcludeContents(t *testing.T) {
	ctx, exporter, _ := traced(t)
	ctx, op := Start(ctx, "courier.agent.send")
	const canary = "secret-token-and-prompt"
	var logs bytes.Buffer
	log := slog.New(LogHandler{Handler: slog.NewJSONHandler(&logs, nil)})
	log.With("binding", "ns/b").InfoContext(ctx, "attempt started")
	op.Finish(errors.New(canary))
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Status.Code != codes.Error {
		t.Fatal("failure missing from span")
	}
	if strings.Contains(spans[0].Status.Description, canary) || len(spans[0].Events) != 0 {
		t.Fatal("raw error was exported")
	}
	for _, a := range spans[0].Attributes {
		if strings.Contains(a.Value.AsString(), canary) {
			t.Fatal("secret in attributes")
		}
	}
	if !strings.Contains(logs.String(), trace.SpanContextFromContext(ctx).TraceID().String()) || !strings.Contains(logs.String(), "span_id") {
		t.Fatal("log missing trace correlation")
	}
}

// Every operation is one observation of courier_operation_duration_seconds and of no other
// operation histogram: agent and store calls used to be counted again in a histogram of their own.
func TestAnOperationIsOneDurationObservation(t *testing.T) {
	ctx, _, reg := traced(t)
	for _, name := range []string{"courier.agent.send", "courier.store.GetSession", "courier.outbox.deliver"} {
		_, op := Start(ctx, name)
		op.Finish(nil)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	observations := 0
	for _, f := range families {
		if strings.HasSuffix(f.GetName(), "operation_duration_seconds") {
			if f.GetName() != "courier_operation_duration_seconds" {
				t.Errorf("a second operation histogram: %s", f.GetName())
			}
			for _, m := range f.Metric {
				observations += int(m.Histogram.GetSampleCount())
			}
		}
	}
	if observations != 3 {
		t.Fatalf("%d observations for 3 operations", observations)
	}
}

func TestRecoveryLinksDoNotReplaceParent(t *testing.T) {
	ctx, exporter, _ := traced(t)
	original, op := Start(ctx, "courier.turn.attempt")
	parent, state := Capture(original)
	op.Finish(nil)
	recovered, recovery := Start(ctx, "courier.turn.attempt")
	Link(recovered, parent, state)
	recovery.Finish(nil)
	spans := exporter.GetSpans()
	if len(spans) != 2 || spans[1].Parent.IsValid() || len(spans[1].Links) != 1 || spans[1].Links[0].SpanContext.TraceID() != spans[0].SpanContext.TraceID() {
		t.Fatalf("recovery links = %+v", spans)
	}
}

func TestHTTPPropagationAndResponseLifecycle(t *testing.T) {
	for _, propagate := range []bool{true, false} {
		t.Run(map[bool]string{true: "backend", false: "slack"}[propagate], func(t *testing.T) {
			ctx, exporter, _ := traced(t)
			ctx, root := Start(ctx, "courier.turn.attempt", attribute.String("courier.binding", "ns/b"))
			expectedID := trace.SpanContextFromContext(ctx).TraceID()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("traceparent"); (got != "") != propagate {
					t.Errorf("traceparent=%q, propagate=%v", got, propagate)
				}
				if r.Header.Get("tracestate") != "" {
					t.Error("stale trace state was propagated")
				}
				if r.Header.Get("baggage") != "" {
					t.Error("baggage was propagated")
				}
				if propagate {
					remote := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
					if trace.SpanContextFromContext(remote).TraceID() != expectedID {
						t.Error("backend got a different trace")
					}
				}
				_, _ = io.WriteString(w, "reply")
			}))
			defer server.Close()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/?token=canary", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("traceparent", "00-99999999999999999999999999999999-8888888888888888-01")
			req.Header.Set("tracestate", "vendor=secret-canary")
			req.Header.Set("baggage", "secret=canary")
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			resp, err := (&http.Client{Transport: Transport(transport, propagate)}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if len(exporter.GetSpans()) != 0 {
				t.Fatal("HTTP span ended before the response body")
			}
			if _, err = io.ReadAll(resp.Body); err != nil {
				t.Fatal(err)
			}
			if err = resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			root.Finish(nil)
			spans := exporter.GetSpans()
			if len(spans) != 2 {
				t.Fatalf("spans=%d", len(spans))
			}
			for _, s := range spans {
				for _, a := range s.Attributes {
					if strings.Contains(a.Value.AsString(), "canary") {
						t.Fatal("URL query entered span")
					}
				}
			}
		})
	}
}
