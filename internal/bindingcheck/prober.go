package bindingcheck

import (
	"context"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"
)

// Defaults of the Options that are left zero.
const (
	defaultInterval    = 5 * time.Minute
	defaultTick        = 5 * time.Second
	defaultTimeout     = 10 * time.Second
	defaultConcurrency = 4
	// jitterFraction is how far the pause between two passes strays from Interval, either way.
	jitterFraction = 0.1
	// shutdownGrace is how long a probe that the shutdown cut off waits for its checker to return. A checker that
	// honors its context unwinds well inside it (broker shutdown steps take 2 to 7 seconds); one that ignores its
	// context is left behind.
	shutdownGrace = time.Second
)

// Target is a Binding to probe and the chat channel it posts to.
type Target struct {
	// Binding is the Binding's name, the key of its conditions.
	Binding string
	// Channel is the chat channel the Binding is bound to.
	Channel string
}

// AgentChecker probes the agent backend of a Binding. It sets BackendReachable.
type AgentChecker interface {
	// CheckAgent reports whether binding's backend can be used. It must honor ctx, which ends at the probe timeout.
	// An error means the probe could not tell and leaves the condition Unknown; a checker that can classify a
	// failure returns it as an Outcome with a nil error.
	CheckAgent(ctx context.Context, binding string) (Outcome, error)
}

// ChannelChecker probes a chat channel. It sets ChannelJoined for every Binding bound to that channel.
type ChannelChecker interface {
	// CheckChannel reports whether the bot is in channel. It must honor ctx, which ends at the probe timeout. An
	// error means the probe could not tell and leaves the condition Unknown.
	CheckChannel(ctx context.Context, channel string) (Outcome, error)
}

// PanicError is the error OnProbeError gets for a checker that panicked.
type PanicError struct {
	// Value is what the checker passed to panic. It may hold anything the checker had in hand, so redact it.
	Value any
	// Stack is the goroutine's stack at the panic.
	Stack []byte
}

// Error describes the panic. It includes Value, so redact it before logging.
func (e *PanicError) Error() string { return fmt.Sprintf("checker panicked: %v", e.Value) }

// ProbeError is a probe whose checker failed: it returned an error, panicked, or reported an outcome with no usable
// status or reason. The condition is Unknown/ProbeFailed and its Message says nothing of Err.
type ProbeError struct {
	// Condition is the condition the probe was setting.
	Condition ConditionType
	// Targets are the Bindings it was setting it for: one for BackendReachable, every Binding of the channel for
	// ChannelJoined.
	Targets []Target
	// Err is the failure. For a panic it is a *PanicError.
	Err error
}

// Options configure a Prober. Zero fields take their defaults.
type Options struct {
	// Targets lists the Bindings to probe now. It is called on every Tick, so it should be cheap. A Binding that
	// stops appearing in it is pruned from the State.
	Targets func() []Target
	// Revision names the state of the config the probes depend on, and a change of it (from what the last pass saw)
	// runs a pass at the next Tick; a channel or a Secret changes only through it. It runs on the Tick loop under
	// the Timeout and must honor its context. When a read fails it should return a marker that differs from any
	// revision it returns on success and is the same on every call while the failure lasts: the next pass then
	// reports the failure, and the recovery is another change that runs another pass. A call that outlasts the
	// Timeout counts as no change. Nil means the config is never revised.
	Revision func(ctx context.Context) string
	// Agents probes backends. Nil means BackendReachable is not tracked.
	Agents AgentChecker
	// Channels probes channels. Nil means ChannelJoined is not tracked.
	Channels ChannelChecker
	// Interval is the pause between passes, 5 minutes by default. Each pause strays from it by up to 10% either
	// way, so that many brokers do not probe together.
	Interval time.Duration
	// Tick is how often the loop looks at Targets and Revision, 5 seconds by default.
	Tick time.Duration
	// Timeout is how long one probe (and one Revision call) may take, 10 seconds by default. A probe that takes
	// longer is Unknown/ProbeTimeout.
	Timeout time.Duration
	// Concurrency is how many probes run at once, 4 by default.
	Concurrency int
	// OnTransition is called for each change of a condition's status or reason, Ready included (filter on
	// Transition.New.Type), and never for a probe that changed neither. A new Binding's Pending conditions are not
	// transitions. Calls are serialized with each other and with OnProbeError, run on a probe's goroutine and hold
	// up the other probes' results while they run, so the hook must be quick and must not wait on the Prober.
	OnTransition func(Transition)
	// OnProbeError is called for each probe whose checker failed, with the same serialization and cost as
	// OnTransition. Nothing in the error reaches a Message, so the hook is where a checker's failure is seen; it
	// should redact before it logs.
	OnProbeError func(ProbeError)
	// OnPrune is called on Run's goroutine with the names of the Bindings pruned from the State, when there are any.
	// Every OnTransition call for a Binding comes before the OnPrune call that names it, and none follows it.
	OnPrune func(names []string)
}

// Prober probes the Bindings of its Targets and records what the checkers report in a State.
type Prober struct {
	o     Options
	state *State

	// apply is a one-slot semaphore that serializes recording an outcome with the hook calls it causes. It is a
	// channel, not a mutex, so a goroutine waiting for it is durably blocked, which a test under testing/synctest needs.
	apply chan struct{}

	mu      sync.Mutex
	running map[probeKey]bool // checker goroutines started and not yet returned

	known map[string]bool // Bindings seen at the last Tick; owned by Run's goroutine
}

// probeKey identifies a probe: a condition type and an agent's Binding or a channel.
type probeKey struct {
	cond ConditionType
	name string
}

// NewProber returns a Prober with its own empty State.
func NewProber(o Options) *Prober {
	if o.Interval <= 0 {
		o.Interval = defaultInterval
	}
	if o.Tick <= 0 {
		o.Tick = defaultTick
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.Concurrency <= 0 {
		o.Concurrency = defaultConcurrency
	}
	return &Prober{o: o, state: NewState(), apply: make(chan struct{}, 1), running: map[probeKey]bool{}, known: map[string]bool{}}
}

// State returns the State the Prober records into. Readers use Snapshot.
func (p *Prober) State() *State { return p.state }

// jittered returns interval moved by up to 10% either way: r in [0, 1) maps to 90% through (just under) 110%.
func jittered(interval time.Duration, r float64) time.Duration {
	return interval + time.Duration((2*r-1)*jitterFraction*float64(interval))
}

// Run probes until ctx ends. A pass runs at once, when a Binding appears in Targets or the Revision differs from the
// last pass's (at the next Tick), and when the jittered Interval since the last pass began has run out. A pass
// finishes before the next begins, so passes never overlap. Call Run once.
//
// When the context ends, Run waits up to a second (shutdownGrace) for each checker still running, so a checker that
// honors its context has finished when Run returns, unless it takes longer than that to unwind. Go cannot stop a
// goroutine, so a checker that ignores its context may outlive Run, until it returns. During a run, such a checker's
// probe is recorded as Unknown/ProbeTimeout when the Timeout ends and frees its place at once; the target is not
// probed again while its checker is still running, so at most one goroutine per target is lost, and what it returns
// late is dropped.
func (p *Prober) Run(ctx context.Context) {
	var (
		rev     string
		nextAt  time.Time
		started bool
	)
	for {
		// Revision is read before Targets: a reload that lands between the two reads is then seen by Targets
		// under the old revision, and the next Tick sees the new revision and probes again. In the other order the
		// new revision would be recorded against the old targets, and nothing would probe until the interval.
		cur := rev
		if p.o.Revision != nil {
			rctx, cancel := context.WithTimeout(ctx, p.o.Timeout)
			if r := p.o.Revision(rctx); rctx.Err() == nil {
				cur = r
			}
			cancel()
		}
		var targets []Target
		if p.o.Targets != nil {
			targets = p.o.Targets()
		}
		targets, changed := p.sync(targets)
		if ctx.Err() != nil {
			return
		}
		if began := time.Now(); !started || changed || cur != rev || !began.Before(nextAt) {
			started, rev = true, cur
			p.pass(ctx, targets)
			nextAt = began.Add(jittered(p.o.Interval, rand.Float64())) //nolint:gosec // jitter spreads probes; it is not a secret
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.o.Tick):
		}
	}
}

// sync brings the State in line with targets and reports whether a Binding is new since the last call. It drops
// repeated Binding names, marks a new Binding's conditions Pending, and prunes the Bindings that are gone.
func (p *Prober) sync(targets []Target) ([]Target, bool) {
	var (
		changed bool
		kept    = make([]Target, 0, len(targets))
		names   = make([]string, 0, len(targets))
		seen    = make(map[string]bool, len(targets))
		at      = time.Now()
	)
	for _, t := range targets {
		if seen[t.Binding] {
			continue
		}
		seen[t.Binding] = true
		kept, names = append(kept, t), append(names, t.Binding)
		if p.known[t.Binding] {
			continue
		}
		p.known[t.Binding] = true
		changed = true
		p.state.Set(t.Binding, at, p.pending(BackendReachable, p.o.Agents != nil), p.pending(ChannelJoined, p.o.Channels != nil))
	}
	for name := range p.known {
		if !seen[name] {
			delete(p.known, name)
		}
	}
	if gone := p.state.Prune(names); len(gone) > 0 && p.o.OnPrune != nil {
		p.o.OnPrune(gone)
	}
	return kept, changed
}

// pending is the Pending condition of type t, or the zero Condition (which State.Set ignores) when t is not tracked.
func (p *Prober) pending(t ConditionType, tracked bool) Condition {
	if !tracked {
		return Condition{}
	}
	return Condition{Type: t, Status: Unknown, Reason: ReasonPending}
}

// pass probes every target once, p.o.Concurrency at a time, and returns when all the probes have ended. The channel
// of Bindings that share one is probed once. Agent and channel probes alternate, so backends that hang do not hold
// every channel probe back.
func (p *Prober) pass(ctx context.Context, targets []Target) {
	var agentJobs, channelJobs []func(context.Context)
	if p.o.Agents != nil {
		for _, t := range targets {
			agentJobs = append(agentJobs, func(ctx context.Context) {
				res := p.probe(ctx, probeKey{BackendReachable, t.Binding}, func(c context.Context) (Outcome, error) { return p.o.Agents.CheckAgent(c, t.Binding) })
				p.record(ctx, BackendReachable, []Target{t}, res)
			})
		}
	}
	if p.o.Channels != nil {
		var order []string
		byChannel := map[string][]Target{}
		for _, t := range targets {
			if _, ok := byChannel[t.Channel]; !ok {
				order = append(order, t.Channel)
			}
			byChannel[t.Channel] = append(byChannel[t.Channel], t)
		}
		for _, ch := range order {
			channelJobs = append(channelJobs, func(ctx context.Context) {
				res := p.probe(ctx, probeKey{ChannelJoined, ch}, func(c context.Context) (Outcome, error) { return p.o.Channels.CheckChannel(c, ch) })
				p.record(ctx, ChannelJoined, byChannel[ch], res)
			})
		}
	}
	var jobs []func(context.Context)
	for i := 0; i < len(agentJobs) || i < len(channelJobs); i++ {
		if i < len(agentJobs) {
			jobs = append(jobs, agentJobs[i])
		}
		if i < len(channelJobs) {
			jobs = append(jobs, channelJobs[i])
		}
	}

	work := make(chan func(context.Context))
	var wg sync.WaitGroup
	for range min(p.o.Concurrency, len(jobs)) {
		wg.Go(func() {
			for job := range work {
				if ctx.Err() == nil {
					job(ctx)
				}
			}
		})
	}
feed:
	for _, job := range jobs {
		select {
		case work <- job:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
}

// probeResult is what a probe has to record.
type probeResult struct {
	out Outcome
	// err is the checker's failure, for OnProbeError; it never reaches out.Message.
	err error
}

// checked is what a checker goroutine hands back.
type checked struct {
	out Outcome
	err error
}

// probe runs fn in a goroutine of its own and waits for it for at most the probe timeout. An error, a panic, a
// timeout (whatever fn returns after it) and an outcome with no usable status all give Unknown, never True. If the
// target's previous checker has not returned, fn is not started again and the probe is a timeout at once.
func (p *Prober) probe(ctx context.Context, key probeKey, fn func(context.Context) (Outcome, error)) probeResult {
	timedOut := probeResult{out: Outcome{Status: Unknown, Reason: ReasonProbeTimeout}}
	if !p.claim(key) {
		timedOut.out.Message = "the previous probe has not returned"
		return timedOut
	}
	pctx, cancel := context.WithTimeout(ctx, p.o.Timeout)
	defer cancel()
	began := time.Now()
	done := make(chan checked, 1) // buffered: a late checker must be able to finish without a reader
	go func() {
		out, err := call(pctx, fn)
		p.release(key)
		done <- checked{out, err}
	}()

	var got checked
	var returned bool
	select {
	case got, returned = <-done:
	case <-pctx.Done():
	}
	switch {
	case ctx.Err() != nil:
		// A shutdown is not a result, and record drops it. Give a checker that honors its context time to unwind, so
		// that it has finished when Run returns.
		if !returned {
			select {
			case <-done:
			case <-time.After(shutdownGrace):
			}
		}
		return probeResult{}
	case !returned || pctx.Err() != nil || time.Since(began) >= p.o.Timeout:
		// The clock is read as well as the context: a checker that answers at the instant the deadline passes can
		// beat the context's own cancel, and an answer at the deadline is not one to believe.
		timedOut.out.Message = fmt.Sprintf("no answer within %s", p.o.Timeout)
		return timedOut
	}
	failed := func(msg string, err error) probeResult {
		return probeResult{err: err, out: Outcome{Status: Unknown, Reason: ReasonProbeFailed, Message: msg}}
	}
	switch out := got.out; {
	case got.err != nil:
		return failed("the probe could not tell", got.err)
	case !out.Status.valid():
		return failed("the probe reported no usable status", fmt.Errorf("checker reported status %q", out.Status))
	case out.Reason == "" && out.Status == True:
		out.Reason = ReasonOK
		return probeResult{out: out}
	case out.Reason == "":
		return failed("the probe reported no reason", fmt.Errorf("checker reported %s with no reason", out.Status))
	default:
		return probeResult{out: out}
	}
}

func (p *Prober) claim(key probeKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running[key] {
		return false
	}
	p.running[key] = true
	return true
}

func (p *Prober) release(key probeKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.running, key)
}

// call runs fn and turns a panic into a *PanicError.
func call(ctx context.Context, fn func(context.Context) (Outcome, error)) (out Outcome, err error) {
	defer func() {
		if v := recover(); v != nil {
			out, err = Outcome{}, &PanicError{Value: v, Stack: debug.Stack()}
		}
	}()
	return fn(ctx)
}

// record sets the result as condition t of each of targets and calls the hooks. It records nothing once ctx has ended.
func (p *Prober) record(ctx context.Context, t ConditionType, targets []Target, res probeResult) {
	select {
	case p.apply <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-p.apply }()
	if ctx.Err() != nil { // both could be ready above
		return
	}
	if res.err != nil && p.o.OnProbeError != nil {
		p.o.OnProbeError(ProbeError{Condition: t, Targets: targets, Err: res.err})
	}
	at := time.Now()
	for _, tg := range targets {
		for _, tr := range p.state.Set(tg.Binding, at, Condition{Type: t, Status: res.out.Status, Reason: res.out.Reason, Message: res.out.Message}) {
			if p.o.OnTransition != nil {
				p.o.OnTransition(tr)
			}
		}
	}
}
