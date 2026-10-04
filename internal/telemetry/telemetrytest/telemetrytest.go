// Package telemetrytest is the test harness for code that records courier telemetry: an
// Observer over an in-memory span exporter and a Prometheus registry, and a way to ask whether
// a string reached either.
package telemetrytest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// Harness records what an Observer exports. Spans are exported as they end.
type Harness struct {
	// Ctx carries the Observer: pass it, or a context derived from it, to the code under test.
	Ctx      context.Context
	Exporter *tracetest.InMemoryExporter
	Registry *prometheus.Registry
}

// New returns a Harness whose provider and Observer are shut down when the test ends.
func New(t testing.TB) *Harness {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	reg := prometheus.NewRegistry()
	obs, err := telemetry.New(reg, provider)
	if err != nil {
		t.Fatal(err)
	}
	return &Harness{Ctx: telemetry.WithObserver(context.Background(), obs), Exporter: exporter, Registry: reg}
}

// Span returns the ended span of the given name, and fails the test unless there is exactly one.
func (h *Harness) Span(t testing.TB, name string) tracetest.SpanStub {
	t.Helper()
	var found []tracetest.SpanStub
	for _, s := range h.Exporter.GetSpans() {
		if s.Name == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d spans named %s, want 1 (all: %s)", len(found), name, h.names())
	}
	return found[0]
}

func (h *Harness) names() string {
	var names []string
	for _, s := range h.Exporter.GetSpans() {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// Root starts a span in the Observer's context, ends it, and returns its W3C carrier with its span
// context: what a durable row would hold after intake.
func (h *Harness) Root(name string) (parent, state string, sc trace.SpanContext) {
	ctx, op := telemetry.Start(h.Ctx, name)
	parent, state = telemetry.Capture(ctx)
	sc = trace.SpanContextFromContext(ctx)
	op.Finish(nil)
	return parent, state, sc
}

// Counter returns the value of the counter series of the given name whose labels are exactly
// labels, or 0 if there is none.
func (h *Harness) Counter(t testing.TB, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := h.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	series:
		for _, m := range f.Metric {
			if len(m.Label) != len(labels) {
				continue
			}
			for _, l := range m.Label {
				if labels[l.GetName()] != l.GetValue() {
					continue series
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// Exported returns everything an exporter or a scrape would show a reader: each ended span's name,
// attribute keys and values, status, events and links, and each metric's name, label names and
// label values.
func (h *Harness) Exported(t testing.TB) string {
	t.Helper()
	var b strings.Builder
	for _, s := range h.Exporter.GetSpans() {
		fmt.Fprintf(&b, "span %s %s %s\n", s.Name, s.Status.Code, s.Status.Description)
		for _, a := range s.Attributes {
			fmt.Fprintf(&b, "  attr %s=%s\n", a.Key, a.Value.Emit())
		}
		for _, e := range s.Events {
			fmt.Fprintf(&b, "  event %s\n", e.Name)
			for _, a := range e.Attributes {
				fmt.Fprintf(&b, "    attr %s=%s\n", a.Key, a.Value.Emit())
			}
		}
		for _, l := range s.Links {
			fmt.Fprintf(&b, "  link %s\n", l.SpanContext.TraceID())
			for _, a := range l.Attributes {
				fmt.Fprintf(&b, "    attr %s=%s\n", a.Key, a.Value.Emit())
			}
		}
	}
	families, err := h.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		fmt.Fprintf(&b, "metric %s\n", f.GetName())
		for _, m := range f.Metric {
			for _, l := range m.Label {
				fmt.Fprintf(&b, "  label %s=%s\n", l.GetName(), l.GetValue())
			}
		}
	}
	return b.String()
}

// RequireNone fails the test if any of the strings reached a span or a metric. The check is
// on the whole exported text, so an attribute, a status description, an event or a label all count.
func (h *Harness) RequireNone(t testing.TB, secrets ...string) {
	t.Helper()
	exported := h.Exported(t)
	if exported == "" || !strings.Contains(exported, "span ") {
		t.Fatalf("nothing was exported, so the check proves nothing:\n%s", exported)
	}
	for _, s := range secrets {
		if s == "" {
			t.Fatal("RequireNone was given an empty string")
		}
		if strings.Contains(exported, s) {
			t.Fatalf("%q reached telemetry:\n%s", s, exported)
		}
	}
}
