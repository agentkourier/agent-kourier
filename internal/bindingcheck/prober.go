package bindingcheck

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
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
)

// errPanicked marks a checker that panicked. It carries no panic value, which could hold a secret.
var errPanicked = errors.New("checker panicked")

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

// Options configure a Prober. Zero fields take their defaults.
type Options struct {
	// Targets lists the Bindings to probe now. It is called on every Tick, so it should be cheap. A Binding that
	// stops appearing in it is pruned from the State.
	Targets func() []Target
	// Revision names the state of the config the probes depend on. When its result differs from the one the last pass
	// saw, the next Tick runs a pass at once. It runs on the Tick loop, so it should return promptly. Nil means the
	// config is never revised.
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
	// Timeout is how long one probe may take, 10 seconds by default. A probe that takes longer is Unknown.
	Timeout time.Duration
	// Concurrency is how many probes run at once, 4 by default.
	Concurrency int
	// OnTransition is called for each change of a condition's status or reason, Ready included (filter on
	// Transition.New.Type), and never for a probe that changed neither. Calls are serialized, run on a probe's
	// goroutine and block the other probes while they run, so the hook must be quick and must not wait on the Prober.
	// A new Binding's Pending conditions are not transitions.
	OnTransition func(Transition)
	// Now and After are the clock; a test injects its own. Defaults: time.Now and time.After.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// Rand returns a number in [0, 1) for the jitter. Default: math/rand/v2's Float64.
	Rand func() float64
}

// Prober probes the Bindings of its Targets and records what the checkers report in a State.
type Prober struct {
	o     Options
	state *State

	applyMu sync.Mutex // serializes recording an outcome with its OnTransition calls

	known map[string]string // binding -> channel, as of the last Tick; owned by Run's goroutine
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
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.After == nil {
		o.After = time.After
	}
	if o.Rand == nil {
		o.Rand = rand.Float64
	}
	return &Prober{o: o, state: NewState(), known: map[string]string{}}
}

// State returns the State the Prober records into. Readers use Snapshot.
func (p *Prober) State() *State { return p.state }

// jittered returns interval moved by up to 10% either way: r in [0, 1) maps to 90% through (just under) 110%.
func jittered(interval time.Duration, r float64) time.Duration {
	return interval + time.Duration((2*r-1)*jitterFraction*float64(interval))
}

// Run probes until ctx ends, then returns once every probe it started has returned. A pass runs at once, when the
// Targets change or the Revision differs from the last pass's (at the next Tick), and when the jittered Interval
// since the last pass began has run out. A pass finishes before the next begins, so passes never overlap. Call Run
// once.
func (p *Prober) Run(ctx context.Context) {
	var (
		rev     string
		nextAt  time.Time
		started bool
	)
	for {
		var targets []Target
		if p.o.Targets != nil {
			targets = p.o.Targets()
		}
		targets, changed := p.sync(targets)
		var cur string
		if p.o.Revision != nil {
			cur = p.o.Revision(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		if began := p.o.Now(); !started || changed || cur != rev || !began.Before(nextAt) {
			started, rev = true, cur
			p.pass(ctx, targets)
			nextAt = began.Add(jittered(p.o.Interval, p.o.Rand()))
		}
		select {
		case <-ctx.Done():
			return
		case <-p.o.After(p.o.Tick):
		}
	}
}

// sync brings the State in line with targets and reports whether they differ from the last call's. It drops repeated
// Binding names, marks a new Binding's conditions Pending, marks the channel condition of a Binding that moved to
// another channel Pending, and prunes the Bindings that are gone.
func (p *Prober) sync(targets []Target) ([]Target, bool) {
	var (
		changed bool
		kept    = make([]Target, 0, len(targets))
		names   = make([]string, 0, len(targets))
		seen    = make(map[string]bool, len(targets))
		at      = p.o.Now()
	)
	for _, t := range targets {
		if seen[t.Binding] {
			continue
		}
		seen[t.Binding] = true
		kept, names = append(kept, t), append(names, t.Binding)
		prev, had := p.known[t.Binding]
		switch {
		case !had:
			p.state.Set(t.Binding, at, p.pending(BackendReachable, p.o.Agents != nil), p.pending(ChannelJoined, p.o.Channels != nil))
		case prev != t.Channel && p.o.Channels != nil:
			p.state.Set(t.Binding, at, p.pending(ChannelJoined, true))
		default:
			continue
		}
		changed = true
		p.known[t.Binding] = t.Channel
	}
	for name := range p.known {
		if !seen[name] {
			delete(p.known, name)
		}
	}
	p.state.Prune(names)
	return kept, changed
}

// pending is the Pending condition of type t, or the zero Condition (which State.Set ignores) when t is not tracked.
func (p *Prober) pending(t ConditionType, tracked bool) Condition {
	if !tracked {
		return Condition{}
	}
	return Condition{Type: t, Status: Unknown, Reason: ReasonPending}
}

// pass probes every target once, p.o.Concurrency at a time, and returns when all the probes have returned. The
// channel of Bindings that share one is probed once.
func (p *Prober) pass(ctx context.Context, targets []Target) {
	var jobs []func(context.Context)
	if p.o.Agents != nil {
		for _, t := range targets {
			jobs = append(jobs, func(ctx context.Context) {
				out, ok := p.probe(ctx, func(c context.Context) (Outcome, error) { return p.o.Agents.CheckAgent(c, t.Binding) })
				if ok {
					p.record(BackendReachable, []string{t.Binding}, out)
				}
			})
		}
	}
	if p.o.Channels != nil {
		var order []string
		byChannel := map[string][]string{}
		for _, t := range targets {
			if _, ok := byChannel[t.Channel]; !ok {
				order = append(order, t.Channel)
			}
			byChannel[t.Channel] = append(byChannel[t.Channel], t.Binding)
		}
		for _, ch := range order {
			jobs = append(jobs, func(ctx context.Context) {
				out, ok := p.probe(ctx, func(c context.Context) (Outcome, error) { return p.o.Channels.CheckChannel(c, ch) })
				if ok {
					p.record(ChannelJoined, byChannel[ch], out)
				}
			})
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
	for _, job := range jobs {
		work <- job
	}
	close(work)
	wg.Wait()
}

// probe runs fn under the probe timeout and returns the outcome to record. It returns false when ctx ended, since
// a shutdown is not a result. An error, a panic, a timeout (whatever fn returned after it) and an outcome with no
// valid status all give Unknown, never True.
func (p *Prober) probe(ctx context.Context, fn func(context.Context) (Outcome, error)) (Outcome, bool) {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := p.o.After(p.o.Timeout)
	var watch sync.WaitGroup
	watch.Go(func() {
		select {
		case <-timer:
			cancel()
		case <-pctx.Done():
		}
	})
	out, err := call(pctx, fn)
	expired := pctx.Err() != nil
	cancel()
	watch.Wait()

	switch {
	case ctx.Err() != nil:
		return Outcome{}, false
	case expired:
		return Outcome{Status: Unknown, Reason: ReasonProbeTimeout, Message: fmt.Sprintf("no answer within %s", p.o.Timeout)}, true
	case err != nil:
		return Outcome{Status: Unknown, Reason: ReasonProbeFailed, Message: "the probe could not tell"}, true
	case !out.Status.valid():
		return Outcome{Status: Unknown, Reason: ReasonProbeFailed, Message: "the probe reported no usable status"}, true
	}
	if out.Reason == "" {
		out.Reason = ReasonProbeFailed
		if out.Status == True {
			out.Reason = ReasonOK
		}
	}
	return out, true
}

// call runs fn and turns a panic into an error.
func call(ctx context.Context, fn func(context.Context) (Outcome, error)) (out Outcome, err error) {
	defer func() {
		if recover() != nil {
			out, err = Outcome{}, errPanicked
		}
	}()
	return fn(ctx)
}

// record sets out as condition t of each of bindings and calls OnTransition for each transition.
func (p *Prober) record(t ConditionType, bindings []string, out Outcome) {
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	at := p.o.Now()
	for _, b := range bindings {
		for _, tr := range p.state.Set(b, at, Condition{Type: t, Status: out.Status, Reason: out.Reason, Message: out.Message}) {
			if p.o.OnTransition != nil {
				p.o.OnTransition(tr)
			}
		}
	}
}
