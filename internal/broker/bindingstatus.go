package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// The status writer is the half of the custom resource config source (spec D50) that tells each Binding's owner what became of it:
// it writes Binding.status.conditions, from the leader only, and only with the crds config source. It reads three things the
// broker already has: what the source refused (config.Report), what the Prober found (bindingcheck.State), and the status that
// each Binding has now, from the cache of the source's Binding informer. It never reads the spec.

const (
	// statusFieldManager is the field manager of every write of a status. It is fixed, so that every leader and every replica
	// that ever led owns the same fields and a write by the next one is an update and not a conflict.
	statusFieldManager = "agent-kourier"
	// maxStatusConditions is what the CRD's schema takes for a Binding's conditions (+kubebuilder:validation:MaxItems on
	// BindingStatus.Conditions): a write of more is refused whole. The writer writes only the types in statusTypes, which are
	// fewer (a test holds it to that), and so decides nothing that can exceed it.
	maxStatusConditions = 16
	// maxStatusMessage is the most bytes of a condition's message: a message is a line for a person, not a log.
	maxStatusMessage = 1024
	// The defaults of statusConfig's timings. A Binding's status is written at most once each statusGap (counted from the end of
	// its last write), the writer looks at what moved once each statusTick and makes a whole pass at least once each statusSlow
	// (so that a write that was held back or failed is tried when it is due), and a call to the API server is cut at statusTimeout
	// (statusCallTimeout gives the one a leader uses). The client stops waiting then; a request that was sent is not unsent.
	statusGap     = 10 * time.Second
	statusTick    = time.Second
	statusSlow    = 10 * time.Second
	statusTimeout = 3 * time.Second
	// statusFloor is the least a call is given, whatever margin the election leaves: a write that is cut at once never succeeds.
	// A margin under twice the floor is not one the writer can keep (the write then outlasts half of it); the fence is still asked
	// before each write, and a write the client gave up on is not unsent.
	statusFloor = 250 * time.Millisecond
	// fightWindow and fightPutBacks say when a Binding's status is being fought over: this many put-backs, each within the window
	// (in gaps) of the one before. A status that is put back once because a person edited it is no fight.
	fightWindow   = 5
	fightPutBacks = 3
	// statusBackoffCap is the longest wait after failed writes of one Binding (it starts at the gap and doubles).
	statusBackoffCap = 5 * time.Minute

	// conditionAccepted is the condition that the writer composes from what the source refused: the probes of the Prober have
	// no part in it, and a Binding that is not served has no Target for them.
	conditionAccepted bindingcheck.ConditionType = "Accepted"
	// reasonRejected is the reason of an Accepted condition whose reason the config package has not named here: a reason
	// outside the closed set is a reason that no one wrote a rule for, and still fits a condition's reason pattern.
	reasonRejected = "Rejected"
	// acceptedPrefix starts the reason of a Ready condition that is False because the Binding is not accepted.
	acceptedPrefix = "Accepted:"
	// ellipsis marks the cut of a message that was too long.
	ellipsis = "…"
)

// statusTypes are the condition types that the writer owns: it writes them and no others, and a type of the same Binding that
// someone else wrote is left alone. It is what a Prober's State can hold, and Accepted.
var statusTypes = append(bindingcheck.Types(), conditionAccepted)

// probedTypes are the conditions that every Binding the source serves has once the Prober has seen it: a writer that looks before
// the Prober's first Tick finds none of them, and says they are Pending, not that they are gone. (ApproversReadable is left out,
// because it applies only to a Binding that asks for approvals from groups (asksForApprovers); the writer adds it for one that does.)
var probedTypes = []bindingcheck.ConditionType{bindingcheck.BackendReachable, bindingcheck.ChannelJoined}

// acceptedReasonOrder is the order in which the reasons of the Rejections of one Binding give way to one: the first of them that
// the Binding has is its Accepted reason, and the messages of all follow in the same order. It is the order of the spec's row for
// the condition, and puts what is wrong with the object itself before what is wrong with the objects it names, and the
// channel, which a Binding can lose only if it resolved, last. ConnectionNotServed is the writer's own (it comes from the connections
// that the report lists as left out, not from a Rejection), and no Binding has it with another reason, since a Rejection of the
// Binding says more.
var acceptedReasonOrder = []config.RejectionReason{
	config.ReasonInvalidSpec, config.ReasonBackendNotFound, config.ReasonBackendRejected, config.ReasonConnectionNotFound,
	config.ReasonConnectionRejected, config.ReasonConnectionNotServed, config.ReasonNamespaceNotAllowed, config.ReasonAgentNamespaceNotAllowed,
	config.ReasonChannelConflict,
}

// boundBy finds the Binding that holds a channel in the message of a ChannelConflict, which names it as "already bound by
// Binding <namespace>/<name>". Resolution writes that, and the report adds words after it when the holder is a Binding that is
// deleted and held (" (that Binding is deleted, and held until its connection is back)"), so the name ends at the first space and
// what follows is kept. A test holds this to what resolution writes.
var boundBy = regexp.MustCompile(`already bound by Binding ([^/\s]+)/\S+`)

var bindingsGVR = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "bindings"}

// warningCounter is the rest.WarningHandler of the status writer's client. A write to a status runs every rule of the
// schema, those of the spec included, and the API server answers one Warning header for each rule that the stored spec
// breaks (the rules are ratcheted, so the write itself passes where the API server ratchets a status write): for an object that
// was stored invalid that is a line for every write, and the client-go default would log each. The counter keeps the number and
// the first text, and logs nothing.
type warningCounter struct {
	mu    sync.Mutex
	n     int
	first string
}

// HandleWarningHeader implements rest.WarningHandler.
func (c *warningCounter) HandleWarningHeader(code int, _ string, text string) {
	if code != 299 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == 0 {
		c.first = text
	}
	c.n++
}

// take returns the warnings since the last call, how many and the first one's text, and starts again. A nil counter has none.
func (c *warningCounter) take() (int, string) {
	if c == nil {
		return 0, ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n, first := c.n, c.first
	c.n, c.first = 0, ""
	return n, first
}

// statusSink is what Run hands the leader half to write statuses with, made only with the crds config source: the client, and the
// counter of its WarningHandler (the one Run made with the client, or the one a test gave).
type statusSink struct {
	client   dynamic.Interface
	warnings *warningCounter
}

// statusConfig is what a statusWriter is built from.
type statusConfig struct {
	// Client writes the statuses. It is the status client (InClusterStatusClient), whose rate limiter is its own.
	Client dynamic.Interface
	// Report is what the config source knows besides the snapshot (Reloader.Report), Conditions is what the Prober found, by
	// Binding (State.Snapshot), and Observed is the status that a Binding has now, from the cache of the source's Binding informer
	// ((*crsource.Source).BindingStatus). All required.
	Report     func() config.Report
	Conditions func() map[string][]bindingcheck.Condition
	Observed   func(namespace, name string) (crsource.BindingStatus, bool, error)
	// Version, when set, is the count of the events of the Binding informer ((*crsource.Source).BindingEvents): the writer makes a
	// pass at a tick only if it, the report, the Prober's hooks or the clock say that something moved. Nil makes a pass at every
	// tick.
	Version func() uint64
	// Fence, when set, is checked before each write: a leader whose lease is past its renew deadline writes nothing. Nil with
	// SQLite, which has no lease.
	Fence *leaseFence
	// Warnings is the counter of the client's WarningHandler, nil if the client is not one that counts.
	Warnings *warningCounter
	Log      *slog.Logger
	// OnWrite is told the result of each write, "written", "put_back" (the status was written again to the words it already had)
	// or "failed", for a metric. Nil means none.
	OnWrite func(result string)
	// Served, when set, is the leader's record of the chat connections it serves (servedRecord), which the writer updates at each pass and
	// from which it tells the Bindings of the connections the broker served before and does not now. Without it the writer tells only
	// the Bindings of the connections that the report lists as left out.
	Served *servedRecord
	// Gap, Tick, Slow and Timeout default to statusGap, statusTick, statusSlow and statusTimeout; a test passes less. Now is
	// time.Now if nil.
	Gap, Tick, Slow, Timeout time.Duration
	Now                      func() time.Time
}

// statusWriter writes Binding.status.conditions with Server-Side Apply on the status subresource. It is built and run by the
// leader half only (leaderHalf.start), so that a standby writes nothing, and a deposed leader is stopped by the fence before a
// write. It has no queue to fill: the hooks of the Prober say that something moved (notify), and a pass over every Binding
// compares what its status should say with what it says now, and writes the difference.
//
// # What a Binding's status says
//
// Accepted is composed here from the Rejections of the report: True when there are none, else False with one reason chosen by
// acceptedReasonOrder and every message of the Binding. BackendReachable and ChannelJoined are the Prober's, and so is ApproversReadable
// for a Binding that asks for tool approvals from groups (asksForApprovers) and for none other, for a Binding the source serves, copied
// as they are, except that a message that the Prober keeps for the log never reaches
// them (Condition.Cause). A Binding that has just gained groups and that the Prober has not yet seen so has ApproversReadable Pending,
// and so is not Ready on the word of probes that did not ask about them; one that has just lost them says nothing of the condition. Ready
// is the Prober's, except that a Binding that is not accepted is not ready: False, with the reason
// "Accepted:<Reason>". Each condition has an observedGeneration: a probed one the generation its pass began at (never above the
// one served), and Accepted the newest generation that was looked at (the one that was refused, if there is one). No time of the
// last probe is written, since a status that changes every few minutes is watch traffic for everyone who watches Bindings.
//
// # Where it reads a status, and when it writes
//
// The status that a Binding has now is read from the cache of the source's Binding informer, which every replica keeps, so a
// new leader sees what the last one wrote, and an object that was deleted and made again, or edited by hand, at watch latency.
// Nothing is written before that cache has synced: a cache that cannot be read would say that every status is empty.
//
// A broker that serves a Binding (it is in the Snapshot) says what it serves, and puts it back if the status says anything else,
// whoever changed it, for one Binding at most once each Gap. A broker that only reports a Binding (the source refused it, so it
// is not in the Snapshot) writes when it first sees the Binding, when its own words change, and when the status holds nothing;
// it does not answer back to a status that differs from the one it wrote, which is the reporter's rule: the status of a Binding
// that the source refused for its spec (InvalidSpec) or for a ChatConnection that does not exist (ConnectionNotFound) may be the
// words of the broker that serves its last good generation, or of another broker that reports it, and a reporter, which cannot
// tell, must not fight them. It does put back the words of a rejection that can only be its own (a backend that is not found,
// a channel that is taken, a namespace that is not allowed: the Binding names a connection that exists, so it is this broker's),
// so that another broker's stray ConnectionNotFound does not stay over them. The serving broker's own writes are bounded by the
// Gap. Two brokers that both serve a Binding cannot be told apart from a controller that writes the status: each puts back, and
// they go on at each other every Gap; the writer counts each put-back (result "put_back") and says so once, naming the Binding,
// when it has put one back three times in a row.
//
// A Binding whose cached object is not the one the Snapshot serves (another uid: it was deleted and made again, and the Snapshot
// has not reloaded yet) is left alone, since what it would say is the old object's; so is a reported Binding whose cached
// generation is below the one the rejection was made for. The new object's status comes with the reload that brings it into the
// Snapshot, up to one poll after the watch brought the object.
//
// A new leader starts every probed condition at Pending, and writing that would blank a good status at each failover. A
// Pending condition is therefore replaced by what the status already says of its type when that is of the generation served, and
// otherwise (an older generation, a generation above the one served, or nothing) Pending is the truth, and is written. A probed
// condition whose evidence is of another object (another UID) or of a generation above the one served is Pending as well.
type statusWriter struct {
	cfg  statusConfig
	wake chan struct{}

	// dirty is set by notify: the Prober said that something moved.
	dirty atomic.Bool

	// What follows belongs to the goroutine that runs reconcile.
	keys         map[string]*statusKey
	recordFailed bool             // the failure to read or write the record has been logged
	unsynced     bool             // the cache's not being synced has been logged
	lastSnap     *config.Snapshot // the Snapshot of the last pass
	lastVersion  uint64           // the informer's event count at the last pass
	nextFull     time.Time        // a whole pass is due at this time
	retryAt      time.Time        // a write that was held back is due at this time, if it is not zero
}

// statusKey is what the writer keeps for one Binding.
type statusKey struct {
	desired   []metav1.Condition // what its status last was found or made to say; none until the writer has seen the Binding
	notBefore time.Time          // no write before this
	failures  int
	failed    bool // the current run of failures has been logged
	stored    bool // a warning about the stored spec has been logged

	putBacks    int       // put-backs in a row, each within fightWindow gaps of the one before
	lastPutBack time.Time // when the latest was made

	rvAtWrite string // the resourceVersion the cache held when this writer last wrote: a cache still at it has seen nothing since
	fought    bool   // the fight has been said
}

// statusWant is what the status of one Binding should say.
type statusWant struct {
	conditions []metav1.Condition
	served     bool   // the source serves the Binding: it is in the Snapshot
	own        bool   // served, or reported for a rejection that can only be this broker's: the writer puts its words back
	uid        string // the uid of the Binding the Snapshot serves
	observed   int64  // the generation of the Binding that the words are about: the generation served, or the one a rejection was made for
}

// newStatusWriter returns a writer for cfg, or an error naming what is missing.
func newStatusWriter(cfg statusConfig) (*statusWriter, error) {
	switch {
	case cfg.Client == nil:
		return nil, errors.New("broker: Client is required for the status writer: it is where the statuses are written")
	case cfg.Report == nil:
		return nil, errors.New("broker: Report is required for the status writer: it is what the config source refused")
	case cfg.Conditions == nil:
		return nil, errors.New("broker: Conditions is required for the status writer: they are what the prober found")
	case cfg.Observed == nil:
		return nil, errors.New("broker: Observed is required for the status writer: it is the status that each Binding has")
	case cfg.Log == nil:
		return nil, errors.New("broker: Log is required for the status writer")
	}
	cfg.Gap, cfg.Tick, cfg.Slow, cfg.Timeout = cmp.Or(cfg.Gap, statusGap), cmp.Or(cfg.Tick, statusTick), cmp.Or(cfg.Slow, statusSlow), cmp.Or(cfg.Timeout, statusTimeout)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &statusWriter{cfg: cfg, wake: make(chan struct{}, 1), keys: map[string]*statusKey{}}, nil
}

// statusCallTimeout is how long a leader's write may take before the client stops waiting: the default, or half of the margin that the
// election leaves between the renew deadline and the lease duration when that is smaller (both are chart values), so that a write that
// the fence let through when the lease was nearly at its deadline is given up before another replica can lead. A broker with no
// election (SQLite) has the default.
func statusCallTimeout(e ElectionConfig, elected bool) time.Duration {
	if !elected {
		return statusTimeout
	}
	e = e.withDefaults()
	return max(statusFloor, min(statusTimeout, (e.LeaseDuration-e.RenewDeadline)/2))
}

// notify says that something the writer reads has moved. It never blocks, and is safe from any goroutine: it is what the hooks
// of the Prober call.
func (w *statusWriter) notify() {
	w.dirty.Store(true)
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run looks at the config, the Prober's findings and the statuses at each tick, and when it is told they moved, until ctx ends.
func (w *statusWriter) Run(ctx context.Context) {
	tick := time.NewTicker(w.cfg.Tick)
	defer tick.Stop()
	for {
		w.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-w.wake:
		}
	}
}

// reconcile is one pass: it works out what each Binding's status should say, compares it with what it says now, and writes those
// that differ and that the rules above let it write. Nothing is written while the cache of statuses has not synced.
func (w *statusWriter) reconcile(ctx context.Context) {
	rep := w.cfg.Report()
	if rep.Snapshot == nil {
		return // nothing has loaded
	}
	now := w.cfg.Now()
	var version uint64
	if w.cfg.Version != nil {
		version = w.cfg.Version()
	}
	// A pass looks at every Binding, which costs, so it is made only if something moved: the Prober said so, the report is another,
	// a Binding changed in the cache, a write that was held back is due, or the slow pass is.
	moved := w.dirty.Swap(false) || w.cfg.Version == nil || rep.Snapshot != w.lastSnap || version != w.lastVersion ||
		!now.Before(w.nextFull) || (!w.retryAt.IsZero() && !now.Before(w.retryAt))
	if !moved {
		return
	}
	w.lastSnap, w.lastVersion, w.nextFull, w.retryAt = rep.Snapshot, version, now.Add(w.cfg.Slow), time.Time{}
	rejections := map[string][]config.Rejection{}
	for _, r := range rep.Rejections {
		key := r.Namespace + "/" + r.Name
		rejections[key] = append(rejections[key], r)
	}
	served := map[string]*v1alpha1.Binding{}
	for _, rb := range rep.Snapshot.Bindings() {
		served[rb.Binding.Namespace+"/"+rb.Binding.Name] = rb.Binding
	}
	for key, rs := range rejections { // a Binding the broker holds and does not run (a second connection of a Slack app) is not served here
		if slices.ContainsFunc(rs, func(r config.Rejection) bool { return r.Reason == config.ReasonConnectionNotServed }) {
			delete(served, key)
		}
	}
	unserved := w.unservedBindings(ctx, rep)
	for key := range unserved {
		if _, isServed := served[key]; isServed || len(rejections[key]) > 0 { // a Rejection of the Binding says more
			delete(unserved, key)
		}
	}
	keys := map[string]bool{}
	for key := range rejections {
		keys[key] = true
	}
	for key := range served {
		keys[key] = true
	}
	for key := range unserved {
		keys[key] = true
	}

	have := make(map[string]crsource.BindingStatus, len(keys))
	found := make(map[string]bool, len(keys))
	for key := range keys {
		namespace, name, _ := strings.Cut(key, "/")
		cur, ok, err := w.cfg.Observed(namespace, name)
		if err != nil {
			if !w.unsynced {
				w.unsynced = true
				w.cfg.Log.Warn("binding status: the statuses of the Bindings cannot be read yet, so none is written", "error", telemetry.LogText(err))
			}
			w.lastSnap = nil // look again at the next tick
			return
		}
		have[key], found[key] = cur, ok
	}
	w.unsynced = false
	for key, rej := range unserved { // the report names a Binding and no more: the generation is the cached object's
		rej.Generation = have[key].Generation
		rejections[key] = []config.Rejection{rej}
	}

	wants := w.desire(keys, rejections, served, have, w.cfg.Conditions(), now)
	for _, key := range slices.Sorted(maps.Keys(wants)) {
		if ctx.Err() != nil {
			return
		}
		if !found[key] { // the cache holds no such Binding: it is gone, or not yet seen, and has no status to write
			continue
		}
		want, cur := wants[key], have[key]
		if want.served && string(cur.UID) != want.uid {
			continue // the cache holds another object of the name than the Snapshot serves: its status is the next reload's to say
		}
		if !want.served && cur.Generation < want.observed {
			continue // the object is older than the generation that the rejection was made for: it was made again
		}
		st := w.keys[key]
		if st == nil {
			st = &statusKey{}
			w.keys[key] = st
		}
		putBack := false
		switch {
		case sameStatus(want.conditions, cur.Conditions):
			st.desired = want.conditions
			continue
		case want.own:
			// It says what it serves, or what only it can say, and puts the status back if it says anything else.
			putBack = st.desired != nil && sameStatus(want.conditions, st.desired)
			if putBack && cur.ResourceVersion == st.rvAtWrite {
				continue // the cache is as it was when this writer wrote: it has not caught up, and so has seen no change of anyone's
			}
		case !sameStatus(want.conditions, st.desired), !hasTypes(cur.Conditions, want.conditions):
			// A reporter writes when it first sees the Binding, when its own words change, and when the status holds nothing.
		default:
			continue // and never answers back to a status that differs from the one it wrote
		}
		if now = w.cfg.Now(); now.Before(st.notBefore) {
			if w.retryAt.IsZero() || st.notBefore.Before(w.retryAt) {
				w.retryAt = st.notBefore
			}
			continue
		}
		if w.cfg.Fence != nil {
			if err := w.cfg.Fence.allowStatus("Binding.status"); err != nil {
				w.cfg.Log.Warn("binding status: a write was refused by the fence", "error", telemetry.LogText(err))
				return
			}
		}
		w.write(ctx, key, st, want.conditions, cur.ResourceVersion, putBack)
	}
	for key := range w.keys { // a Binding that is gone from the report is forgotten, as its status goes with it
		if _, ok := wants[key]; !ok {
			delete(w.keys, key)
		}
	}
}

// unservedBindings returns, by "namespace/name", a ConnectionNotServed for each Binding that names a ChatConnection that exists and that
// this broker does not serve: the connections that the report lists as left out, and the connections that the served set finds that the
// broker served before and does not now. Its message names the connection and says why. The record is read and written at every pass that moves (the Snapshot, a
// Binding or the slow pass, which comes each Slow); a failure of it is logged once until it works, and what the record could find stands. The caller leaves out the Bindings that the Snapshot serves and that a Rejection is about.
func (w *statusWriter) unservedBindings(ctx context.Context, rep config.Report) map[string]config.Rejection {
	left := rep.UnservedConnections
	if w.cfg.Served != nil {
		stopped, err := w.cfg.Served.update(ctx, rep)
		switch {
		case err != nil && !w.recordFailed:
			w.recordFailed = true
			w.cfg.Log.Warn("binding status: the served connections cannot be compared with the record in the store, or cannot be recorded; "+
				"the statuses of the Bindings are written, and this is tried again at every pass that moves and said once until it works", "error", telemetry.LogText(err))
		case err == nil:
			w.recordFailed = false
		}
		left = append(slices.Clone(left), stopped...)
	}
	out := map[string]config.Rejection{}
	for _, u := range left {
		message := fmt.Sprintf("spec.chat.connectionRef: ChatConnection %s/%s is not served by this broker: %s", u.Namespace, u.Name, u.Message)
		for _, key := range u.Admitted { // a Binding that the connection does not admit is told nothing of it
			namespace, name, _ := strings.Cut(key, "/")
			out[key] = config.Rejection{Namespace: namespace, Name: name, Reason: config.ReasonConnectionNotServed, Message: message}
		}
	}
	return out
}

// write applies want as the status of the Binding key and records what came of it. The call is cut at the timeout, so that one
// request that hangs does not hold every other Binding's write, and the fence is asked again before the next.
func (w *statusWriter) write(ctx context.Context, key string, st *statusKey, want []metav1.Condition, seenRV string, putBack bool) {
	namespace, name, _ := strings.Cut(key, "/")
	callCtx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	_, err := w.cfg.Client.Resource(bindingsGVR).Namespace(namespace).ApplyStatus(callCtx, name, statusObject(namespace, name, want),
		metav1.ApplyOptions{FieldManager: statusFieldManager, Force: true})
	if n, first := w.cfg.Warnings.take(); n > 0 && !st.stored {
		st.stored = true
		w.cfg.Log.Warn("binding status: the API server warned that the stored spec of a Binding breaks rules of its schema; this is said once for the Binding, whether or not the write was taken",
			"binding", key, "written", err == nil, "warnings_per_write", n, "first", redact.FreeTextCutLine(first, 256))
	}
	now := w.cfg.Now() // after the call: the gap is from the end of a write, not from the start of the pass
	switch {
	case err == nil:
		if putBack {
			w.reportWrite("put_back")
			w.fight(key, st, now)
		} else {
			w.reportWrite("written")
		}
		st.desired, st.rvAtWrite, st.failures, st.failed = want, seenRV, 0, false
		st.notBefore = now.Add(w.cfg.Gap)
		w.cfg.Log.Debug("binding status written", "binding", key, "put_back", putBack)
	case apierrors.IsNotFound(err): // the Binding was deleted since the config was read; the cache will drop it, and the report after it
		st.notBefore = now.Add(w.cfg.Gap) // until then it is not asked again
		w.cfg.Log.Debug("binding status: the Binding is gone", "binding", key)
	default:
		w.reportWrite("failed")
		st.failures++
		wait := w.cfg.Gap
		for i := 1; i < st.failures && wait < statusBackoffCap; i++ {
			wait *= 2
		}
		st.notBefore = now.Add(min(wait, statusBackoffCap))
		if !st.failed {
			st.failed = true
			w.cfg.Log.Warn("binding status: a write failed; it is tried again, and this is said once until one succeeds",
				"binding", key, "error", telemetry.LogText(err))
		}
	}
}

func (w *statusWriter) reportWrite(result string) {
	if w.cfg.OnWrite != nil {
		w.cfg.OnWrite(result)
	}
}

// fight counts a put-back of the Binding key at now, and says once, when it is the third in a row (each within fightWindow gaps
// of the one before), that something else keeps changing the status: the put-backs go on every gap, which a person's edit does not.
func (w *statusWriter) fight(key string, st *statusKey, now time.Time) {
	if st.putBacks > 0 && now.Sub(st.lastPutBack) <= fightWindow*w.cfg.Gap {
		st.putBacks++
	} else {
		st.putBacks, st.fought = 1, false
	}
	st.lastPutBack = now
	if st.putBacks >= fightPutBacks && !st.fought {
		st.fought = true
		w.cfg.Log.Warn("binding status: something else keeps changing the status of a Binding, and this broker keeps putting it back: "+
			"another broker that serves the same Binding, or a controller that writes its status; the two will go on at each other every gap, and this is said once while they do",
			"binding", key, "put_backs_in_a_row", st.putBacks)
	}
}

// desire works out what the status of each Binding of keys should say now: the Bindings that the source serves or rejects.
func (w *statusWriter) desire(keys map[string]bool, rejections map[string][]config.Rejection, served map[string]*v1alpha1.Binding,
	have map[string]crsource.BindingStatus, probed map[string][]bindingcheck.Condition, now time.Time) map[string]statusWant {
	out := make(map[string]statusWant, len(keys))
	at := metav1.NewTime(now.Truncate(time.Second))
	for key := range keys {
		var generation int64
		var uid string
		binding, isServed := served[key]
		if isServed {
			generation, uid = binding.Generation, string(binding.UID)
		}
		old := byType(have[key].Conditions)
		var chosen map[string]metav1.Condition // the words this writer last chose, for the dates it gave them
		if st := w.keys[key]; st != nil {
			chosen = byType(st.desired)
		}
		accepted := acceptedCondition(rejections[key], generation)
		conds := []metav1.Condition{accepted}
		var ready *bindingcheck.Condition
		if isServed { // a Binding that is not served has no Target, and what the Prober knew of it is gone or going
			groups := asksForApprovers(binding.Spec)
			expected := probedTypes
			if groups {
				expected = append(slices.Clone(probedTypes), bindingcheck.ApproversReadable)
			}
			seen := map[bindingcheck.ConditionType]bool{}
			for _, c := range probed[key] {
				switch {
				case c.Type == bindingcheck.Ready:
					ready = &c
				case c.Type == bindingcheck.ApproversReadable && !groups:
					// The Binding no longer asks for it, and the Prober has not yet seen the edit that took the groups away.
				case slices.Contains(statusTypes, c.Type):
					seen[c.Type] = true
					conds = append(conds, probedCondition(c, generation, uid, old))
				}
			}
			for _, t := range expected { // the Prober has not listed the Binding yet: those are Pending, not gone
				if !seen[t] {
					conds = append(conds, probedCondition(bindingcheck.Condition{Type: t, Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonPending, ObservedGeneration: generation, UID: uid}, generation, uid, old))
				}
			}
			if groups && !seen[bindingcheck.ApproversReadable] {
				ready = pendingOnGroups(ready, generation, uid)
			}
			if !groups {
				ready = unlessCitesGroups(ready, generation, uid)
			}
		}
		conds = append(conds, readyCondition(accepted, ready, generation, uid, old))
		if last := len(conds) - 1; conds[last].Status == metav1.ConditionTrue && groupsPending(conds) {
			// The takeover rule gave back a Ready that the last status said was True, but that status had no ApproversReadable, and the
			// one written now is not known: the Binding is not Ready on a word that did not ask about its groups.
			conds[last] = metav1.Condition{
				Type: string(bindingcheck.Ready), Status: metav1.ConditionUnknown, ObservedGeneration: generation,
				Reason: string(bindingcheck.ApproversReadable) + ":" + string(bindingcheck.ReasonPending),
			}
		}
		for i := range conds {
			conds[i].LastTransitionTime = transitionTime(conds[i], old, chosen, at)
		}
		slices.SortFunc(conds, func(a, b metav1.Condition) int { return cmp.Compare(a.Type, b.Type) })
		out[key] = statusWant{conditions: conds, served: isServed, own: isServed || !othersToo(rejections[key]), uid: uid, observed: accepted.ObservedGeneration}
	}
	return out
}

// groupsPending reports whether conds holds an ApproversReadable that is Unknown and Pending.
func groupsPending(conds []metav1.Condition) bool {
	return slices.ContainsFunc(conds, func(c metav1.Condition) bool {
		return c.Type == string(bindingcheck.ApproversReadable) && c.Status == metav1.ConditionUnknown && c.Reason == string(bindingcheck.ReasonPending)
	})
}

// pendingOnGroups is the Ready of a Binding that asks for approvals from groups whose ApproversReadable the Prober has not yet listed
// (it has not seen the edit that added them), where ready is the Ready it derived without the condition: not ready on that word. A Ready
// that is False stays so, since the Binding is known not to be ready, and so does one that is not yet known (nil, or Pending). Any other is
// Unknown, Pending on ApproversReadable, as the Prober's own Ready is while a first probe has not ended.
func pendingOnGroups(ready *bindingcheck.Condition, generation int64, uid string) *bindingcheck.Condition {
	if ready == nil || ready.Status == bindingcheck.False || ready.Pending() {
		return ready
	}
	return &bindingcheck.Condition{
		Type: bindingcheck.Ready, Status: bindingcheck.Unknown, ObservedGeneration: generation, UID: uid,
		Reason: bindingcheck.Reason(string(bindingcheck.ApproversReadable) + ":" + string(bindingcheck.ReasonPending)),
	}
}

// unlessCitesGroups is the Ready of a Binding that asks for no approver groups, where ready is the Prober's: if the Prober has not yet seen
// the edit that took the groups away, its Ready may still be held back by ApproversReadable (its reason starts with "ApproversReadable:"),
// with a message about a group the Binding no longer has, and that is not copied to a status that has no such condition. It is not known
// until the Prober has seen the edit, so it is Unknown and Pending at the generation served. Any other Ready is the Prober's.
func unlessCitesGroups(ready *bindingcheck.Condition, generation int64, uid string) *bindingcheck.Condition {
	if ready == nil || !strings.HasPrefix(string(ready.Reason), string(bindingcheck.ApproversReadable)+":") {
		return ready
	}
	return &bindingcheck.Condition{Type: bindingcheck.Ready, Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonPending, ObservedGeneration: generation, UID: uid}
}

// acceptedCondition composes the Accepted condition of a Binding from its Rejections, and generation is the one the source
// serves (zero if it serves none). The reason is that of the first Rejection in acceptedReasonOrder, and the message holds those
// of all of them in that order and then by text, so that it does not depend on the order the source listed them in. The
// message of a ChannelConflict does not name a Binding of another namespace, which the tenant has no right to know of; the log
// keeps the whole text.
func acceptedCondition(rejections []config.Rejection, generation int64) metav1.Condition {
	c := metav1.Condition{Type: string(conditionAccepted), ObservedGeneration: generation}
	if len(rejections) == 0 {
		c.Status, c.Reason = metav1.ConditionTrue, string(bindingcheck.ReasonOK)
		return c
	}
	rank := func(r config.RejectionReason) int {
		if i := slices.Index(acceptedReasonOrder, r); i >= 0 {
			return i
		}
		return len(acceptedReasonOrder)
	}
	sorted := slices.Clone(rejections)
	slices.SortStableFunc(sorted, func(a, b config.Rejection) int {
		return cmp.Or(cmp.Compare(rank(a.Reason), rank(b.Reason)), cmp.Compare(a.Message, b.Message))
	})
	c.Status, c.Reason, c.ObservedGeneration = metav1.ConditionFalse, reasonRejected, 0
	if rank(sorted[0].Reason) < len(acceptedReasonOrder) {
		c.Reason = string(sorted[0].Reason)
	}
	messages := make([]string, 0, len(sorted))
	for _, r := range sorted {
		c.ObservedGeneration = max(c.ObservedGeneration, r.Generation)
		msg := ownNamespaceText(r)
		if msg != "" && !slices.Contains(messages, msg) {
			messages = append(messages, msg)
		}
	}
	if c.ObservedGeneration == 0 {
		c.ObservedGeneration = generation
	}
	c.Message = statusMessage(strings.Join(messages, "; "))
	return c
}

// ownNamespaceText is the message of a Rejection as the Binding's own namespace may read it: where it names the Binding that holds
// the channel, and that Binding is in another namespace, it says "a Binding in another namespace".
func ownNamespaceText(r config.Rejection) string {
	if r.Reason != config.ReasonChannelConflict {
		return r.Message
	}
	m := boundBy.FindStringSubmatchIndex(r.Message)
	if m == nil { // the wording of resolution moved: say nothing of the winner, rather than say what may be another namespace's
		return "spec.chat.channel: the channel is already bound by another Binding"
	}
	if r.Message[m[2]:m[3]] == r.Namespace {
		return r.Message
	}
	return r.Message[:m[0]] + "already bound by a Binding in another namespace" + r.Message[m[1]:]
}

// probedCondition is the condition of the Prober c as a condition of the status of a Binding served at generation, and with uid.
// A Pending one is replaced by what the status already says of that type if that is of the generation served (the takeover rule;
// one of a generation above it is of another object, since a generation never goes down), and otherwise is what it is: not known,
// as of the generation served. A condition that the Prober made for
// another object (a Binding of the name that was deleted and made again) or for a generation above the one served is no finding
// about this one, and is Pending the same way.
func probedCondition(c bindingcheck.Condition, generation int64, uid string, old map[string]metav1.Condition) metav1.Condition {
	if !c.Pending() && (c.ObservedGeneration > generation || c.UID != uid) {
		c = bindingcheck.Condition{Type: c.Type, Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonPending}
	}
	out := metav1.Condition{
		Type: string(c.Type), Status: metav1.ConditionStatus(c.Status), Reason: string(c.Reason), Message: statusMessage(c.Message),
		ObservedGeneration: c.ObservedGeneration, LastTransitionTime: metav1.NewTime(c.LastTransitionTime.Truncate(time.Second)),
	}
	if !c.Pending() {
		return out
	}
	if was, ok := old[out.Type]; ok && was.ObservedGeneration == generation && !strings.HasPrefix(was.Reason, acceptedPrefix) {
		return was
	}
	out.ObservedGeneration = generation
	return out
}

// readyCondition is the Ready condition of a Binding: False with "Accepted:<Reason>" if it is not accepted, else the
// Prober's (ready, nil before the Prober has seen the Binding, when it is Pending), by the takeover rule as probedCondition has
// it. A Ready that a rejection wrote is never carried over to a Binding that is accepted now.
func readyCondition(accepted metav1.Condition, ready *bindingcheck.Condition, generation int64, uid string, old map[string]metav1.Condition) metav1.Condition {
	if accepted.Status != metav1.ConditionTrue {
		return metav1.Condition{
			Type: string(bindingcheck.Ready), Status: metav1.ConditionFalse, Reason: acceptedPrefix + accepted.Reason,
			Message: accepted.Message, ObservedGeneration: accepted.ObservedGeneration,
		}
	}
	if ready == nil {
		ready = &bindingcheck.Condition{Type: bindingcheck.Ready, Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonPending, ObservedGeneration: generation, UID: uid}
	}
	return probedCondition(*ready, generation, uid, old)
}

// transitionTime is when c's status last changed: the time the status already holds for the type when its status is the same, else
// the time this writer gave the same status of the type when it last wrote (so that a status that has not been seen to change,
// as with a watch that is behind, is not dated anew at each write), else now (at) when the status differs from the status's, so that
// a condition that heals is not dated before the failure it healed; and for a type the status does not hold, the time the
// Prober gave it, else now.
func transitionTime(c metav1.Condition, old, chosen map[string]metav1.Condition, at metav1.Time) metav1.Time {
	if was, ok := old[c.Type]; ok {
		if was.Status == c.Status && !was.LastTransitionTime.IsZero() {
			return was.LastTransitionTime
		}
		if mine, ok := chosen[c.Type]; ok && mine.Status == c.Status && !mine.LastTransitionTime.IsZero() {
			return mine.LastTransitionTime
		}
		return at
	}
	if !c.LastTransitionTime.IsZero() {
		return c.LastTransitionTime
	}
	return at
}

// statusMessage redacts a message for a status, which anyone who can read the Binding reads, and cuts it to one line of at most
// maxStatusMessage bytes, with a mark where it was cut. It cuts after it redacts, so that no cut shows the start of a secret.
func statusMessage(s string) string {
	if s == "" {
		return ""
	}
	text := redact.FreeTextLine(s)
	if len(text) <= maxStatusMessage {
		return text
	}
	cut := maxStatusMessage - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + ellipsis
}

// byType maps each condition to its type.
func byType(conds []metav1.Condition) map[string]metav1.Condition {
	out := make(map[string]metav1.Condition, len(conds))
	for _, c := range conds {
		out[c.Type] = c
	}
	return out
}

// othersToo reports whether the rejections of a Binding are ones that another broker can make of it too: its spec is refused
// (a refused edit is owned by the connection it names and by the one the last good generation names), or it names a ChatConnection
// that does not exist (every broker owns a Binding of that), or it names a connection that exists and that this broker does not serve
// (ConnectionNotServed: a broker that does serve it has the last word). Any other reason is of a Binding that names a connection that
// exists, which only that connection's broker owns.
func othersToo(rejections []config.Rejection) bool {
	return slices.ContainsFunc(rejections, func(r config.Rejection) bool {
		return r.Reason == config.ReasonInvalidSpec || r.Reason == config.ReasonConnectionNotFound || r.Reason == config.ReasonConnectionNotServed
	})
}

// restrict keeps the conditions of types, sorted by type.
func restrict(conds []metav1.Condition, types map[string]bool) []metav1.Condition {
	var out []metav1.Condition
	for _, c := range conds {
		if types[c.Type] {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b metav1.Condition) int { return cmp.Compare(a.Type, b.Type) })
	return out
}

// typesOf is the set of types of conds.
func typesOf(lists ...[]metav1.Condition) map[string]bool {
	out := map[string]bool{}
	for _, conds := range lists {
		for _, c := range conds {
			out[c.Type] = true
		}
	}
	return out
}

// hasTypes reports whether cur holds any condition of a type that want has.
func hasTypes(cur, want []metav1.Condition) bool { return len(restrict(cur, typesOf(want))) > 0 }

// sameStatus reports whether the status cur says what want says: every field but the time of the transition, which is kept from the
// cluster when the status is the same, and so is no part of what they say. It looks at the types that want writes and at no others:
// a condition of another type that someone else wrote is not this writer's to compare or to remove, and the types that this writer
// wrote and no longer says are removed by the write that changes the words (a Binding that stops being served is not accepted, so
// its Accepted changes with them).
func sameStatus(want, cur []metav1.Condition) bool {
	types := typesOf(want)
	return slices.EqualFunc(restrict(want, types), restrict(cur, types), func(x, y metav1.Condition) bool {
		return x.Type == y.Type && x.Status == y.Status && x.Reason == y.Reason && x.Message == y.Message && x.ObservedGeneration == y.ObservedGeneration
	})
}

// statusObject is the body of the apply: the kind and the name, and the conditions, and no spec. Each condition has message,
// reason and lastTransitionTime, which metav1.Condition requires (a Condition with no message is not "no message" to the schema,
// but a refusal), so message is there even when it is empty. An observedGeneration of zero is left out, as a Binding of a file has none.
func statusObject(namespace, name string, conds []metav1.Condition) *unstructured.Unstructured {
	items := make([]any, 0, len(conds))
	for _, c := range conds {
		item := map[string]any{
			"type": c.Type, "status": string(c.Status), "reason": c.Reason, "message": c.Message,
			"lastTransitionTime": c.LastTransitionTime.UTC().Format(time.RFC3339),
		}
		if c.ObservedGeneration > 0 {
			item["observedGeneration"] = c.ObservedGeneration
		}
		items = append(items, item)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.Group + "/" + v1alpha1.Version,
		"kind":       config.KindBinding,
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"status":     map[string]any{"conditions": items},
	}}
}
