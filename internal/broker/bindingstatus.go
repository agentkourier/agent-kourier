package broker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// The status writer is the half of the custom resource config source (spec D50) that tells each Binding's owner what became of it:
// it writes Binding.status.conditions, from the leader only, and only with the crds config source. It reads two things the
// broker already has: what the source refused (config.Report), and what the Prober found (bindingcheck.State). It never reads
// the spec.

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
	// The defaults of statusConfig's timings. A Binding's status is written at most once each statusGap; the writer looks at
	// the config once each statusTick, and reads every Binding's status from the cluster once each statusResync.
	statusGap    = 10 * time.Second
	statusTick   = time.Second
	statusResync = 10 * time.Minute
	// statusListRetry is the wait before the next try of a list that failed, and statusBackoffCap the longest wait after
	// failed writes of one Binding (it starts at the gap and doubles).
	statusListRetry  = 10 * time.Second
	statusBackoffCap = 5 * time.Minute

	// conditionAccepted is the condition that the writer composes from what the source refused: the probes of the Prober have
	// no part in it, and a Binding that is not served has no Target for them.
	conditionAccepted bindingcheck.ConditionType = "Accepted"
	// reasonRejected is the reason of an Accepted condition whose reason the config package has not named here: a reason
	// outside the closed set is a reason that no one wrote a rule for, and still fits a condition's reason pattern.
	reasonRejected = "Rejected"
	// acceptedPrefix starts the reason of a Ready condition that is False because the Binding is not accepted.
	acceptedPrefix = "Accepted:"
)

// statusTypes are the condition types that the writer owns: it writes them and no others, and a type of the same Binding that
// someone else wrote is left alone. It is what a Prober's State can hold, and Accepted.
var statusTypes = append(bindingcheck.Types(), conditionAccepted)

// acceptedReasonOrder is the order in which the reasons of the Rejections of one Binding give way to one: the first of them that
// the Binding has is its Accepted reason, and the messages of all follow in the same order. It is the order of the spec's row for
// the condition, and puts what is wrong with the object itself before what is wrong with the objects it names, and the
// channel, which a Binding can lose only if it resolved, last.
var acceptedReasonOrder = []config.RejectionReason{
	config.ReasonInvalidSpec, config.ReasonBackendNotFound, config.ReasonBackendRejected, config.ReasonConnectionNotFound,
	config.ReasonConnectionRejected, config.ReasonNamespaceNotAllowed, config.ReasonChannelConflict,
}

var bindingsGVR = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "bindings"}

// warningCounter is the rest.WarningHandler of the status writer's client. A write to a status runs every rule of the
// schema, those of the spec included, and the API server answers one Warning header for each rule that the stored spec
// breaks (the rules are ratcheted, so the write itself passes): for an object that was stored invalid that is a line for every
// write, and the client-go default would log each. The counter keeps the number and the first text, and logs nothing.
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
// counter of its WarningHandler when Run made the client (nil for one a test gave).
type statusSink struct {
	client   dynamic.Interface
	warnings *warningCounter
}

// statusConfig is what a statusWriter is built from.
type statusConfig struct {
	// Client writes the statuses and lists the Bindings. It is the status client (InClusterStatusClient), whose rate limiter
	// is its own.
	Client dynamic.Interface
	// Report is what the config source knows besides the snapshot (Reloader.Report), and Conditions is what the Prober found,
	// by Binding (State.Snapshot). Both required.
	Report     func() config.Report
	Conditions func() map[string][]bindingcheck.Condition
	// Fence, when set, is checked before each write: a leader whose lease is past its renew deadline writes nothing. Nil with
	// SQLite, which has no lease.
	Fence *leaseFence
	// Warnings is the counter of the client's WarningHandler, nil if the client is not one that counts.
	Warnings *warningCounter
	Log      *slog.Logger
	// OnWrite is told the result of each write, "written" or "failed", for a metric. Nil means none.
	OnWrite func(result string)
	// Gap, Tick and Resync default to statusGap, statusTick and statusResync; a test passes less. Now is time.Now if nil.
	Gap, Tick, Resync time.Duration
	Now               func() time.Time
}

// statusWriter writes Binding.status.conditions with Server-Side Apply on the status subresource. It is built and run by the
// leader half only (leaderHalf.start), so that a standby writes nothing, and a deposed leader is stopped by the fence before a
// write. It has no queue to fill: the hooks of the Prober say that something moved (notify), and a pass over every Binding
// compares what its status should say with what it was last known to say, and writes the difference.
//
// # What a Binding's status says
//
// Accepted is composed here from the Rejections of the report: True when there are none, else False with one reason chosen by
// acceptedReasonOrder and every message of the Binding. BackendReachable, ChannelJoined and ApproversReadable are the Prober's,
// for a Binding the source serves, copied as they are. Ready is the Prober's, except that a Binding that is not accepted is
// not ready: False, with the reason "Accepted:<Reason>". Each condition has an observedGeneration: a probed one the generation
// its pass began at, and Accepted the newest generation that was looked at (the one that was refused, if there is one). No time
// of the last probe is written, since a status that changes every few minutes is watch traffic for everyone who watches Bindings.
//
// # When it writes
//
// Only when the status differs from what the cluster holds, and for one Binding at most once each Gap. A write that fails is
// tried again after a wait that doubles. Three rules decide a difference:
//
//   - A Binding this writer has not seen is compared with the cluster's status, read by a list when the writer starts and again
//     each Resync: a new leader sets right a status that the previous one left (it may say what that leader had in memory, as a
//     served generation), and writes nothing when the status is already what it would say.
//   - A new leader starts every probed condition at Pending, and writing that would blank a good status at each failover. A
//     Pending condition is therefore replaced by what the status already says, unless the status says it of an older generation
//     than the one served, or says nothing: then Pending is the truth, and is written.
//   - A Binding this writer has seen is written again only when what it should say changed, or when the cluster holds no
//     conditions for it at all (it was deleted and made again, which a status of the same words cannot tell from the old).
//
// # Two writers
//
// The fence leaves one writer at a time among the replicas of a broker, and its fixed field manager makes the next leader's write
// an update, so the second leader's view replaces the first's: that is right, because each says what it serves. Two brokers can
// both report one Binding only when it names no ChatConnection that exists, and each says the same, except where one of
// them holds a last good generation in memory that the other lacks. They then do not fight: neither rewrites a status that
// differs from the one it wrote while its own words stay the same, so each writes once and the last writer's words stay until
// either's change. That is the whole rule. No broker ranks another's report, since none knows which of them serves.
type statusWriter struct {
	cfg  statusConfig
	wake chan struct{}
	// dirty is set by notify and read by the loop: something the Prober found has moved since the last pass.
	dirty atomic.Bool

	// What follows belongs to the goroutine that runs reconcile.
	observed   map[string][]metav1.Condition // the owned conditions of each Binding as the cluster holds them, by namespace/name
	synced     bool                          // observed has been listed once
	nextResync time.Time
	lastSnap   *config.Snapshot // the snapshot of the last pass
	waiting    bool             // a Binding's write is held back by its gap or backoff: look again at the next tick
	keys       map[string]*statusKey
}

// statusKey is what the writer keeps for one Binding.
type statusKey struct {
	desired   []metav1.Condition // what its status last was found or made to say
	seen      bool               // desired has been set
	notBefore time.Time          // no write before this
	failures  int
	failed    bool // the current run of failures has been logged
	stored    bool // a warning about the stored spec has been logged
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
	case cfg.Log == nil:
		return nil, errors.New("broker: Log is required for the status writer")
	}
	cfg.Gap, cfg.Tick, cfg.Resync = cmp.Or(cfg.Gap, statusGap), cmp.Or(cfg.Tick, statusTick), cmp.Or(cfg.Resync, statusResync)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &statusWriter{cfg: cfg, wake: make(chan struct{}, 1), observed: map[string][]metav1.Condition{}, keys: map[string]*statusKey{}}, nil
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

// Run looks at the config and the Prober's findings at each tick, and when it is told they moved, until ctx ends.
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

// reconcile is one pass: it brings the list of what the cluster holds up to date when that is due, works out what each Binding's
// status should say, and writes those that differ. Nothing is written before the first list has succeeded, since a writer that
// does not know what the cluster holds would write Pending over a good status.
func (w *statusWriter) reconcile(ctx context.Context) {
	now := w.cfg.Now()
	listed := false
	if !now.Before(w.nextResync) { // zero before the first list, which is made at once
		if err := w.refresh(ctx); err != nil {
			w.nextResync = now.Add(statusListRetry)
			w.cfg.Log.Warn("binding status: listing the Bindings failed", "error", telemetry.LogText(err))
		} else {
			w.synced, listed, w.nextResync = true, true, now.Add(w.cfg.Resync)
		}
	}
	if !w.synced {
		return
	}
	rep := w.cfg.Report()
	if rep.Snapshot == nil {
		return // nothing has loaded
	}
	if changed := w.dirty.Swap(false); !changed && !listed && !w.waiting && rep.Snapshot == w.lastSnap {
		return
	}
	w.lastSnap, w.waiting = rep.Snapshot, false

	desired := w.desire(rep, w.cfg.Conditions(), now)
	for _, key := range slices.Sorted(maps.Keys(desired)) {
		if ctx.Err() != nil {
			return
		}
		want := desired[key]
		st := w.keys[key]
		if st == nil {
			st = &statusKey{}
			w.keys[key] = st
		}
		have := w.observed[key]
		switch {
		case !st.seen, !equalConditions(want, st.desired):
			if equalConditions(want, have) { // already what it says: someone, or this writer before, wrote it
				st.desired, st.seen = want, true
				continue
			}
		case len(have) == 0: // the cluster holds none, though the writer has written: the object was made again
		default:
			continue
		}
		if now.Before(st.notBefore) {
			w.waiting = true
			continue
		}
		if w.cfg.Fence != nil {
			if err := w.cfg.Fence.allowStatus("Binding.status"); err != nil {
				w.cfg.Log.Warn("binding status: a write was refused by the fence", "error", telemetry.LogText(err))
				return
			}
		}
		w.write(ctx, key, st, want, now)
	}
	for key := range w.keys { // a Binding that is gone from the report is forgotten, as its status goes with it
		if _, ok := desired[key]; !ok {
			delete(w.keys, key)
		}
	}
}

// write applies want as the status of the Binding key and records what came of it.
func (w *statusWriter) write(ctx context.Context, key string, st *statusKey, want []metav1.Condition, now time.Time) {
	namespace, name, _ := strings.Cut(key, "/")
	out, err := w.cfg.Client.Resource(bindingsGVR).Namespace(namespace).ApplyStatus(ctx, name, statusObject(namespace, name, want),
		metav1.ApplyOptions{FieldManager: statusFieldManager, Force: true})
	if n, first := w.cfg.Warnings.take(); n > 0 && !st.stored {
		st.stored = true
		w.cfg.Log.Warn("binding status: the API server warned that the stored spec of a Binding breaks rules of its schema; the status is written all the same, and this is said once for the Binding",
			"binding", key, "warnings_per_write", n, "first", redact.FreeTextCutLine(first, 256))
	}
	switch {
	case err == nil:
		w.reportWrite(true)
		st.desired, st.seen, st.failures, st.failed = want, true, 0, false
		st.notBefore = now.Add(w.cfg.Gap)
		if got := ownedConditions(conditionsOf(out)); len(got) > 0 {
			w.observed[key] = got
		} else {
			w.observed[key] = want
		}
		w.cfg.Log.Debug("binding status written", "binding", key)
	case apierrors.IsNotFound(err): // the Binding was deleted since the config was read; the report will not have it
		delete(w.observed, key)
		w.cfg.Log.Debug("binding status: the Binding is gone", "binding", key)
	default:
		w.reportWrite(false)
		st.failures++
		wait := w.cfg.Gap
		for i := 1; i < st.failures && wait < statusBackoffCap; i++ {
			wait *= 2
		}
		st.notBefore, w.waiting = now.Add(min(wait, statusBackoffCap)), true
		if !st.failed {
			st.failed = true
			w.cfg.Log.Warn("binding status: a write failed; it is tried again, and this is said once until one succeeds",
				"binding", key, "error", telemetry.LogText(err))
		}
	}
}

func (w *statusWriter) reportWrite(ok bool) {
	if w.cfg.OnWrite == nil {
		return
	}
	if ok {
		w.cfg.OnWrite("written")
		return
	}
	w.cfg.OnWrite("failed")
}

// refresh lists the owned conditions of every Binding of the cluster, as the cluster holds them now.
func (w *statusWriter) refresh(ctx context.Context) error {
	observed := map[string][]metav1.Condition{}
	opts := metav1.ListOptions{Limit: 500}
	for {
		list, err := w.cfg.Client.Resource(bindingsGVR).List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list Bindings: %w", err)
		}
		for i := range list.Items {
			u := &list.Items[i]
			observed[u.GetNamespace()+"/"+u.GetName()] = ownedConditions(conditionsOf(u))
		}
		if list.GetContinue() == "" {
			break
		}
		opts.Continue = list.GetContinue()
	}
	w.observed = observed
	return nil
}

// desire works out what the status of each Binding of rep should say now. A Binding is in it if the source serves it or rejects
// it; one that is in neither has no status to write.
func (w *statusWriter) desire(rep config.Report, probed map[string][]bindingcheck.Condition, now time.Time) map[string][]metav1.Condition {
	rejections := map[string][]config.Rejection{}
	for _, r := range rep.Rejections {
		key := r.Namespace + "/" + r.Name
		rejections[key] = append(rejections[key], r)
	}
	served := map[string]int64{}
	for _, rb := range rep.Snapshot.Bindings() {
		served[rb.Binding.Namespace+"/"+rb.Binding.Name] = rb.Binding.Generation
	}
	out := map[string][]metav1.Condition{}
	for key := range rejections {
		out[key] = nil
	}
	for key := range served {
		out[key] = nil
	}
	at := metav1.NewTime(now.Truncate(time.Second))
	for key := range out {
		generation, isServed := served[key]
		old := byType(w.observed[key])
		accepted := acceptedCondition(rejections[key], generation)
		conds := []metav1.Condition{accepted}
		var ready *bindingcheck.Condition
		if isServed { // a Binding that is not served has no Target, and what the Prober knew of it is gone or going
			for _, c := range probed[key] {
				switch {
				case c.Type == bindingcheck.Ready:
					ready = &c
				case slices.Contains(statusTypes, c.Type):
					conds = append(conds, probedCondition(c, generation, old))
				}
			}
		}
		conds = append(conds, readyCondition(accepted, ready, generation, old))
		for i := range conds {
			conds[i].LastTransitionTime = transitionTime(conds[i], old, at)
		}
		slices.SortFunc(conds, func(a, b metav1.Condition) int { return cmp.Compare(a.Type, b.Type) })
		out[key] = conds
	}
	return out
}

// acceptedCondition composes the Accepted condition of a Binding from its Rejections, and generation is the one the source
// serves (zero if it serves none). The reason is that of the first Rejection in acceptedReasonOrder, and the message holds those
// of all of them in that order and then by text, so that it does not depend on the order the source listed them in.
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
		if r.Message != "" && !slices.Contains(messages, r.Message) {
			messages = append(messages, r.Message)
		}
	}
	if c.ObservedGeneration == 0 {
		c.ObservedGeneration = generation
	}
	c.Message = statusMessage(strings.Join(messages, "; "))
	return c
}

// probedCondition is the condition of the Prober c as a condition of the status of a Binding served at generation. A Pending one
// is replaced by what the status already says of that type if that is of the generation served or a newer one (the takeover
// rule), and otherwise is what it is: not known, as of the generation served.
func probedCondition(c bindingcheck.Condition, generation int64, old map[string]metav1.Condition) metav1.Condition {
	out := metav1.Condition{
		Type: string(c.Type), Status: metav1.ConditionStatus(c.Status), Reason: string(c.Reason), Message: statusMessage(c.Message),
		ObservedGeneration: c.ObservedGeneration, LastTransitionTime: metav1.NewTime(c.LastTransitionTime.Truncate(time.Second)),
	}
	if !c.Pending() {
		return out
	}
	if was, ok := old[out.Type]; ok && was.ObservedGeneration >= generation && !strings.HasPrefix(was.Reason, acceptedPrefix) {
		return was
	}
	out.ObservedGeneration = generation
	return out
}

// readyCondition is the Ready condition of a Binding: False with "Accepted:<Reason>" if it is not accepted, else the
// Prober's (ready, nil before the Prober has seen the Binding, when it is Pending), by the takeover rule as probedCondition has
// it. A Ready that a rejection wrote is never carried over to a Binding that is accepted now.
func readyCondition(accepted metav1.Condition, ready *bindingcheck.Condition, generation int64, old map[string]metav1.Condition) metav1.Condition {
	if accepted.Status != metav1.ConditionTrue {
		return metav1.Condition{
			Type: string(bindingcheck.Ready), Status: metav1.ConditionFalse, Reason: acceptedPrefix + accepted.Reason,
			Message: accepted.Message, ObservedGeneration: accepted.ObservedGeneration,
		}
	}
	if ready == nil {
		ready = &bindingcheck.Condition{Type: bindingcheck.Ready, Status: bindingcheck.Unknown, Reason: bindingcheck.ReasonPending, ObservedGeneration: generation}
	}
	return probedCondition(*ready, generation, old)
}

// transitionTime is when c's status last changed: when the status already holds the same status for the type, that time, else
// the time the Prober gave it, else at.
func transitionTime(c metav1.Condition, old map[string]metav1.Condition, at metav1.Time) metav1.Time {
	if was, ok := old[c.Type]; ok && was.Status == c.Status && !was.LastTransitionTime.IsZero() {
		return was.LastTransitionTime
	}
	if !c.LastTransitionTime.IsZero() {
		return c.LastTransitionTime
	}
	return at
}

// statusMessage redacts a message for a status, which anyone who can read the Binding reads, and cuts it to one line of at
// most maxStatusMessage bytes.
func statusMessage(s string) string { return redact.FreeTextCutLine(s, maxStatusMessage) }

// byType maps each condition to its type.
func byType(conds []metav1.Condition) map[string]metav1.Condition {
	out := make(map[string]metav1.Condition, len(conds))
	for _, c := range conds {
		out[c.Type] = c
	}
	return out
}

// ownedConditions keeps the conditions the writer owns, sorted by type.
func ownedConditions(conds []metav1.Condition) []metav1.Condition {
	var out []metav1.Condition
	for _, c := range conds {
		if slices.ContainsFunc(statusTypes, func(t bindingcheck.ConditionType) bool { return string(t) == c.Type }) {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b metav1.Condition) int { return cmp.Compare(a.Type, b.Type) })
	return out
}

// equalConditions reports whether two lists, each sorted by type, say the same: every field but the time of the transition,
// which is kept from the cluster when the status is the same, and so is no part of what they say.
func equalConditions(a, b []metav1.Condition) bool {
	return slices.EqualFunc(ownedConditions(a), ownedConditions(b), func(x, y metav1.Condition) bool {
		return x.Type == y.Type && x.Status == y.Status && x.Reason == y.Reason && x.Message == y.Message && x.ObservedGeneration == y.ObservedGeneration
	})
}

// conditionsOf reads status.conditions of an object, and none if it has no readable ones.
func conditionsOf(u *unstructured.Unstructured) []metav1.Condition {
	if u == nil {
		return nil
	}
	raw, _, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil {
		return nil
	}
	var out []metav1.Condition
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var c metav1.Condition
		if runtime.DefaultUnstructuredConverter.FromUnstructured(m, &c) == nil {
			out = append(out, c)
		}
	}
	return out
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
