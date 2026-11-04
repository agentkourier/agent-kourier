package bindingcheck

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults of the Options that are left zero.
const (
	defaultInterval    = 5 * time.Minute
	defaultTick        = 5 * time.Second
	defaultTimeout     = 10 * time.Second
	defaultConcurrency = 4
	// jitterFraction is how far the time from one pass's start to the next's strays from Interval, either way.
	jitterFraction = 0.1
	// shutdownGrace is how long Run, once its context has ended, waits in all for the checkers and the Revision call
	// that are still running. A checker that honors its context unwinds well inside it (broker shutdown steps take 2
	// to 7 seconds); one that ignores its context is left behind.
	shutdownGrace = time.Second
)

// Target is a Binding to probe and the chat channel it posts to.
type Target struct {
	// Binding is the Binding's key, namespace/name, which is the key of its conditions. The bare name is not enough:
	// two Bindings of one name in two namespaces are two Bindings, and the checkers look the Binding up by this key.
	Binding string
	// Channel is the chat channel the Binding is bound to.
	Channel string
	// Generation is the metadata.generation of the Binding as it is in the config now, zero for one read from a file. The
	// Prober stamps each result with the one its pass began with (Condition.ObservedGeneration).
	Generation int64
	// UID is the metadata.uid of the Binding, empty for one read from a file. A Binding of the same name with another UID is
	// another object (deleted and made again, which restarts its generation at 1): at the next Tick after a pass the Prober
	// forgets what it found of the first and makes the conditions of the second Pending at its generation. Each result is
	// stamped with the UID of its pass (Condition.UID), so that whoever reads it can tell whose it is while a pass that began
	// before the change is still running.
	UID string
}

// AgentChecker probes the agent backend of a Binding. It sets BackendReachable.
type AgentChecker interface {
	// CheckAgent reports whether binding's backend can be used. binding is the Target's key, namespace/name. It must
	// honor ctx, which ends at the probe timeout. An error means the probe could not tell and leaves the condition
	// Unknown; a checker that can classify a failure returns it as an Outcome with a nil error.
	CheckAgent(ctx context.Context, binding string) (Outcome, error)
}

// ChannelChecker probes a chat channel. It sets ChannelJoined for the Binding whose Target names the channel; each
// Target is probed on its own, even when several name one channel. A ChannelChecker answers for one bot, so Targets
// must hold only Bindings on that bot's chat connection: for a Binding on another connection its answer is the wrong
// bot's, and a True it gives would be false.
type ChannelChecker interface {
	// CheckChannel reports whether the bot is in channel. It must honor ctx, which ends at the probe timeout. An
	// error means the probe could not tell and leaves the condition Unknown.
	CheckChannel(ctx context.Context, channel string) (Outcome, error)
}

// PanicError is the error OnProbeError gets for a checker that panicked. For a panic of Targets, Revision,
// OnTransition or OnPrune it is wrapped in the error (find it with errors.As).
type PanicError struct {
	// Value is what was passed to panic. It may hold anything the panicking code had in hand, so redact it.
	Value any
	// Stack is the goroutine's stack at the panic.
	Stack []byte
}

// Error describes the panic. It includes Value, so redact it before logging.
func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// ProbeError is a failure the Prober reports to OnProbeError. Most are probes whose checker failed: it returned an
// error, panicked, or reported an outcome with no usable status or reason. The condition is then Unknown/ProbeFailed
// and its Message says nothing of Err. The others are failures of the caller's own code, which leave no condition
// behind: Condition is empty and Target the zero Target when Targets, Revision or OnPrune panicked or Revision did
// not answer in time or ended its goroutine (Targets is reported on every Tick it panics, Revision for every call
// that fails). When OnTransition panicked, Condition and Target are the probe whose result was being recorded, which
// is the Binding whose transition it was.
type ProbeError struct {
	// Condition is the condition the probe was setting. It is empty when the failure is not a probe's, as above.
	Condition ConditionType
	// Target is the Binding the probe was setting it for. It is the zero Target for a failure that is not a probe's.
	Target Target
	// Err is the failure. For a panic it is or wraps a *PanicError.
	Err error
}

// Options configure a Prober. Zero fields take their defaults.
type Options struct {
	// Targets lists the Bindings to probe now. It is called on Run's goroutine on every Tick, so it must be cheap
	// and must not block: a call that does not return stops the Prober. A Binding that stops appearing in it is
	// pruned from the State. When a call panics, the panic is reported to OnProbeError and the Prober keeps the
	// Targets of the last good call, prunes nothing and probes those; a panic never reads as an empty list.
	Targets func() []Target
	// Revision names the state of the config the probes depend on, and a change of it (from what the last pass saw)
	// runs a pass at the next Tick, or when MinRevisionGap since the last pass began is over, if that is later; a channel or a Secret changes only through it. Each call runs in a goroutine of
	// its own under the Timeout, and must honor its context. When a read fails it should return a marker that
	// differs from any revision it returns on success and is the same on every call while the failure lasts: the
	// next pass then reports the failure, and the recovery is another change that runs another pass. A call that
	// panics, ends its goroutine (runtime.Goexit) or outlasts the Timeout counts as no change and is reported to
	// OnProbeError. While a call that outlasted
	// the Timeout has not returned, Revision is not called again and each Tick counts as no change (unreported), so
	// a Revision that ignores its context loses one goroutine, and what it returns late is dropped. Nil means the
	// config is never revised.
	Revision func(ctx context.Context) string
	// Agents probes backends. Nil means BackendReachable is not tracked.
	Agents AgentChecker
	// Channels probes channels, one probe per Target. Nil means ChannelJoined is not tracked. Targets must hold only
	// Bindings on the chat connection that Channels answers for (see ChannelChecker).
	Channels ChannelChecker
	// MinRevisionGap is the least time between the start of a pass and the start of one that only a change calls for: a change of
	// Revision, or a Binding that is new in Targets (a reload that adds one is a change like any other). A change that comes inside
	// the gap is not lost: it waits, and the pass runs at the first Tick after the gap, for whatever the Revision and the Targets are
	// then, so many changes in the gap give one pass, which sees the last. A new Binding's conditions are Pending until that pass. It
	// does not hold back the first pass or a pass the Interval calls for, and it does not hold back pruning, which happens at every
	// Tick, and so waits for a pass that is running to end (a Binding that leaves Targets is no change that needs a pass). It exists so that a Revision that moves for no cause the
	// probes can see, such as a Secret written to again and again, cannot make the Prober probe every Binding at every Tick. Zero
	// means no gap.
	MinRevisionGap time.Duration
	// Interval is how long after a pass begins the next is due, 5 minutes by default, and not a pause after it ends.
	// Each wait strays from it by up to 10% either way, so that many brokers do not probe together. A pass that outlasts
	// its wait is followed by the next one Tick after it ends. The loop looks only at each Tick, so a pass can begin up
	// to a Tick after it is due.
	Interval time.Duration
	// Tick is how often the loop looks at Targets and Revision, 5 seconds by default.
	Tick time.Duration
	// Timeout is how long one probe (and one Revision call) may take, 10 seconds by default. A probe that takes
	// longer is Unknown/ProbeTimeout. The loop waits for a Revision call for up to this long.
	Timeout time.Duration
	// Concurrency is how many probes run at once, 4 by default.
	Concurrency int
	// OnTransition is called for each change of a condition's status or reason, Ready included (filter on
	// Transition.New.Type), and never for a probe that changed neither. The Pending a new Binding's probed conditions are stored
	// with is not a transition, but Ready's can be: when the first probes end one at a time, Ready moves from being Pending on one
	// condition to being Pending on the next, so a consumer that treats Ready as unknown only once it has a result tests
	// Condition.Pending. Calls are serialized with each other and with OnProbeError, run on a probe's goroutine and hold
	// up the other probes' results while they run, so the hook must be quick and must not wait on the Prober. The
	// Prober does not bound a hook, so one that blocks stops probing and delays shutdown. A hook that panics is
	// recovered and the panic is reported to OnProbeError; the State is already updated.
	OnTransition func(Transition)
	// OnProbeError is called for each failure ProbeError describes, with the same serialization and cost as
	// OnTransition (a failure of Targets or Revision comes from Run's goroutine, when no probe is running). Nothing
	// in the error reaches a Message, so the hook is where a failure is seen; it should redact before it logs. A
	// panic of this hook is recovered and dropped, not reported.
	OnProbeError func(ProbeError)
	// OnPrune is called on Run's goroutine with the keys (namespace/name) of the Bindings pruned from the State, when there are any.
	// Every OnTransition call for a Binding comes before the OnPrune call that names it, and none follows it. It
	// must not block, for the reason given at OnTransition, and a panic is recovered and reported to OnProbeError.
	OnPrune func(names []string)
	// OnPass is called on Run's goroutine when a pass has ended and its results are recorded, before the Prober looks at the
	// next Tick. A probe that finds what it found before is no transition, but it moves the condition's ObservedGeneration, and
	// this is how a consumer that writes conditions somewhere learns that. It must not block, as OnPrune must not, and a panic
	// is recovered and reported to OnProbeError. It is not called for a pass that ctx ended.
	OnPass func()
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

	// started holds the checker and Revision goroutines, for Run to wait for. revisionBusy is set while a Revision
	// call is running, so that a call that ignores its context is not followed by another.
	started      sync.WaitGroup
	revisionBusy atomic.Bool

	known map[string]string // the UID of each Binding seen at the last Tick, by key; owned by Run's goroutine
}

// probeKey identifies a probe: a condition type and a Binding's key.
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
	return &Prober{o: o, state: NewState(), apply: make(chan struct{}, 1), running: map[probeKey]bool{}, known: map[string]string{}}
}

// State returns the State the Prober records into. Readers use Snapshot.
func (p *Prober) State() *State { return p.state }

// jittered returns interval moved by up to 10% either way: r in [0, 1) maps to 90% through (just under) 110%.
func jittered(interval time.Duration, r float64) time.Duration {
	return interval + time.Duration((2*r-1)*jitterFraction*float64(interval))
}

// Run probes until ctx ends. A pass runs at once, when a Binding appears in Targets or the Revision differs from the
// last pass's (at the next Tick, and not within MinRevisionGap of the start of the last pass: the pass then waits for the
// gap to end, with the change kept), and when the jittered Interval since the last pass began has run out. A pass
// finishes before the next begins, so passes never overlap. Call Run once.
//
// When the context ends, Run waits for the checkers and the Revision call it started that are still running, up to
// a second (shutdownGrace) for all of them together, so a checker that honors its context has finished when Run
// returns, whether or not its probe had timed out, unless it takes longer than that to unwind. Go cannot stop a
// goroutine, so a checker that ignores its context may outlive Run, until it returns. During a run, such a checker's
// probe is recorded as Unknown/ProbeTimeout when the Timeout ends and frees its place at once; the target is not
// probed again for that condition while its checker is still running, even if the Binding has moved to another channel,
// so at most one goroutine per condition of a target is lost, two for a target, and what it returns late is dropped.
func (p *Prober) Run(ctx context.Context) {
	defer p.waitForGoroutines()
	var (
		rev      string
		nextAt   time.Time
		started  bool
		lastPass time.Time // when the last pass began
		fresh    bool      // a Binding appeared since the last pass: the changed that sync reports once, kept until a pass runs
		targets  []Target  // the last good Targets, deduplicated
	)
	for {
		// Revision is read before Targets: a reload that lands between the two reads is then seen by Targets
		// under the old revision, and the next Tick sees the new revision and probes again. In the other order the
		// new revision would be recorded against the old targets, and nothing would probe until the interval.
		cur := rev
		if r, ok := p.revision(ctx); ok {
			cur = r
		}
		if got, ok := p.listTargets(); ok {
			var changed bool
			targets, changed = p.sync(got)
			fresh = fresh || changed
		} else {
			// The revision cannot be probed against targets that are not known to be current. Leave it unseen, so
			// that the first Tick where Targets answers sees the change and probes.
			cur = rev
		}
		if ctx.Err() != nil {
			return
		}
		if began := time.Now(); !started || !began.Before(nextAt) || ((fresh || cur != rev) && !began.Before(lastPass.Add(p.o.MinRevisionGap))) {
			started, rev, lastPass, fresh = true, cur, began, false
			p.pass(ctx, targets)
			if ctx.Err() == nil && p.o.OnPass != nil {
				p.hook("OnPass", "", Target{}, p.o.OnPass)
			}
			nextAt = began.Add(jittered(p.o.Interval, rand.Float64())) //nolint:gosec // jitter spreads probes; it is not a secret
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.o.Tick):
		}
	}
}

// waitForGoroutines waits for the goroutines Run started, for shutdownGrace in all.
func (p *Prober) waitForGoroutines() {
	done := make(chan struct{})
	go func() { p.started.Wait(); close(done) }()
	t := time.NewTimer(shutdownGrace)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// revision calls Options.Revision in a goroutine of its own and reports whether it answered within the Timeout
// without panicking. A failure is reported to OnProbeError, unless the context ended or a call that outlasted the
// Timeout is still running, in which case Revision is not called again.
func (p *Prober) revision(ctx context.Context) (string, bool) {
	if p.o.Revision == nil || !p.revisionBusy.CompareAndSwap(false, true) {
		return "", false
	}
	type result struct {
		rev string
		ok  bool // false when the goroutine ended without returning (runtime.Goexit)
		err error
	}
	rctx, cancel := context.WithTimeout(ctx, p.o.Timeout)
	defer cancel()
	done := make(chan result, 1) // buffered: a late call must be able to finish without a reader
	p.started.Go(func() {
		var res result
		defer func() { p.revisionBusy.Store(false); done <- res }()
		res.err = guard(func() { res.rev = p.o.Revision(rctx) })
		res.ok = true
	})
	var (
		res      result
		returned bool
	)
	select {
	case res, returned = <-done:
	case <-rctx.Done():
	}
	switch {
	case ctx.Err() != nil: // a shutdown is not a failure
	case res.err != nil:
		p.fail(ProbeError{Err: fmt.Errorf("calling Revision: %w", res.err)})
	case !returned || rctx.Err() != nil:
		p.fail(ProbeError{Err: fmt.Errorf("revision did not answer within %s", p.o.Timeout)})
	case !res.ok:
		p.fail(ProbeError{Err: errors.New("revision ended its goroutine without returning")})
	default:
		return res.rev, true
	}
	return "", false
}

// listTargets calls Options.Targets and reports whether it returned. A panic is reported to OnProbeError and gives
// false, which is not the same as an empty list: the caller keeps its last targets.
func (p *Prober) listTargets() ([]Target, bool) {
	if p.o.Targets == nil {
		return nil, true
	}
	var got []Target
	if err := guard(func() { got = p.o.Targets() }); err != nil {
		p.fail(ProbeError{Err: fmt.Errorf("calling Targets: %w", err)})
		return nil, false
	}
	return got, true
}

// fail calls OnProbeError. A panic of the hook is dropped: reporting it to the hook would recurse.
func (p *Prober) fail(pe ProbeError) {
	if p.o.OnProbeError != nil {
		_ = guard(func() { p.o.OnProbeError(pe) })
	}
}

// hook runs one of the caller's hooks (name says which) and reports its panic to OnProbeError for cond and target.
func (p *Prober) hook(name string, cond ConditionType, target Target, fn func()) {
	if err := guard(fn); err != nil {
		p.fail(ProbeError{Condition: cond, Target: target, Err: fmt.Errorf("calling %s: %w", name, err)})
	}
}

// sync brings the State in line with targets and reports whether a Binding is new since the last call. It drops
// repeated Binding keys, marks a new Binding's conditions Pending, and prunes the Bindings that are gone.
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
		if uid, ok := p.known[t.Binding]; ok && uid == t.UID {
			continue
		}
		p.known[t.Binding] = t.UID
		changed = true
		p.state.Set(t.Binding, at, p.pending(BackendReachable, p.o.Agents != nil, t.Generation, t.UID), p.pending(ChannelJoined, p.o.Channels != nil, t.Generation, t.UID))
	}
	for name := range p.known {
		if !seen[name] {
			delete(p.known, name)
		}
	}
	if gone := p.state.Prune(names); len(gone) > 0 && p.o.OnPrune != nil {
		p.hook("OnPrune", "", Target{}, func() { p.o.OnPrune(gone) })
	}
	return kept, changed
}

// pending is the Pending condition of type t, or the zero Condition (which State.Set ignores) when t is not tracked.
func (p *Prober) pending(t ConditionType, tracked bool, generation int64, uid string) Condition {
	if !tracked {
		return Condition{}
	}
	return Condition{Type: t, Status: Unknown, Reason: ReasonPending, ObservedGeneration: generation, UID: uid}
}

// pass probes every target once, p.o.Concurrency at a time, and returns when all the probes have ended. Each target
// gets its own agent probe and its own channel probe, even when targets share a channel. Agent and channel probes
// alternate, so backends that hang do not hold every channel probe back.
func (p *Prober) pass(ctx context.Context, targets []Target) {
	var agentJobs, channelJobs []func(context.Context)
	if p.o.Agents != nil {
		for _, t := range targets {
			agentJobs = append(agentJobs, func(ctx context.Context) {
				res := p.probe(ctx, probeKey{BackendReachable, t.Binding}, func(c context.Context) (Outcome, error) { return p.o.Agents.CheckAgent(c, t.Binding) })
				p.record(ctx, BackendReachable, t, res)
			})
		}
	}
	if p.o.Channels != nil {
		for _, t := range targets {
			channelJobs = append(channelJobs, func(ctx context.Context) {
				res := p.probe(ctx, probeKey{ChannelJoined, t.Binding}, func(c context.Context) (Outcome, error) { return p.o.Channels.CheckChannel(c, t.Channel) })
				p.record(ctx, ChannelJoined, t, res)
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
// timeout (whatever fn returns after it) and an outcome with no usable status or reason all give Unknown, never True. If the
// target's previous checker has not returned, fn is not started again and the probe is a timeout at once. Run, not
// probe, waits for the goroutine at shutdown.
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
	p.started.Go(func() {
		// Deferred, so that a checker that ends its goroutine (runtime.Goexit, as a test fake's t.Fatal does) still
		// frees its target and answers; the zero answer has no usable status and is a failed probe.
		var got checked
		defer func() { p.release(key); done <- got }()
		if err := guard(func() { got.out, got.err = fn(pctx) }); err != nil {
			got.out, got.err = Outcome{}, err
		}
	})

	var got checked
	var returned bool
	select {
	case got, returned = <-done:
	case <-pctx.Done():
	}
	switch {
	case ctx.Err() != nil:
		// A shutdown is not a result, and record drops it.
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
		// The value is left out of the error, as for a reason below: it may be free text, such as a checker's error.
		return failed("the probe reported no usable status", errors.New("checker reported a status outside the three"))
	case out.Reason == "" && out.Status == True:
		out.Reason = ReasonOK
		return probeResult{out: out}
	case out.Reason == "":
		return failed("the probe reported no reason", fmt.Errorf("checker reported %s with no reason", out.Status))
	case !out.Reason.reportable(out.Status):
		return failed("the probe reported an unusable reason", fmt.Errorf("checker reported %s with a reason it may not report", out.Status))
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

// guard runs fn and returns the panic it raised, if any, as a *PanicError.
func guard(fn func()) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = &PanicError{Value: v, Stack: debug.Stack()}
		}
	}()
	fn()
	return nil
}

// record sets the result as condition t of target and calls the hooks. It records nothing once ctx has ended.
func (p *Prober) record(ctx context.Context, t ConditionType, target Target, res probeResult) {
	select {
	case p.apply <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-p.apply }()
	if ctx.Err() != nil { // both could be ready above
		return
	}
	if res.err != nil {
		p.fail(ProbeError{Condition: t, Target: target, Err: res.err})
	}
	at := time.Now()
	cond := Condition{Type: t, Status: res.out.Status, Reason: res.out.Reason, Message: res.out.Message, Cause: res.out.Cause, ObservedGeneration: target.Generation, UID: target.UID}
	for _, tr := range p.state.Set(target.Binding, at, cond) {
		if p.o.OnTransition != nil {
			p.hook("OnTransition", t, target, func() { p.o.OnTransition(tr) })
		}
	}
}
