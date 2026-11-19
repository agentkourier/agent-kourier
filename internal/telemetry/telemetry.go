// Package telemetry records Agent Kourier operations without capturing message contents or credentials.
// Each broker owns its tracer and registry; there is no global provider.
package telemetry

import (
	"context"
	"errors"
	"net"
	"slices"
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

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

type observerKey struct{}

// ConnectionLabel is the label that names the ChatConnection ("namespace/name") on every series that belongs to one: the series
// of the Slack adapter and of the intake, and the durable outbox's backlog gauges (which the stores export). The set of its values is bounded by
// the number of connections a broker serves; ForgetConnection deletes a connection's series when it is no longer served. Series
// that are a Binding's carry the Binding's name instead, and the rest belong to the broker as a whole.
const ConnectionLabel = "connection"

// Observer owns the instruments for one running broker.
type Observer struct {
	tracer     trace.Tracer
	duration   *prometheus.HistogramVec
	counts     map[string]*prometheus.CounterVec
	histograms map[string]*prometheus.HistogramVec
	gauges     map[string]*prometheus.GaugeVec
	// perConnection are the vectors that have the connection label, for ForgetConnection.
	perConnection []connectionVec
}

// connectionVec is a metric vector that can delete the series that match some of its labels: every vector of the Prometheus client is one.
type connectionVec interface {
	DeletePartialMatch(labels prometheus.Labels) int
}

// New registers Agent Kourier's operation and domain instruments in reg.
func New(reg prometheus.Registerer, provider trace.TracerProvider) (*Observer, error) {
	if provider == nil {
		provider = noop.NewTracerProvider()
	}
	o := &Observer{tracer: provider.Tracer("github.com/agentkourier/agent-kourier"),
		counts: map[string]*prometheus.CounterVec{}, histograms: map[string]*prometheus.HistogramVec{}, gauges: map[string]*prometheus.GaugeVec{}}
	o.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agentkourier_operation_duration_seconds", Help: "Duration of finite Agent Kourier operations, including failed attempts.", Buckets: durationBuckets()}, []string{"operation", "outcome"})
	if err := reg.Register(o.duration); err != nil {
		return nil, err
	}
	for _, d := range []struct {
		name, help string
		labels     []string
	}{
		{"agentkourier_intake_total", "Inbound messages and presses by intake disposition.", []string{"source", "disposition", ConnectionLabel}},
		{"agentkourier_thread_unmentioned_total", "Replies in a thread Agent Kourier owns that a mention-only Binding ignored because they did not mention Agent Kourier, by Binding.", []string{"binding"}},
		{"agentkourier_mentions_total", "What the declared names in a turn's reply came to, by Binding and outcome: made (a message that mentions the person was queued), undeclared (an @name no entry declares, which mentions nobody) and capped (a declared person past the Binding's cap, who is not mentioned).", []string{"binding", "outcome"}},
		{"agentkourier_thread_context_total", "Thread contexts a mentioned turn of a mention-only Binding asked for, by Binding and outcome (read, empty, cut, failed, unsupported).", []string{"binding", "outcome"}},
		{"agentkourier_trigger_matched_total", "Bot messages a botMessage trigger matched, by Binding.", []string{"binding"}},
		{"agentkourier_trigger_unmatched_total", "Bot messages the platform passed on that matched none of the Binding's triggers, by Binding.", []string{"binding"}},
		{"agentkourier_trigger_decisions_total", "What a matched bot message did to its key's thread, by Binding and decision (started, repeat, closed, close_unknown, dedup, empty_key, not_started, limited).", []string{"binding", "decision"}},
		{"agentkourier_trigger_limited_total", "Trigger starts a limit stopped, so no investigation began, by Binding, trigger index and the limit (rate, concurrent, daily).", []string{"binding", "trigger", "limit"}},
		{"agentkourier_trigger_refused_total", "Trigger starts whose first prompt was refused, so no turn started, by Binding, trigger index, the value that failed its check (ref) and why (reason).", []string{"binding", "trigger", "ref", "reason"}},
		{"agentkourier_investigation_feedback_total", "Votes cast on the final message of an investigation or an answer, by Binding, agent, alert name (none for a thread no alert started, unknown for an alert with no name, other past 100 names for a Binding) and verdict (useful, not_useful). A vote that changes another is one more vote, under its new verdict.", []string{"binding", "agent", "alertname", "verdict"}},
		{"agentkourier_stream_reconnects_total", "Stream reconnect attempts by outcome.", []string{"outcome"}},
		{"agentkourier_session_recovery_total", "Session recovery and reconciliation outcomes.", []string{"outcome"}},
		{"agentkourier_render_final_total", "Final output delivery outcomes.", []string{"outcome"}},
		{"agentkourier_chat_api_calls_total", "Chat API operations, including logical errors over HTTP 200.", []string{"operation", "outcome", ConnectionLabel}},
		{"agentkourier_slack_events_shed_total", "Slack events left unacknowledged, so that Slack sends them again, because the adapter already held as many as it may: channel_backlog (one channel's lane was full) or adapter_backlog (all lanes were).", []string{"reason", ConnectionLabel}},
		{"agentkourier_slack_http_requests_total", "Requests Slack sent to a connection in HTTP mode, by what they were answered: acknowledged, challenge, bad_signature, stale, malformed, not_running, shed, withheld or timeout.", []string{"outcome", ConnectionLabel}},
		{"agentkourier_teams_requests_total", "Requests Microsoft Teams sent to a teams connection, by what they were answered: accepted, duplicate, invoke, bad_token, keys_unavailable, malformed, wrong_tenant, not_running or shed.", []string{"outcome", ConnectionLabel}},
		{"agentkourier_teams_tokens_refused_total", "Requests to a teams connection whose Bot Framework token was refused (answered 401), by the check it failed.", []string{"reason", ConnectionLabel}},
		{"agentkourier_teams_activities_shed_total", "Activities Microsoft Teams sent to a teams connection that were answered 503, which Teams sends again once over https: channel_backlog or adapter_backlog (the lanes were full), not_running (the connection was not taking them) or socket_busy (in socket mode, its sockets were handling as many as they may).", []string{"reason", ConnectionLabel}},
		{"agentkourier_teams_activities_lost_total", "Messages of a teams connection that were never handed on after Microsoft Teams was answered 200 for their activity or Microsoft Graph 202 for their notification, which neither sends again: stopped (dropped when the drain at the stop ran out) or not_taken (the broker did not take the message in its tries).", []string{"reason", ConnectionLabel}},
		{"agentkourier_teams_socket_envelopes_total", "Envelopes Microsoft Teams sent over a teams connection's sockets in socket mode, by type (activity, invoke, other) and outcome: what the activity was answered with (accepted, duplicate, invoke, malformed, wrong_tenant, not_running, shed), busy (answered 503: the connection's sockets were handling as many envelopes as they may) or unanswered (the reply frame could not be sent).", []string{"type", "outcome", ConnectionLabel}},
		{"agentkourier_teams_socket_reconnects_total", "Connects of a teams connection's socket in socket mode that follow another, by region and the reason for it: expiry (a replacement before the socket's token expired), a SocketClosing code (socket_mode_disabled, replaced, token_reused, no_identity, closing_other), closed, server_timeout, ready_timeout, rate_limited, refused, unavailable or failed.", []string{"region", "reason", ConnectionLabel}},
		{"agentkourier_teams_graph_notifications_total", "What Microsoft Graph sent to the notificationURL of a teams connection, by kind (validation, change, lifecycle) and outcome: answered (a validation handshake), taken, not_taken, duplicate, own, ignored, edit, not_named, gone, fetch_failed or malformed (a change), reauthorization_required, subscription_removed, missed or other (a lifecycle event), and, of either, client_state (its clientState was not the connection's), wrong_tenant, unknown_subscription, shed or not_running.", []string{"kind", "outcome", ConnectionLabel}},
		{"agentkourier_teams_graph_polls_total", "Polls of a bound channel's messages through Microsoft Graph by a teams connection with no notificationURL, by outcome: started (the run's first read of the channel, which hands on nothing), polled, reset (Graph refused the delta link: read anew from now), throttled (429), failed (may pass) or refused (401, 403, 404 or 400: the channel is failing).", []string{"outcome", ConnectionLabel}},
		{"agentkourier_teams_graph_read_messages_total", "Messages that a poll or a catch-up read of Microsoft Graph found in a bound channel of a teams connection, by outcome: taken, not_taken, duplicate, own, ignored, not_named, old (made before the read's bound) or shed (its channel's lane had no room).", []string{"outcome", ConnectionLabel}},
		{"agentkourier_teams_graph_catchups_total", "Catch-up reads of a bound channel's recent messages through Microsoft Graph by a teams connection, by trigger (start, socket_gap, missed) and outcome (read, failed, not_running).", []string{"trigger", "outcome", ConnectionLabel}},
		{"agentkourier_teams_graph_throttled_total", "Reads of Microsoft Graph by the polls of a teams connection that Graph answered 429, each of which paused every read of the connection for its Retry-After.", []string{ConnectionLabel}},
		{"agentkourier_slack_http_retries_total", "Signed requests to a connection in HTTP mode that Slack marked as a retry (X-Slack-Retry-Num): a request it sent again because an earlier one was not answered 200 in time.", []string{ConnectionLabel}},
	} {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: d.name, Help: d.help}, d.labels)
		if err := reg.Register(c); err != nil {
			return nil, err
		}
		o.counts[d.name] = c
		if slices.Contains(d.labels, ConnectionLabel) {
			o.perConnection = append(o.perConnection, c)
		}
	}
	for _, d := range []struct {
		name, help string
		labels     []string
	}{
		{"http_client_request_duration_seconds", "HTTP client duration through response EOF or close.", []string{"http_request_method", "server_address", "http_response_status_code", "error_type"}},
		{"agentkourier_slack_ack_duration_seconds", "Envelope receipt to acknowledgement attempt completion.", []string{"outcome", ConnectionLabel}},
		{"agentkourier_turn_queue_wait_seconds", "Enqueue to the first execution attempt in this process.", []string{"binding"}},
		{"agentkourier_agent_first_text_duration_seconds", "Execution attempt to its first nonempty text event; recovered snapshots excluded.", []string{"operation"}},
		{"agentkourier_outbox_delivery_duration_seconds", "Durable row creation to successful delivery or adoption.", []string{"role", "mode"}},
		{"agentkourier_interaction_wait_seconds", "Interaction creation to its first durable resolution.", []string{"kind", "outcome"}},
		{"agentkourier_teams_socket_gap_seconds", "Intervals in which no socket of a teams connection in socket mode was ready, from the start of its run or the end of its last ready socket to the next ready one, or to the end of the run. Teams delivers nothing in them, and does not send it later.", []string{ConnectionLabel}},
	} {
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: d.name, Help: d.help, Buckets: durationBuckets()}, d.labels)
		if err := reg.Register(h); err != nil {
			return nil, err
		}
		o.histograms[d.name] = h
		if slices.Contains(d.labels, ConnectionLabel) {
			o.perConnection = append(o.perConnection, h)
		}
	}
	for _, d := range []struct {
		name, help string
		labels     []string
	}{
		{"agentkourier_sessions_active", "Conversation runners currently active in this process.", nil},
		{"agentkourier_sessions_paused", "Conversation runners currently awaiting input in this process.", nil},
		{"agentkourier_teams_socket_ready", "Whether a teams connection in socket mode has a ready socket in the region (1) or not (0).", []string{"region", ConnectionLabel}},
		{"agentkourier_teams_graph_channels", "Bound channels of a teams connection, by the state of their Microsoft Graph intake: subscribed (a notificationURL) or polling (none), waiting_group (the Microsoft Entra group of the channel's team is not known yet) or failing (the last call to make or renew its subscription failed, or the app's subscription to the channel for another URL holds it; or Graph refused its poll for good).", []string{"state", ConnectionLabel}},
	} {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: d.name, Help: d.help}, d.labels)
		if err := reg.Register(g); err != nil {
			return nil, err
		}
		o.gauges[d.name] = g
		if slices.Contains(d.labels, ConnectionLabel) {
			o.perConnection = append(o.perConnection, g)
		}
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
	tracer := noop.NewTracerProvider().Tracer("agent-kourier")
	if o != nil && (!strings.HasPrefix(name, "agentkourier.store.") || trace.SpanContextFromContext(ctx).IsValid()) {
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
		op.span.SetAttributes(attribute.String("agentkourier.outcome", outcome))
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
	case errors.Is(err, dialect.ErrCardTooLarge):
		return "card_too_large"
	case errors.Is(err, dialect.ErrResponseTooLarge):
		return "response_too_large"
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

// ForgetConnection deletes every series that carries the connection label with the value connection ("namespace/name"), so that a
// connection that is no longer served leaves none behind. It is for the connection's removal: its counters start again from zero if it
// is served again. A call that records for the connection afterwards makes a series again, so call it once nothing of the connection
// runs. It does nothing without an Observer in ctx, or for a connection with no series.
func ForgetConnection(ctx context.Context, connection string) {
	if o := observer(ctx); o != nil {
		for _, v := range o.perConnection {
			v.DeletePartialMatch(prometheus.Labels{ConnectionLabel: connection})
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

// SetGauge sets the series of the gauge name under labels to v.
func SetGauge(ctx context.Context, name string, v float64, labels ...string) {
	if o := observer(ctx); o != nil {
		if g := o.gauges[name]; g != nil {
			g.WithLabelValues(labels...).Set(v)
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
