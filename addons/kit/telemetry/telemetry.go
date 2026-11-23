package telemetry

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
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

// TurnOutcome is how a turn ended: the label of agentkourier_<add-on>_turns_total. An add-on may add outcomes of its
// own (and names in Spec.ErrorTurns those that fail the turn's span).
type TurnOutcome string

// The turn outcomes every add-on has.
const (
	TurnCompleted TurnOutcome = "completed"
	// TurnInputRequired is a turn that paused for a person: an approval, or an answer to a question.
	TurnInputRequired TurnOutcome = "input_required"
	TurnFailed        TurnOutcome = "failed"
	TurnCanceled      TurnOutcome = "canceled"
	// TurnInterrupted is a turn that a restart or a takeover cut off, which the next leader failed.
	TurnInterrupted TurnOutcome = "interrupted"
)

// Operation names a call to an add-on's backend: the operation label of agentkourier_<add-on>_operation_duration_seconds
// and the last part of its span's name, agentkourier.<add-on>.<Spec.Calls>.<operation>. Each add-on has its own closed
// set.
type Operation string

// ForwardOutcome is what became of a request a standby forwarded: the label of
// agentkourier_<add-on>_forwarded_requests_total.
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

// The span attributes an add-on sets. Their values are IDs and outcomes, never content.
const (
	AttrTaskID    = "agentkourier.task.id"
	AttrContextID = "agentkourier.context.id"
	AttrOutcome   = "agentkourier.outcome"
)

// Spec is what names an add-on's telemetry: its metrics, its spans and its tracer, and the help of the metrics whose
// words are the add-on's own.
type Spec struct {
	// Name is the add-on's, such as cma: its metrics are agentkourier_<Name>_*, its spans agentkourier.<Name>.*, and its
	// tracer github.com/agentkourier/agent-kourier/addons/<Name>. Lower-case letters and digits, a letter first.
	Name string
	// Calls is the part of a call's span name that names the backend: agentkourier.<Name>.<Calls>.<operation>. The same
	// characters as Name.
	Calls string
	// TurnsHelp, OperationHelp and AuthRefusedHelp are the help of agentkourier_<Name>_turns_total,
	// agentkourier_<Name>_operation_duration_seconds and agentkourier_<Name>_auth_refused_total: the outcomes, the
	// operations and the reasons each add-on has. None may be empty.
	TurnsHelp, OperationHelp, AuthRefusedHelp string
	// ErrorTurns are the outcomes, besides TurnFailed, that end a turn's span with the status Error.
	ErrorTurns []TurnOutcome
}

// nameRE is what Spec.Name and Spec.Calls may be: part of a metric name, a span name and a service name alike.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// Telemetry is an add-on's metrics, in a registry of its own, and its tracer. A nil *Telemetry records nothing, so a
// part built without it (in a test) needs no check.
type Telemetry struct {
	reg        *prometheus.Registry
	tracer     trace.Tracer
	spans      string // agentkourier.<Name>.
	calls      string // Spec.Calls
	errorTurns []TurnOutcome

	turns       *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	authRefused *prometheus.CounterVec
	replays     prometheus.Counter
	interrupted prometheus.Counter
	leader      prometheus.Gauge
	takeovers   prometheus.Counter
	forwarded   *prometheus.CounterVec
}

// New registers the metrics every add-on has, named by spec, with the Go and process collectors, in a registry of
// their own, and takes the tracer from provider (nil records no spans). The add-on adds its own metrics with Register.
func New(spec Spec, provider trace.TracerProvider) (*Telemetry, error) {
	if !nameRE.MatchString(spec.Name) || !nameRE.MatchString(spec.Calls) {
		return nil, errors.New("telemetry: the add-on's name and its calls' name must be lower-case letters and digits, a letter first")
	}
	if spec.TurnsHelp == "" || spec.OperationHelp == "" || spec.AuthRefusedHelp == "" {
		return nil, errors.New("telemetry: every help text of the spec must be set")
	}
	if provider == nil {
		provider = noop.NewTracerProvider()
	}
	prefix := "agentkourier_" + spec.Name + "_"
	t := &Telemetry{reg: prometheus.NewRegistry(), tracer: provider.Tracer("github.com/agentkourier/agent-kourier/addons/" + spec.Name),
		spans: "agentkourier." + spec.Name + ".", calls: spec.Calls,
		errorTurns: slices.Clone(spec.ErrorTurns)}
	t.turns = prometheus.NewCounterVec(prometheus.CounterOpts{Name: prefix + "turns_total", Help: spec.TurnsHelp}, []string{"outcome"})
	t.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: prefix + "operation_duration_seconds",
		Help: spec.OperationHelp, Buckets: durationBuckets()}, []string{"operation", "outcome"})
	t.authRefused = prometheus.NewCounterVec(prometheus.CounterOpts{Name: prefix + "auth_refused_total", Help: spec.AuthRefusedHelp},
		[]string{"reason"})
	t.replays = prometheus.NewCounter(prometheus.CounterOpts{Name: prefix + "message_replays_total",
		Help: "Messages whose ID the store had recorded, which joined that message's task instead of starting another turn."})
	t.interrupted = prometheus.NewCounter(prometheus.CounterOpts{Name: prefix + "tasks_interrupted_total",
		Help: "Tasks left submitted or working by a restart or a takeover, which the leader failed when it began to lead."})
	t.leader = prometheus.NewGauge(prometheus.GaugeOpts{Name: prefix + "leader",
		Help: "1 while this replica runs turns (it leads, or it is the only replica: SQLite), 0 while it is a standby."})
	t.takeovers = prometheus.NewCounter(prometheus.CounterOpts{Name: prefix + "leader_takeovers_total",
		Help: "Times this replica took the lease, each with a new epoch."})
	t.forwarded = prometheus.NewCounterVec(prometheus.CounterOpts{Name: prefix + "forwarded_requests_total",
		Help: "Requests to the A2A endpoint or the agent card that this replica, a standby, forwarded or could not, by outcome: forwarded, failed, cancelled, no_leader or loop."},
		[]string{"outcome"})
	for _, c := range []prometheus.Collector{t.turns, t.duration, t.authRefused, t.replays, t.interrupted, t.leader,
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

// Register adds one of the add-on's own metrics to the registry /metrics serves. Its name begins with
// agentkourier_<Spec.Name>_, as the add-on writes it. A nil Telemetry registers nothing.
func (t *Telemetry) Register(c prometheus.Collector) error {
	if t == nil {
		return nil
	}
	return t.reg.Register(c)
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

// AuthRefused counts a request the caller check refused, under reason: one of the check's own, a closed set.
func (t *Telemetry) AuthRefused(reason string) {
	if t != nil {
		t.authRefused.WithLabelValues(reason).Inc()
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

// SetLeader sets agentkourier_<add-on>_leader.
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

// start starts the span agentkourier.<add-on>.<name>, or one that records nothing when t is nil.
func (t *Telemetry) start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if t == nil {
		return noop.NewTracerProvider().Tracer("").Start(ctx, name, opts...)
	}
	return t.tracer.Start(ctx, t.spans+name, opts...)
}

// Turn is one turn's span, agentkourier.<add-on>.turn.
type Turn struct {
	t    *Telemetry
	span trace.Span
}

// StartTurn starts the span of a turn of taskID in contextID. Its parent is the span in ctx: the caller's, which Extract
// took from the request's traceparent.
func (t *Telemetry) StartTurn(ctx context.Context, taskID, contextID string) (context.Context, *Turn) {
	ctx, span := t.start(ctx, "turn", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String(AttrTaskID, taskID), attribute.String(AttrContextID, contextID)))
	return ctx, &Turn{t: t, span: span}
}

// End ends the turn's span and counts the turn under outcome. A failed turn's span, and one whose outcome is in
// Spec.ErrorTurns, ends with the status Error.
func (tr *Turn) End(outcome TurnOutcome) {
	tr.span.SetAttributes(attribute.String(AttrOutcome, string(outcome)))
	if outcome == TurnFailed || (tr.t != nil && slices.Contains(tr.t.errorTurns, outcome)) {
		tr.span.SetStatus(codes.Error, string(outcome))
	}
	tr.span.End()
	if tr.t != nil {
		tr.t.turns.WithLabelValues(string(outcome)).Inc()
	}
}

// Cut ends the span of a turn that the add-on's own stop cut short, with the outcome interrupted and the status Error,
// and does not count it: its task stays as it was, and the leader that fails it at its takeover counts it
// (TasksInterrupted, and turns_total under interrupted).
func (tr *Turn) Cut() {
	tr.span.SetAttributes(attribute.String(AttrOutcome, string(TurnInterrupted)))
	tr.span.SetStatus(codes.Error, string(TurnInterrupted))
	tr.span.End()
}

// Call is one call to the add-on's backend: its span, agentkourier.<add-on>.<Spec.Calls>.<operation>, and its
// duration.
type Call struct {
	t       *Telemetry
	op      Operation
	span    trace.Span
	started time.Time
}

// StartCall starts a call's span, a child of the span in ctx. Observe starts and ends one around a function.
func (t *Telemetry) StartCall(ctx context.Context, op Operation) (context.Context, *Call) {
	name := string(op)
	if t != nil {
		name = t.calls + "." + name
	}
	ctx, span := t.start(ctx, name, trace.WithSpanKind(trace.SpanKindClient))
	return ctx, &Call{t: t, op: op, span: span, started: time.Now()}
}

// End ends the call with outcome, "success" or a class of its error from a bounded set (the caller's to choose: never
// an error's text; Outcome gives one).
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
