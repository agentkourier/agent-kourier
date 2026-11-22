package telemetry

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TurnOutcome is how a turn ended: the label of agentkourier_ax_turns_total.
type TurnOutcome string

// The turn outcomes, a closed set.
const (
	TurnCompleted TurnOutcome = "completed"
	// TurnInputRequired is a turn that paused on an ask_user question.
	TurnInputRequired TurnOutcome = "input_required"
	TurnFailed        TurnOutcome = "failed"
	TurnCanceled      TurnOutcome = "canceled"
	// TurnInterrupted is a turn that a restart or a takeover cut off, which the next leader failed.
	TurnInterrupted TurnOutcome = "interrupted"
)

// Operation names a call to Amazon Bedrock: the operation label of agentkourier_ax_operation_duration_seconds and the
// last part of its span's name, agentkourier.ax.model.<operation>.
type Operation string

// The operations, a closed set.
const (
	OpConverse       Operation = "converse"
	OpConverseStream Operation = "converse_stream"
)

// AuthReason is why the caller check refused a request: the label of agentkourier_ax_auth_refused_total. It is one of
// package auth's reasons.
type AuthReason string

// The reasons, a closed set.
const (
	AuthMissing     AuthReason = "missing"
	AuthInvalid     AuthReason = "invalid"
	AuthUnavailable AuthReason = "unavailable"
)

// DeleteReason is why the agent deleted a context's ADK session: the label of agentkourier_ax_sessions_deleted_total.
type DeleteReason string

// The reasons, a closed set.
const (
	// DeleteIdle is a session deleted, with its context's tasks, after SESSION_IDLE_TTL without activity.
	DeleteIdle DeleteReason = "idle"
)

// ForwardOutcome is what became of a request a standby forwarded: the label of
// agentkourier_ax_forwarded_requests_total.
type ForwardOutcome string

// The outcomes, a closed set.
const (
	// Forwarded is a request whose answer came back from the leader, whatever its status.
	Forwarded ForwardOutcome = "forwarded"
	// ForwardFailed is a request the leader could not be reached for (answered 503).
	ForwardFailed ForwardOutcome = "failed"
	// ForwardCancelled is a request whose caller went away before the leader answered.
	ForwardCancelled ForwardOutcome = "cancelled"
	// ForwardNoLeader is a request that came while no other replica held a live lease (answered 503).
	ForwardNoLeader ForwardOutcome = "no_leader"
	// ForwardLoop is a forwarded request that reached a replica that does not serve (answered 503): it is never
	// forwarded twice.
	ForwardLoop ForwardOutcome = "loop"
)

// The span attributes the agent sets. Their values are IDs and outcomes, never content.
const (
	AttrTaskID    = "agentkourier.task.id"
	AttrContextID = "agentkourier.context.id"
	AttrOutcome   = "agentkourier.outcome"
)

// Telemetry is the agent's metrics, in a registry of its own, and its tracer. A nil *Telemetry records nothing, so a
// part built without it (in a test) needs no check.
type Telemetry struct {
	reg    *prometheus.Registry
	tracer trace.Tracer

	turns       *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	authRefused *prometheus.CounterVec
	deleted     *prometheus.CounterVec
	replays     prometheus.Counter
	interrupted prometheus.Counter
	leader      prometheus.Gauge
	takeovers   prometheus.Counter
	forwarded   *prometheus.CounterVec
}

// New registers the agent's metrics, with the Go and process collectors, in a registry of their own, and takes its
// tracer from provider (nil records no spans).
func New(provider trace.TracerProvider) (*Telemetry, error) {
	if provider == nil {
		provider = noop.NewTracerProvider()
	}
	t := &Telemetry{reg: prometheus.NewRegistry(), tracer: provider.Tracer("github.com/agentkourier/agent-kourier/addons/ax")}
	t.turns = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_ax_turns_total",
		Help: "Turns the agent ran, by how they ended: completed, input_required (paused on an ask_user question), failed, canceled or interrupted (failed by the leader after a restart or a takeover)."},
		[]string{"outcome"})
	t.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agentkourier_ax_operation_duration_seconds",
		Help:    "Duration of the agent's calls to Amazon Bedrock (converse, converse_stream (opening the stream)), by outcome: success, cancelled, timeout, the HTTP status Bedrock answered, or error.",
		Buckets: durationBuckets()}, []string{"operation", "outcome"})
	t.authRefused = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_ax_auth_refused_total",
		Help: "Requests to the A2A endpoint or the agent card that the caller check refused, by reason: missing (no credential, 401), invalid (not accepted, 401) or unavailable (the OIDC issuer could not be reached, 503)."},
		[]string{"reason"})
	t.deleted = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_ax_sessions_deleted_total",
		Help: "ADK sessions the agent deleted, each with its context's tasks, by reason: idle (no activity for SESSION_IDLE_TTL)."},
		[]string{"reason"})
	t.replays = prometheus.NewCounter(prometheus.CounterOpts{Name: "agentkourier_ax_message_replays_total",
		Help: "Messages whose ID the store had recorded, which joined that message's task instead of starting another turn."})
	t.interrupted = prometheus.NewCounter(prometheus.CounterOpts{Name: "agentkourier_ax_tasks_interrupted_total",
		Help: "Tasks left submitted or working by a restart or a takeover, which the leader failed when it began to lead."})
	t.leader = prometheus.NewGauge(prometheus.GaugeOpts{Name: "agentkourier_ax_leader",
		Help: "1 while this replica runs turns (it leads, or it is the only replica: SQLite), 0 while it is a standby."})
	t.takeovers = prometheus.NewCounter(prometheus.CounterOpts{Name: "agentkourier_ax_leader_takeovers_total",
		Help: "Times this replica took the lease, each with a new epoch."})
	t.forwarded = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentkourier_ax_forwarded_requests_total",
		Help: "Requests to the A2A endpoint or the agent card that this replica, a standby, forwarded or could not, by outcome: forwarded, failed, cancelled, no_leader or loop."},
		[]string{"outcome"})
	for _, c := range []prometheus.Collector{t.turns, t.duration, t.authRefused, t.deleted, t.replays, t.interrupted, t.leader,
		t.takeovers, t.forwarded, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})} {
		if err := t.reg.Register(c); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// durationBuckets are the broker's.
func durationBuckets() []float64 {
	return []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 900, 3600}
}

// Handler serves the metrics in Prometheus's text format.
func (t *Telemetry) Handler() http.Handler {
	if t == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(t.reg, promhttp.HandlerOpts{})
}

// Registry is the registry the metrics are in, for a test to gather.
func (t *Telemetry) Registry() *prometheus.Registry { return t.reg }

// AuthRefused counts a request the caller check refused.
func (t *Telemetry) AuthRefused(reason AuthReason) {
	if t != nil {
		t.authRefused.WithLabelValues(string(reason)).Inc()
	}
}

// SessionsDeleted counts n sessions deleted for reason.
func (t *Telemetry) SessionsDeleted(reason DeleteReason, n int) {
	if t != nil && n > 0 {
		t.deleted.WithLabelValues(string(reason)).Add(float64(n))
	}
}

// MessageReplay counts a message that joined its recorded task.
func (t *Telemetry) MessageReplay() {
	if t != nil {
		t.replays.Inc()
	}
}

// TasksInterrupted counts n tasks the leader failed because a restart or a takeover cut their turns off. Each turn
// counts as interrupted too.
func (t *Telemetry) TasksInterrupted(n int) {
	if t != nil && n > 0 {
		t.interrupted.Add(float64(n))
		t.turns.WithLabelValues(string(TurnInterrupted)).Add(float64(n))
	}
}

// SetLeader sets agentkourier_ax_leader.
func (t *Telemetry) SetLeader(leading bool) {
	if t != nil {
		v := 0.0
		if leading {
			v = 1
		}
		t.leader.Set(v)
	}
}

// Takeover counts a take of the lease.
func (t *Telemetry) Takeover() {
	if t != nil {
		t.takeovers.Inc()
	}
}

// Forward counts what became of a request a standby received for the leader.
func (t *Telemetry) Forward(outcome ForwardOutcome) {
	if t != nil {
		t.forwarded.WithLabelValues(string(outcome)).Inc()
	}
}

func (t *Telemetry) tracerOrNoop() trace.Tracer {
	if t == nil {
		return noop.NewTracerProvider().Tracer("")
	}
	return t.tracer
}

// Turn is one turn's span, agentkourier.ax.turn.
type Turn struct {
	t    *Telemetry
	span trace.Span
}

// StartTurn starts the span of a turn of taskID in contextID. Its parent is the span in ctx: the caller's, which Extract
// took from the request's traceparent.
func (t *Telemetry) StartTurn(ctx context.Context, taskID, contextID string) (context.Context, *Turn) {
	ctx, span := t.tracerOrNoop().Start(ctx, "agentkourier.ax.turn", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String(AttrTaskID, taskID), attribute.String(AttrContextID, contextID)))
	return ctx, &Turn{t: t, span: span}
}

// End ends the turn's span and counts the turn under outcome.
func (tr *Turn) End(outcome TurnOutcome) {
	tr.span.SetAttributes(attribute.String(AttrOutcome, string(outcome)))
	if outcome == TurnFailed {
		tr.span.SetStatus(codes.Error, string(outcome))
	}
	tr.span.End()
	if tr.t != nil {
		tr.t.turns.WithLabelValues(string(outcome)).Inc()
	}
}

// Call is one call to Bedrock: its span, agentkourier.ax.model.<operation>, and its duration.
type Call struct {
	t       *Telemetry
	op      Operation
	span    trace.Span
	started time.Time
}

// StartCall starts a call's span, a child of the span in ctx.
func (t *Telemetry) StartCall(ctx context.Context, op Operation) (context.Context, *Call) {
	ctx, span := t.tracerOrNoop().Start(ctx, "agentkourier.ax.model."+string(op), trace.WithSpanKind(trace.SpanKindClient))
	return ctx, &Call{t: t, op: op, span: span, started: time.Now()}
}

// End ends the call with outcome, "success" or a class of its error from a bounded set (the caller's to choose: never
// an error's text).
func (c *Call) End(outcome string) {
	c.span.SetAttributes(attribute.String(AttrOutcome, outcome))
	if outcome != "success" && outcome != "cancelled" {
		c.span.SetStatus(codes.Error, outcome)
	}
	c.span.End()
	if c.t != nil {
		c.t.duration.WithLabelValues(string(c.op), outcome).Observe(time.Since(c.started).Seconds())
	}
}

// Extract returns next with the W3C trace context of each request (traceparent, tracestate; never baggage) as the
// parent of the spans the request starts: Agent Kourier sends its own, so a turn's span is a child of the broker's.
func Extract(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
