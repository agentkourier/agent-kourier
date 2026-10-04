// Package telemetry records courier operations without capturing message contents or credentials.
// Each broker owns its tracer and registry; there is no global provider.
package telemetry

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

type observerKey struct{}

// Observer owns the instruments for one running broker.
type Observer struct {
	tracer     trace.Tracer
	duration   *prometheus.HistogramVec
	counts     map[string]*prometheus.CounterVec
	histograms map[string]*prometheus.HistogramVec
	gauges     map[string]*prometheus.GaugeVec
}

// New registers courier's operation and domain instruments in reg.
func New(reg prometheus.Registerer, provider trace.TracerProvider) (*Observer, error) {
	if provider == nil {
		provider = noop.NewTracerProvider()
	}
	o := &Observer{tracer: provider.Tracer("github.com/agentkourier/agent-kourier"),
		counts: map[string]*prometheus.CounterVec{}, histograms: map[string]*prometheus.HistogramVec{}, gauges: map[string]*prometheus.GaugeVec{}}
	o.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "courier_operation_duration_seconds", Help: "Duration of finite courier operations, including failed attempts.", Buckets: durationBuckets()}, []string{"operation", "outcome"})
	if err := reg.Register(o.duration); err != nil {
		return nil, err
	}
	for _, d := range []struct {
		name, help string
		labels     []string
	}{
		{"courier_intake_total", "Inbound messages and presses by intake disposition.", []string{"source", "disposition"}},
		{"courier_trigger_matched_total", "Bot messages a botMessage trigger matched, by Binding.", []string{"binding"}},
		{"courier_trigger_unmatched_total", "Bot messages the platform passed on that matched none of the Binding's triggers, by Binding.", []string{"binding"}},
		{"courier_trigger_decisions_total", "What a matched bot message did to its key's thread, by Binding and decision (started, repeat, closed, close_unknown, dedup, empty_key, not_started, limited).", []string{"binding", "decision"}},
		{"courier_trigger_limited_total", "Trigger starts a limit stopped, so no investigation began, by Binding, trigger index and the limit (rate, concurrent, daily).", []string{"binding", "trigger", "limit"}},
		{"courier_trigger_refused_total", "Trigger starts whose first prompt was refused, so no turn started, by Binding, trigger index, the value that failed its check (ref) and why (reason).", []string{"binding", "trigger", "ref", "reason"}},
		{"courier_stream_reconnects_total", "Stream reconnect attempts by outcome.", []string{"outcome"}},
		{"courier_session_recovery_total", "Session recovery and reconciliation outcomes.", []string{"outcome"}},
		{"courier_render_final_total", "Final output delivery outcomes.", []string{"outcome"}},
		{"courier_chat_api_calls_total", "Chat API operations, including logical errors over HTTP 200.", []string{"operation", "outcome"}},
	} {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: d.name, Help: d.help}, d.labels)
		if err := reg.Register(c); err != nil {
			return nil, err
		}
		o.counts[d.name] = c
	}
	for _, d := range []struct {
		name, help string
		labels     []string
	}{
		{"http_client_request_duration_seconds", "HTTP client duration through response EOF or close.", []string{"http_request_method", "server_address", "http_response_status_code", "error_type"}},
		{"courier_slack_ack_duration_seconds", "Envelope receipt to acknowledgement attempt completion.", []string{"outcome"}},
		{"courier_turn_queue_wait_seconds", "Enqueue to the first execution attempt in this process.", []string{"binding"}},
		{"courier_agent_first_text_duration_seconds", "Execution attempt to its first nonempty text event; recovered snapshots excluded.", []string{"operation"}},
		{"courier_outbox_delivery_duration_seconds", "Durable row creation to successful delivery or adoption.", []string{"role", "mode"}},
		{"courier_interaction_wait_seconds", "Interaction creation to its first durable resolution.", []string{"kind", "outcome"}},
	} {
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: d.name, Help: d.help, Buckets: durationBuckets()}, d.labels)
		if err := reg.Register(h); err != nil {
			return nil, err
		}
		o.histograms[d.name] = h
	}
	for _, d := range []struct {
		name, help string
		labels     []string
	}{
		{"courier_sessions_active", "Conversation runners currently active in this process.", nil},
		{"courier_sessions_paused", "Conversation runners currently awaiting input in this process.", nil},
	} {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: d.name, Help: d.help}, d.labels)
		if err := reg.Register(g); err != nil {
			return nil, err
		}
		o.gauges[d.name] = g
	}
	for _, c := range []prometheus.Collector{collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return o, nil
}

func durationBuckets() []float64 {
	return []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 900, 3600}
}

// WithObserver attaches the broker's instruments to its lifetime context.
func WithObserver(ctx context.Context, o *Observer) context.Context {
	return context.WithValue(ctx, observerKey{}, o)
}

// Enabled reports whether this context carries broker instruments.
func Enabled(ctx context.Context) bool { return observer(ctx) != nil }

func observer(ctx context.Context) *Observer { o, _ := ctx.Value(observerKey{}).(*Observer); return o }

// Operation is one finite span and its duration observation.
type Operation struct {
	span                   trace.Span
	observer               *Observer
	name                   string
	httpMethod, httpServer string
	httpStatus             int
	started                time.Time
	once                   sync.Once
}

// Start starts a finite internal operation. name must be a fixed, content-free operation name.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, *Operation) {
	return StartKind(ctx, name, trace.SpanKindInternal, attrs...)
}

// StartKind starts a span with the specified kind. Store operations require a valid
// parent span context, so background polling records metrics without creating root traces.
func StartKind(ctx context.Context, name string, kind trace.SpanKind, attrs ...attribute.KeyValue) (context.Context, *Operation) {
	o := observer(ctx)
	tracer := noop.NewTracerProvider().Tracer("courier")
	if o != nil && (!strings.HasPrefix(name, "courier.store.") || trace.SpanContextFromContext(ctx).IsValid()) {
		tracer = o.tracer
	}
	ctx, span := tracer.Start(ctx, name, trace.WithSpanKind(kind), trace.WithAttributes(attrs...))
	return ctx, &Operation{span: span, observer: o, name: name, started: time.Now()}
}

// Finish ends the operation once. Raw error strings never enter telemetry.
func (op *Operation) Finish(err error) { op.FinishOutcome(err, ErrorClass(err)) }

// FinishOutcome records a bounded domain outcome and optionally marks a failure.
func (op *Operation) FinishOutcome(err error, outcome string) {
	op.once.Do(func() {
		if err != nil {
			op.span.SetAttributes(attribute.String("error.type", outcome))
			if outcome != "cancelled" {
				op.span.SetStatus(codes.Error, outcome)
			}
		}
		op.span.SetAttributes(attribute.String("courier.outcome", outcome))
		if op.observer != nil {
			op.observer.duration.WithLabelValues(op.name, outcome).Observe(time.Since(op.started).Seconds())
			if op.httpMethod != "" {
				errorType := ""
				if err != nil {
					errorType = ErrorClass(err)
				}
				status := ""
				if op.httpStatus != 0 {
					status = strconv.Itoa(op.httpStatus)
				}
				op.observer.histograms["http_client_request_duration_seconds"].WithLabelValues(op.httpMethod, op.httpServer, status, errorType).Observe(time.Since(op.started).Seconds())
			}
		}
		op.span.End()
	})
}

// ErrorClass maps errors to a bounded class without serializing their text.
func ErrorClass(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var status httpStatusError
	if errors.As(err, &status) {
		return strconv.Itoa(int(status))
	}
	var n net.Error
	if errors.As(err, &n) && n.Timeout() {
		return "timeout"
	}
	return "error"
}

// Count records a domain event. name and labels must come from the closed sets at call sites.
func Count(ctx context.Context, name string, labels ...string) {
	if o := observer(ctx); o != nil {
		if c := o.counts[name]; c != nil {
			c.WithLabelValues(labels...).Inc()
		}
	}
}

// Observe records a nonnegative domain duration.
func Observe(ctx context.Context, name string, d time.Duration, labels ...string) {
	if o := observer(ctx); o != nil {
		if h := o.histograms[name]; h != nil {
			h.WithLabelValues(labels...).Observe(max(d.Seconds(), 0))
		}
	}
}

// AddGauge updates an in-process lifecycle gauge.
func AddGauge(ctx context.Context, name string, delta float64) {
	if o := observer(ctx); o != nil {
		if g := o.gauges[name]; g != nil {
			g.WithLabelValues().Add(delta)
		}
	}
}

// Capture returns validated W3C trace headers, never baggage.
func Capture(ctx context.Context) (parent, state string) {
	c := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, c)
	return c.Get("traceparent"), c.Get("tracestate")
}

// Restore restores persisted ancestry while preserving the current lifetime and observer.
// Empty or malformed headers leave the current span unchanged.
func Restore(ctx context.Context, parent, state string) context.Context {
	if parent == "" {
		return ctx
	}
	extracted := propagation.TraceContext{}.Extract(trace.ContextWithSpanContext(ctx, trace.SpanContext{}), propagation.MapCarrier{"traceparent": parent, "tracestate": state})
	sc := trace.SpanContextFromContext(extracted)
	if !sc.IsValid() {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}

// Link adds a causal link to persisted work without changing the current parent.
func Link(ctx context.Context, parent, state string) {
	extracted := propagation.TraceContext{}.Extract(trace.ContextWithSpanContext(ctx, trace.SpanContext{}), propagation.MapCarrier{"traceparent": parent, "tracestate": state})
	sc := trace.SpanContextFromContext(extracted)
	if sc.IsValid() {
		trace.SpanFromContext(ctx).AddLink(trace.Link{SpanContext: sc})
	}
}
