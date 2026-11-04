package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// statusApply is one apply of a status that the fake API server took.
type statusApply struct {
	Namespace, Name, Subresource string
	Options                      metav1.PatchOptions
	Body                         map[string]any
}

// conds returns the conditions of the body, by type.
func (a statusApply) conds() map[string]map[string]any {
	out := map[string]map[string]any{}
	status, _ := a.Body["status"].(map[string]any)
	items, _ := status["conditions"].([]any)
	for _, it := range items {
		m := it.(map[string]any)
		out[m["type"].(string)] = m
	}
	return out
}

// statusCluster is a fake API server that holds Bindings and takes an apply of their status as the real one does for the one thing
// the writer asks of it: the conditions in the body replace the ones the object had (every condition is owned by the one
// manager). It records each apply as it was sent.
type statusCluster struct {
	dyn *dynamicfake.FakeDynamicClient

	mu      sync.Mutex
	applies []statusApply
	fail    func(statusApply) error // an error to answer an apply with, nil to take it
	warn    *warningCounter         // told warnN Warnings with each apply it takes
	warnN   int
	applied chan struct{} // told, without waiting, of each apply taken: for a test that runs a whole broker

	// The informer's cache, as the writer reads it: the objects as they are, unless unsynced is set (the cache has not synced) or
	// held holds what the cache said when it was frozen (it has not yet seen what was written since).
	unsynced error
	held     map[string]crsource.BindingStatus
	managed  map[string]map[string]bool // the condition types that each Binding's last apply of the writer's manager held: SSA owns those and no others
}

func newStatusCluster(t *testing.T, objs ...*unstructured.Unstructured) *statusCluster {
	t.Helper()
	rt := make([]runtime.Object, len(objs))
	for i, o := range objs {
		rt[i] = o
	}
	return newStatusClusterOn(crClusterOf(rt...))
}

// newStatusClusterOn makes dyn, a fake cluster that holds Bindings, take applies of their status as statusCluster says.
func newStatusClusterOn(dyn *dynamicfake.FakeDynamicClient) *statusCluster {
	c := &statusCluster{dyn: dyn, applied: make(chan struct{}, 256)}
	c.dyn.PrependReactor("patch", "bindings", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pa := a.(k8stesting.PatchActionImpl)
		if pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		ap := statusApply{Namespace: pa.GetNamespace(), Name: pa.GetName(), Subresource: pa.GetSubresource(), Options: pa.PatchOptions}
		if err := utiljson.Unmarshal(pa.GetPatch(), &ap.Body); err != nil {
			return true, nil, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		defer func() { // after the object holds the status, so a test that waits for it finds it
			select {
			case c.applied <- struct{}{}:
			default:
			}
		}()
		c.applies = append(c.applies, ap)
		if c.fail != nil {
			if err := c.fail(ap); err != nil {
				return true, nil, err
			}
		}
		for range c.warnN {
			c.warn.HandleWarningHeader(299, "kube-apiserver", "spec.chat.triggers: a rule of the stored spec is broken")
		}
		obj, err := c.dyn.Tracker().Get(crBindings, ap.Namespace, ap.Name)
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		// Server-Side Apply replaces what the manager owns, which is what its last apply held, and keeps the conditions of others.
		key := ap.Namespace + "/" + ap.Name
		if c.managed == nil {
			c.managed = map[string]map[string]bool{}
		}
		body := ap.conds()
		kept := []any{}
		for _, existing := range crsource.ConditionsOf(u) {
			if _, replaced := body[existing.Type]; !replaced && !c.managed[key][existing.Type] {
				m := map[string]any{"type": existing.Type, "status": string(existing.Status), "reason": existing.Reason, "message": existing.Message,
					"lastTransitionTime": existing.LastTransitionTime.UTC().Format(time.RFC3339)}
				if existing.ObservedGeneration > 0 {
					m["observedGeneration"] = existing.ObservedGeneration
				}
				kept = append(kept, m)
			}
		}
		held := map[string]bool{}
		status, _ := ap.Body["status"].(map[string]any)
		items, _ := status["conditions"].([]any)
		for _, it := range items {
			kept = append(kept, it)
			held[it.(map[string]any)["type"].(string)] = true
		}
		c.managed[key] = held
		u.Object["status"] = map[string]any{"conditions": kept}
		if err := c.dyn.Tracker().Update(crBindings, u, ap.Namespace); err != nil {
			return true, nil, err
		}
		return true, u, nil
	})
	return c
}

// observed is what the Binding informer's cache holds of the status of the Binding namespace/name: statusConfig.Observed.
func (c *statusCluster) observed(namespace, name string) (crsource.BindingStatus, bool, error) {
	c.mu.Lock()
	err, held := c.unsynced, c.held
	c.mu.Unlock()
	if err != nil {
		return crsource.BindingStatus{}, false, err
	}
	if st, ok := held[namespace+"/"+name]; ok {
		return st, true, nil
	}
	obj, err := c.dyn.Tracker().Get(crBindings, namespace, name)
	if apierrors.IsNotFound(err) {
		return crsource.BindingStatus{}, false, nil
	}
	if err != nil {
		return crsource.BindingStatus{}, false, err
	}
	u := obj.(*unstructured.Unstructured)
	return crsource.BindingStatus{UID: u.GetUID(), Generation: u.GetGeneration(), Conditions: crsource.ConditionsOf(u)}, true, nil
}

// freeze makes the cache say what it says now of the Bindings key, however the cluster changes, until thaw: a cache that the watch has not reached.
func (c *statusCluster) freeze(t *testing.T, keys ...string) {
	t.Helper()
	held := map[string]crsource.BindingStatus{}
	for _, key := range keys {
		ns, name, _ := strings.Cut(key, "/")
		st, ok, err := c.observed(ns, name)
		if err != nil || !ok {
			t.Fatalf("freeze %s: %v %v", key, ok, err)
		}
		held[key] = st
	}
	c.mu.Lock()
	c.held = held
	c.mu.Unlock()
}

func (c *statusCluster) thaw() {
	c.mu.Lock()
	c.held = nil
	c.mu.Unlock()
}

// crClusterOf is a fake dynamic client over the three kinds that holds objs.
func crClusterOf(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crBindings: "BindingList", crBackends: "AgentBackendList", crConnections: "ChatConnectionList",
	}, objs...)
}

// count is the number of applies taken so far.
func (c *statusCluster) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.applies)
}

// snapshotApplies is a copy of the applies taken so far.
func (c *statusCluster) snapshotApplies() []statusApply {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.applies)
}

// last is the newest apply.
func (c *statusCluster) last(t *testing.T) statusApply {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.applies) == 0 {
		t.Fatal("no status was applied")
	}
	return c.applies[len(c.applies)-1]
}

// stored returns the conditions that the cluster holds for the Binding, by type.
func (c *statusCluster) stored(t *testing.T, key string) map[string]metav1.Condition {
	t.Helper()
	ns, name, _ := strings.Cut(key, "/")
	obj, err := c.dyn.Tracker().Get(crBindings, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	return byType(crsource.ConditionsOf(obj.(*unstructured.Unstructured)))
}

// storedBinding is a Binding of the cluster at generation, with the status conds if there are any.
func storedBinding(key string, generation int64, conds ...metav1.Condition) *unstructured.Unstructured {
	ns, name, _ := strings.Cut(key, "/")
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.Group + "/" + v1alpha1.Version, "kind": "Binding",
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     map[string]any{},
	}}
	u.SetGeneration(generation)
	if len(conds) > 0 {
		u.Object["status"] = statusObject(ns, name, conds).Object["status"]
	}
	return u
}

// withUID gives the object of the cluster a uid, which the Snapshot's Binding must have too for the writer to say anything of it.
func withUID(u *unstructured.Unstructured, uid string) *unstructured.Unstructured {
	u.SetUID(types.UID(uid))
	return u
}

// cond is a condition of the cluster's status.
func cond(typ string, status metav1.ConditionStatus, reason string, generation int64, at time.Time) metav1.Condition {
	return metav1.Condition{Type: typ, Status: status, Reason: reason, ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(at)}
}

// snapshotOf resolves one Binding for each key, each at the generation given, with its own channel on one connection.
func snapshotOf(t *testing.T, generations map[string]int64) *config.Snapshot {
	t.Helper()
	return snapshotWithUIDs(t, generations, nil)
}

// snapshotWithUIDs is snapshotOf for Bindings that have the uids given, by key, and none where it has none.
func snapshotWithUIDs(t *testing.T, generations map[string]int64, uids map[string]string) *config.Snapshot {
	t.Helper()
	set := &config.Set{
		AgentBackends:   []v1alpha1.AgentBackend{{ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "agent"}, Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A}}},
		ChatConnections: []v1alpha1.ChatConnection{{ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "slack"}}},
	}
	for _, key := range slices.Sorted(maps.Keys(generations)) {
		ns, name, _ := strings.Cut(key, "/")
		b := v1alpha1.Binding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: generations[key], UID: types.UID(uids[key])}}
		b.Spec.Agent.BackendRef = v1alpha1.ObjectRef{Namespace: "sre", Name: "agent"}
		b.Spec.Chat.ConnectionRef = v1alpha1.ObjectRef{Namespace: "sre", Name: "slack"}
		b.Spec.Chat.Channel = "C-" + name
		set.Bindings = append(set.Bindings, b)
	}
	set.AgentBackends[0].Spec.AllowedNamespaces = []string{"sre", "other"}
	set.ChatConnections[0].Spec.AllowedNamespaces = []string{"sre", "other"}
	snap, rejected := config.ResolvePartial(set)
	if len(rejected) != 0 {
		t.Fatalf("the fixture's Bindings were rejected: %+v", rejected)
	}
	return snap
}

// statusRig is a writer over a statusCluster with a report and conditions that a test moves by hand, and a clock.
type statusRig struct {
	t       *testing.T
	cluster *statusCluster
	w       *statusWriter
	logs    *bytes.Buffer

	mu     sync.Mutex
	report config.Report
	probed map[string][]bindingcheck.Condition
	now    time.Time
	writes []string
}

var statusT0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// newStatusRig builds a writer over cluster. serving gives the generation of each Binding the source serves.
func newStatusRig(t *testing.T, cluster *statusCluster, serving map[string]int64) *statusRig {
	t.Helper()
	r := &statusRig{t: t, cluster: cluster, logs: &bytes.Buffer{}, now: statusT0, probed: map[string][]bindingcheck.Condition{}}
	r.report = config.Report{Snapshot: snapshotOf(t, serving)}
	cluster.warn = &warningCounter{}
	w, err := newStatusWriter(statusConfig{
		Client: cluster.dyn, Report: func() config.Report { r.mu.Lock(); defer r.mu.Unlock(); return r.report },
		Conditions: func() map[string][]bindingcheck.Condition { r.mu.Lock(); defer r.mu.Unlock(); return r.probed },
		Observed:   cluster.observed, Warnings: cluster.warn, Log: slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		OnWrite: func(result string) { r.writes = append(r.writes, result) },
		Gap:     10 * time.Second, Tick: time.Second,
		Now: func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.w = w
	return r
}

// advance moves the clock by d.
func (r *statusRig) advance(d time.Duration) { r.mu.Lock(); r.now = r.now.Add(d); r.mu.Unlock() }

// set replaces what the Prober found for the Binding key.
func (r *statusRig) set(key string, conds ...bindingcheck.Condition) {
	r.mu.Lock()
	r.probed[key] = conds
	r.mu.Unlock()
	r.w.notify()
}

// serveWith makes the source serve the Bindings at the generations and with the uids given.
func (r *statusRig) serveWith(generations map[string]int64, uids map[string]string) {
	r.mu.Lock()
	r.report.Snapshot = snapshotWithUIDs(r.t, generations, uids)
	r.mu.Unlock()
}

// reject replaces what the source refused.
func (r *statusRig) reject(rej ...config.Rejection) {
	r.mu.Lock()
	r.report.Rejections = rej
	r.mu.Unlock()
}

// serve makes the source serve the Bindings at the generations given.
func (r *statusRig) serve(generations map[string]int64) {
	r.mu.Lock()
	r.report.Snapshot = snapshotOf(r.t, generations)
	r.mu.Unlock()
}

// reconcile runs one pass.
func (r *statusRig) reconcile() { r.w.reconcile(r.t.Context()) }

// probe returns the conditions of the Prober for a Binding that has been probed at generation, all True, or Pending for those named.
func probe(generation int64, pending ...bindingcheck.ConditionType) []bindingcheck.Condition {
	mk := func(t bindingcheck.ConditionType) bindingcheck.Condition {
		c := bindingcheck.Condition{Type: t, Status: bindingcheck.True, Reason: bindingcheck.ReasonOK, ObservedGeneration: generation, LastTransitionTime: statusT0.Add(time.Minute)}
		if slices.Contains(pending, t) {
			c.Status, c.Reason = bindingcheck.Unknown, bindingcheck.ReasonPending
		}
		return c
	}
	ready := bindingcheck.Condition{Type: bindingcheck.Ready, Status: bindingcheck.True, Reason: bindingcheck.ReasonOK, ObservedGeneration: generation, LastTransitionTime: statusT0.Add(time.Minute)}
	if len(pending) > 0 {
		ready.Status, ready.Reason = bindingcheck.Unknown, bindingcheck.Reason(string(pending[0])+":"+string(bindingcheck.ReasonPending))
	}
	return []bindingcheck.Condition{mk(bindingcheck.BackendReachable), mk(bindingcheck.ChannelJoined), ready}
}

const sreAlerts = "sre/alerts"

// The body of the apply is what the CRD takes and what Server-Side Apply on the status subresource needs: the kind and the name
// and no spec, under the one field manager, forced, to the subresource "status", and each condition has message, reason and
// lastTransitionTime, so that metav1.Condition's required fields are there although a message is empty.
func TestStatusIsAppliedAsServerSideApplyOnTheStatusSubresource(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 3))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 3})
	r.set(sreAlerts, probe(3)...)
	r.reconcile()

	if c.count() != 1 {
		t.Fatalf("applies = %d, want 1", c.count())
	}
	ap := c.last(t)
	if ap.Namespace != "sre" || ap.Name != "alerts" || ap.Subresource != "status" {
		t.Errorf("applied to %s/%s subresource %q, want sre/alerts, status", ap.Namespace, ap.Name, ap.Subresource)
	}
	if ap.Options.FieldManager != statusFieldManager || statusFieldManager == "" || ap.Options.Force == nil || !*ap.Options.Force {
		t.Errorf("options = %+v, want the fixed field manager %q and force", ap.Options, statusFieldManager)
	}
	if ap.Body["apiVersion"] != "agentkourier.dev/v1alpha1" || ap.Body["kind"] != "Binding" {
		t.Errorf("body names %v %v, want agentkourier.dev/v1alpha1 Binding", ap.Body["apiVersion"], ap.Body["kind"])
	}
	if _, has := ap.Body["spec"]; has {
		t.Error("the body carries a spec: the writer applies the status and nothing else")
	}
	if meta := ap.Body["metadata"].(map[string]any); meta["name"] != "alerts" || meta["namespace"] != "sre" || len(meta) != 2 {
		t.Errorf("metadata = %v, want the name and namespace only (no resourceVersion: there is no optimistic lock)", meta)
	}
	got := ap.conds()
	var types []string
	for typ, m := range got {
		types = append(types, typ)
		for _, field := range []string{"type", "status", "reason", "message", "lastTransitionTime"} {
			if _, has := m[field]; !has {
				t.Errorf("%s has no %q: metav1.Condition requires it", typ, field)
			}
		}
		if m["observedGeneration"] != int64(3) {
			t.Errorf("%s observedGeneration = %v (%T), want 3", typ, m["observedGeneration"], m["observedGeneration"])
		}
		if _, err := time.Parse(time.RFC3339, m["lastTransitionTime"].(string)); err != nil {
			t.Errorf("%s lastTransitionTime = %v: %v", typ, m["lastTransitionTime"], err)
		}
	}
	slices.Sort(types)
	if want := []string{"Accepted", "BackendReachable", "ChannelJoined", "Ready"}; !slices.Equal(types, want) {
		t.Errorf("condition types = %v, want %v", types, want)
	}
	if got["Accepted"]["status"] != "True" || got["Accepted"]["reason"] != "OK" || got["Ready"]["status"] != "True" {
		t.Errorf("Accepted = %v, Ready = %v; want both True", got["Accepted"], got["Ready"])
	}
	if got["Accepted"]["message"] != "" {
		t.Errorf("Accepted message = %q, want empty (and present)", got["Accepted"]["message"])
	}
	if _, has := got["Ready"]["lastProbeTime"]; has {
		t.Error("lastProbeTime is written: a heartbeat of the status is watch traffic for every consumer")
	}
}

// A status that already says what the Binding should say is not written again, and a second pass over the same findings
// writes nothing: the writer writes on a difference, and has no heartbeat.
func TestStatusIsWrittenOnlyWhenItDiffers(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...)
	r.reconcile()
	for range 5 {
		r.advance(time.Minute)
		r.reconcile()
	}
	if c.count() != 1 {
		t.Errorf("applies after six passes over the same findings = %d, want 1", c.count())
	}
	if !slices.Equal(r.writes, []string{"written"}) {
		t.Errorf("write results = %v, want one written", r.writes)
	}
	// A probe that finds the same again, but at the generation of a later pass, is a difference: observedGeneration moves.
	r.serve(map[string]int64{sreAlerts: 2})
	r.set(sreAlerts, probe(2)...)
	r.advance(time.Minute)
	r.reconcile()
	if c.count() != 2 {
		t.Fatalf("applies after the generation moved = %d, want 2", c.count())
	}
	if got := c.last(t).conds()["Ready"]["observedGeneration"]; got != int64(2) {
		t.Errorf("Ready observedGeneration = %v, want 2 although nothing transitioned", got)
	}
}

// A takeover starts every probed condition at Pending, and writing that would blank a good status at each failover. A Pending
// condition keeps what the status says unless that is of an older generation than the one served, or nothing: then Pending is the
// truth. A Ready that a rejection wrote is not what an accepted Binding keeps.
func TestTakeoverDoesNotBlankAGoodStatus(t *testing.T) {
	good := func(gen int64) []metav1.Condition {
		at := statusT0.Add(-time.Hour)
		return []metav1.Condition{
			cond("Accepted", metav1.ConditionTrue, "OK", gen, at), cond("BackendReachable", metav1.ConditionTrue, "OK", gen, at),
			cond("ChannelJoined", metav1.ConditionTrue, "OK", gen, at), cond("Ready", metav1.ConditionTrue, "OK", gen, at),
		}
	}
	for name, tc := range map[string]struct {
		stored  []metav1.Condition
		pending []bindingcheck.ConditionType
		applies int
		want    map[string][2]string // type -> status, reason in the cluster afterwards
	}{
		"good status of the generation served is kept": {
			stored: good(2), pending: []bindingcheck.ConditionType{bindingcheck.BackendReachable, bindingcheck.ChannelJoined}, applies: 0,
			want: map[string][2]string{"BackendReachable": {"True", "OK"}, "ChannelJoined": {"True", "OK"}, "Ready": {"True", "OK"}},
		},
		"a status of an older generation is not": {
			stored: good(1), pending: []bindingcheck.ConditionType{bindingcheck.BackendReachable, bindingcheck.ChannelJoined}, applies: 1,
			want: map[string][2]string{"BackendReachable": {"Unknown", "Pending"}, "ChannelJoined": {"Unknown", "Pending"}, "Ready": {"Unknown", "BackendReachable:Pending"}},
		},
		"no status at all is written as it is": {
			stored: nil, pending: []bindingcheck.ConditionType{bindingcheck.BackendReachable, bindingcheck.ChannelJoined}, applies: 1,
			want: map[string][2]string{"Accepted": {"True", "OK"}, "BackendReachable": {"Unknown", "Pending"}, "Ready": {"Unknown", "BackendReachable:Pending"}},
		},
		"only the condition that is pending is kept": {
			stored: good(2), pending: []bindingcheck.ConditionType{bindingcheck.ChannelJoined}, applies: 0,
			want: map[string][2]string{"BackendReachable": {"True", "OK"}, "ChannelJoined": {"True", "OK"}, "Ready": {"True", "OK"}},
		},
		"a Ready that a rejection wrote is not carried": {
			stored: []metav1.Condition{
				cond("Accepted", metav1.ConditionFalse, "InvalidSpec", 2, statusT0), cond("BackendReachable", metav1.ConditionTrue, "OK", 2, statusT0),
				cond("ChannelJoined", metav1.ConditionTrue, "OK", 2, statusT0), cond("Ready", metav1.ConditionFalse, "Accepted:InvalidSpec", 2, statusT0),
			},
			pending: []bindingcheck.ConditionType{bindingcheck.ChannelJoined}, applies: 1,
			want: map[string][2]string{"Accepted": {"True", "OK"}, "ChannelJoined": {"True", "OK"}, "Ready": {"Unknown", "ChannelJoined:Pending"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newStatusCluster(t, storedBinding(sreAlerts, 2, tc.stored...))
			r := newStatusRig(t, c, map[string]int64{sreAlerts: 2})
			// A new leader's Prober: the conditions it has not probed are Pending, at the generation it saw.
			r.set(sreAlerts, probe(2, tc.pending...)...)
			r.reconcile()
			if c.count() != tc.applies {
				t.Fatalf("applies = %d, want %d", c.count(), tc.applies)
			}
			got := c.stored(t, sreAlerts)
			for typ, want := range tc.want {
				if g := got[typ]; string(g.Status) != want[0] || g.Reason != want[1] {
					t.Errorf("%s in the cluster = %s/%s, want %s/%s", typ, g.Status, g.Reason, want[0], want[1])
				}
			}
		})
	}
}

// The failover that the takeover rule is for, end to end for the writer: the new leader's conditions go from Pending to True, and
// the status that was good before the failover is never written, since what the probes find is what it already said, with the
// time it became so kept.
func TestFailoverWritesNothingWhileTheStatusStaysTrue(t *testing.T) {
	then := statusT0.Add(-24 * time.Hour)
	stored := []metav1.Condition{
		cond("Accepted", metav1.ConditionTrue, "OK", 4, then), cond("BackendReachable", metav1.ConditionTrue, "OK", 4, then),
		cond("ChannelJoined", metav1.ConditionTrue, "OK", 4, then), cond("Ready", metav1.ConditionTrue, "OK", 4, then),
	}
	c := newStatusCluster(t, storedBinding(sreAlerts, 4, stored...))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 4})
	r.set(sreAlerts, probe(4, bindingcheck.BackendReachable, bindingcheck.ChannelJoined)...) // the new leader has probed nothing
	r.reconcile()
	r.advance(5 * time.Second)
	r.set(sreAlerts, probe(4)...) // the first pass ended: all True, since a moment ago
	r.reconcile()
	r.advance(time.Minute)
	r.reconcile()
	if c.count() != 0 {
		t.Errorf("applies = %d, want none: the status was true before the failover and is true after it", c.count())
	}
	// A probe that finds a thing false afterwards is written, and the time of the transition is when the writer saw the status change.
	bad := probe(4)
	bad[0].Status, bad[0].Reason, bad[0].LastTransitionTime = bindingcheck.False, bindingcheck.ReasonAgentNotFound, statusT0.Add(2*time.Minute)
	bad[2].Status, bad[2].Reason, bad[2].LastTransitionTime = bindingcheck.False, "BackendReachable:AgentNotFound", statusT0.Add(2*time.Minute)
	r.advance(time.Minute)
	seenAt := statusT0.Add(5*time.Second + 2*time.Minute)
	r.set(sreAlerts, bad...)
	r.reconcile()
	if c.count() != 1 {
		t.Fatalf("applies after a failing probe = %d, want 1", c.count())
	}
	got := c.last(t).conds()
	if got["BackendReachable"]["status"] != "False" || got["BackendReachable"]["reason"] != "AgentNotFound" ||
		got["BackendReachable"]["lastTransitionTime"] != seenAt.Format(time.RFC3339) {
		t.Errorf("BackendReachable = %v, want False/AgentNotFound, at %s", got["BackendReachable"], seenAt.Format(time.RFC3339))
	}
	if got["ChannelJoined"]["lastTransitionTime"] != then.Format(time.RFC3339) {
		t.Errorf("ChannelJoined lastTransitionTime = %v, want %s: it did not change status", got["ChannelJoined"]["lastTransitionTime"], then.Format(time.RFC3339))
	}
}

// The writer writes only as the leader it is run by, behind the fence: a leader whose lease is past the renew deadline writes
// nothing and the process is stopped, and the same writer under a fresh lease writes.
func TestAFencedLeaderWritesNothing(t *testing.T) {
	for name, renewed := range map[string]bool{"lease is fresh": true, "lease was never renewed": false} {
		t.Run(name, func(t *testing.T) {
			c := newStatusCluster(t, storedBinding(sreAlerts, 1))
			r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
			clock := newStepClock()
			lease := newLeaseClock(clock.now)
			if renewed {
				lease.renewed(clock.now())
			}
			var stopped []string
			r.w.cfg.Fence = newLeaseFence(ElectionConfig{}, lease, func(reason string) { stopped = append(stopped, reason) })
			r.set(sreAlerts, probe(1)...)
			r.reconcile()
			if renewed && (c.count() != 1 || len(stopped) != 0) {
				t.Errorf("applies = %d, stops = %v; want one write and no stop", c.count(), stopped)
			}
			if !renewed && (c.count() != 0 || len(stopped) != 1) {
				t.Errorf("applies = %d, stops = %v; want no write and one stop", c.count(), stopped)
			}
		})
	}
	t.Run("lease runs out between two writes", func(t *testing.T) {
		c := newStatusCluster(t, storedBinding(sreAlerts, 1))
		r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
		clock := newStepClock()
		lease := newLeaseClock(clock.now)
		lease.renewed(clock.now())
		var stopped int
		r.w.cfg.Fence = newLeaseFence(ElectionConfig{}, lease, func(string) { stopped++ })
		r.set(sreAlerts, probe(1)...)
		r.reconcile()
		clock.advance(time.Minute) // past the renew deadline
		r.advance(time.Minute)
		r.serve(map[string]int64{sreAlerts: 2})
		r.set(sreAlerts, probe(2)...)
		r.reconcile()
		if c.count() != 1 || stopped != 1 {
			t.Errorf("applies = %d, stops = %d; want the first write and no second, and one stop", c.count(), stopped)
		}
	})
}

// A Binding's status is written at most once each gap, and what a Binding says when the gap ends is the newest, however many
// times it moved inside it.
func TestWritesOfOneBindingAreSpacedByTheGapAndCoalesced(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...)
	r.reconcile()
	for i, reason := range []bindingcheck.Reason{bindingcheck.ReasonAgentNotFound, bindingcheck.ReasonAgentNotReady, bindingcheck.ReasonBackendError} {
		r.advance(2 * time.Second)
		conds := probe(1)
		conds[0].Status, conds[0].Reason = bindingcheck.False, reason
		conds[2].Status, conds[2].Reason = bindingcheck.False, "BackendReachable:"+reason
		conds[0].LastTransitionTime = statusT0.Add(time.Duration(i+1) * time.Hour)
		r.set(sreAlerts, conds...)
		r.reconcile()
		if c.count() != 1 {
			t.Fatalf("applies %ds after the first write = %d, want 1: the gap is 10s", 2*(i+1), c.count())
		}
	}
	r.advance(5 * time.Second) // 11s since the first write
	r.reconcile()
	if c.count() != 2 {
		t.Fatalf("applies after the gap = %d, want 2", c.count())
	}
	if got := c.last(t).conds()["BackendReachable"]["reason"]; got != "BackendError" {
		t.Errorf("reason written = %v, want the newest, BackendError: the ones in between are coalesced", got)
	}
	r.advance(time.Minute)
	r.reconcile()
	if c.count() != 2 {
		t.Errorf("applies = %d after the last write caught up, want 2", c.count())
	}
}

// A write that fails is tried again after a wait that doubles, said once in the log, and counted; the Binding that was deleted
// meanwhile is let go without a word.
func TestAFailedWriteIsTriedAgainWithBackoffAndSaidOnce(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	c.fail = func(statusApply) error {
		return apierrors.NewForbidden(crBindings.GroupResource(), "alerts", errors.New("bindings/status is not allowed"))
	}
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...)
	r.reconcile()
	counts := []int{c.count()}
	for _, wait := range []time.Duration{5 * time.Second, 6 * time.Second, 10 * time.Second, 15 * time.Second} {
		r.advance(wait)
		r.reconcile()
		counts = append(counts, c.count())
	}
	// Tries at 0, 10, 30 (10s then 20s), not at the times in between.
	if want := []int{1, 1, 2, 2, 3}; !slices.Equal(counts, want) {
		t.Errorf("applies after each step = %v, want %v", counts, want)
	}
	if n := strings.Count(r.logs.String(), "a write failed"); n != 1 {
		t.Errorf("the failure was said %d times, want once:\n%s", n, r.logs)
	}
	if strings.Contains(r.logs.String(), "is not allowed") && !strings.Contains(r.logs.String(), "forbidden") {
		t.Errorf("the log lost the error's class:\n%s", r.logs)
	}
	c.mu.Lock()
	c.fail = nil
	c.mu.Unlock()
	r.advance(statusBackoffCap)
	r.reconcile()
	if got := c.stored(t, sreAlerts)["Accepted"]; got.Status != metav1.ConditionTrue {
		t.Errorf("Accepted after the API server recovered = %+v, want True", got)
	}
	if want := []string{"failed", "failed", "failed", "written"}; !slices.Equal(r.writes, want) {
		t.Errorf("write results = %v, want %v", r.writes, want)
	}

	t.Run("deleted", func(t *testing.T) {
		c := newStatusCluster(t, storedBinding(sreAlerts, 1)) // the cache has the Binding, the API server has already lost it
		c.fail = func(statusApply) error { return apierrors.NewNotFound(crBindings.GroupResource(), "alerts") }
		r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
		r.set(sreAlerts, probe(1)...)
		r.reconcile()
		for range 8 { // the cache's next event, which a watch brings at once, drops the Binding from the report
			r.advance(time.Second)
			r.reconcile()
		}
		if c.count() != 1 {
			t.Errorf("applies = %d, want 1: a Binding that is gone is not asked again within the gap", c.count())
		}
		if len(r.writes) != 0 || strings.Contains(r.logs.String(), "level=WARN") {
			t.Errorf("results %v, log:\n%s\nwant neither a result nor a warning for a Binding that is gone", r.writes, r.logs)
		}
	})
}

// Nothing is written while the statuses cannot be read: a cache that has not synced would say that every status is empty, and a
// new leader would write Pending over a good one. It is said once, and the writer goes on as soon as the cache can be read.
func TestNothingIsWrittenUntilTheStatusesCanBeRead(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1,
		cond("Accepted", metav1.ConditionTrue, "OK", 1, statusT0), cond("BackendReachable", metav1.ConditionTrue, "OK", 1, statusT0),
		cond("ChannelJoined", metav1.ConditionTrue, "OK", 1, statusT0), cond("Ready", metav1.ConditionTrue, "OK", 1, statusT0)))
	c.unsynced = crsource.ErrNotSynced
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1, bindingcheck.BackendReachable, bindingcheck.ChannelJoined)...)
	for range 3 {
		r.advance(time.Second)
		r.reconcile()
	}
	if c.count() != 0 {
		t.Fatalf("applies = %d with a cache that has not synced, want none", c.count())
	}
	if n := strings.Count(r.logs.String(), "cannot be read yet"); n != 1 {
		t.Errorf("the cache that cannot be read was said %d times, want once:\n%s", n, r.logs)
	}
	c.mu.Lock()
	c.unsynced = nil
	c.mu.Unlock()
	r.advance(time.Second)
	r.reconcile()
	if c.count() != 0 {
		t.Errorf("applies = %d once the cache synced, want none: the good status is kept (Pending is carried over it)", c.count())
	}
	if got := c.stored(t, sreAlerts)["Ready"]; got.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want it kept True", got)
	}
}

// A broker that serves a Binding says what it serves, and puts the status back if it says anything else, whoever changed it, at most
// once each gap; and when nothing differs it writes nothing, however many passes follow.
func TestAServingBrokerPutsItsStatusBackAtMostOnceEachGap(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...)
	r.reconcile()
	if c.count() != 1 {
		t.Fatalf("applies = %d, want 1", c.count())
	}
	edited := storedBinding(sreAlerts, 1, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ByHand", Message: "edited by hand", LastTransitionTime: metav1.NewTime(statusT0)})
	if err := c.dyn.Tracker().Update(crBindings, edited, "sre"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		r.advance(time.Second)
		r.reconcile()
	}
	if c.count() != 1 {
		t.Errorf("applies = %d inside the gap, want 1", c.count())
	}
	r.advance(10 * time.Second)
	r.reconcile()
	if c.count() != 2 {
		t.Fatalf("applies after the gap = %d, want 2: the status is put back", c.count())
	}
	if got := c.stored(t, sreAlerts); got["Ready"].Status != metav1.ConditionTrue || got["Accepted"].Status != metav1.ConditionTrue {
		t.Errorf("status = %+v, want it put back", got)
	}
	for range 50 {
		r.advance(time.Minute)
		r.reconcile()
	}
	if c.count() != 2 {
		t.Errorf("applies = %d after fifty passes with nothing different, want 2", c.count())
	}
}

// A cache that has not yet seen what the writer wrote is no drift: the writer does not write again for it inside the gap, and
// writes nothing once the cache has caught up and says the same.
func TestACacheThatLagsItsOwnWriteIsNotDrift(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...)
	c.freeze(t, sreAlerts) // the cache says there is no status
	r.reconcile()
	for range 5 {
		r.advance(time.Second)
		r.reconcile()
	}
	if c.count() != 1 {
		t.Fatalf("applies = %d while the cache lagged inside the gap, want 1", c.count())
	}
	c.thaw()
	r.advance(10 * time.Second)
	r.reconcile()
	if c.count() != 1 {
		t.Errorf("applies = %d once the cache caught up, want 1", c.count())
	}
}

// A Binding that was deleted and made again has no status, and the writer says nothing of the new object until the Snapshot serves it:
// until the reload that brings it in, the Snapshot and the Prober hold the old object (its uid, its generation, its True), and what
// they say is not the new object's. The informer shows the new object at watch latency, so for up to one reload the cache and the
// Snapshot disagree about which object it is, and the writer leaves the status alone: it never writes the old object's True onto the
// new one, with an observedGeneration above the generation of the object, where a kubectl wait would take it for met. The new
// object's status comes with the reload. The same generation (1 again) is no different, which only the uid tells. A reporter, which
// has no uid to compare, leaves an object that is older than the generation its rejection was made for.
func TestAStatusIsWrittenToABindingMadeAgainOnlyOnceTheSnapshotServesIt(t *testing.T) {
	c := newStatusCluster(t, withUID(storedBinding(sreAlerts, 7), "uid-old"))
	r := newStatusRig(t, c, nil)
	r.serveWith(map[string]int64{sreAlerts: 7}, map[string]string{sreAlerts: "uid-old"})
	conds := probe(7)
	for i := range conds {
		conds[i].UID = "uid-old"
	}
	r.set(sreAlerts, conds...)
	r.reconcile()
	if got := c.stored(t, sreAlerts); got["Ready"].Status != metav1.ConditionTrue || got["Ready"].ObservedGeneration != 7 {
		t.Fatalf("Ready = %+v, want True at 7 for the old object", got["Ready"])
	}
	for name, generation := range map[string]int64{"another generation": 1, "the same generation": 7} {
		t.Run(name, func(t *testing.T) {
			c := newStatusCluster(t, withUID(storedBinding(sreAlerts, 7), "uid-old"))
			r := newStatusRig(t, c, nil)
			r.serveWith(map[string]int64{sreAlerts: 7}, map[string]string{sreAlerts: "uid-old"})
			r.set(sreAlerts, conds...)
			r.reconcile()

			again := withUID(storedBinding(sreAlerts, generation), "uid-new") // deleted and made again: no status; the Snapshot has not reloaded
			if err := c.dyn.Tracker().Update(crBindings, again, "sre"); err != nil {
				t.Fatal(err)
			}
			applies := c.count()
			for range 10 { // the watch is seen at once, and the reload is up to ten seconds away
				r.advance(time.Second)
				r.reconcile()
			}
			if c.count() != applies || len(c.stored(t, sreAlerts)) != 0 {
				t.Fatalf("applies went from %d to %d and the new object has %v: nothing may be written until the Snapshot serves it", applies, c.count(), c.stored(t, sreAlerts))
			}

			// The reload: the Snapshot serves the new object, and the Prober, which has seen the new uid, says Pending at its generation.
			r.serveWith(map[string]int64{sreAlerts: generation}, map[string]string{sreAlerts: "uid-new"})
			pending := probe(generation, bindingcheck.BackendReachable, bindingcheck.ChannelJoined)
			for i := range pending {
				pending[i].UID = "uid-new"
			}
			r.set(sreAlerts, pending...)
			r.reconcile()
			got := c.stored(t, sreAlerts)
			for _, typ := range []string{"BackendReachable", "ChannelJoined", "Ready"} {
				if got[typ].Status == metav1.ConditionTrue || got[typ].ObservedGeneration > generation {
					t.Errorf("%s = %s at %d, want it not True and never above the generation %d", typ, got[typ].Status, got[typ].ObservedGeneration, generation)
				}
			}
			if got["Accepted"].ObservedGeneration != generation {
				t.Errorf("Accepted observedGeneration = %d, want %d", got["Accepted"].ObservedGeneration, generation)
			}
		})
	}

	t.Run("the deletion seen first", func(t *testing.T) {
		c := newStatusCluster(t, withUID(storedBinding(sreAlerts, 1), "uid-1"))
		r := newStatusRig(t, c, nil)
		r.serveWith(map[string]int64{sreAlerts: 1}, map[string]string{sreAlerts: "uid-1"})
		pending := probe(1)
		for i := range pending {
			pending[i].UID = "uid-1"
		}
		r.set(sreAlerts, pending...)
		r.reconcile()
		if err := c.dyn.Tracker().Delete(crBindings, "sre", "alerts"); err != nil {
			t.Fatal(err)
		}
		applies := c.count()
		for range 3 {
			r.advance(time.Minute)
			r.reconcile()
		}
		if c.count() != applies || strings.Contains(r.logs.String(), "level=WARN") {
			t.Errorf("applies = %d (was %d), log:\n%s\nwant no write and no warning while the cache holds no Binding", c.count(), applies, r.logs)
		}
		if err := c.dyn.Tracker().Add(withUID(storedBinding(sreAlerts, 1), "uid-2")); err != nil {
			t.Fatal(err)
		}
		r.advance(time.Minute)
		r.reconcile()
		if c.count() != applies {
			t.Errorf("applies = %d, want %d: the object that came after is not the one served", c.count(), applies)
		}
		r.serveWith(map[string]int64{sreAlerts: 1}, map[string]string{sreAlerts: "uid-2"})
		for i := range pending {
			pending[i].UID = "uid-2"
		}
		r.set(sreAlerts, pending...)
		r.reconcile()
		if c.count() != applies+1 {
			t.Errorf("applies = %d, want %d: the new object is written once the Snapshot serves it", c.count(), applies+1)
		}
	})

	t.Run("a reported Binding made again", func(t *testing.T) {
		c := newStatusCluster(t, storedBinding(sreAlerts, 7))
		r := newStatusRig(t, c, nil)
		rejected := config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonInvalidSpec, Generation: 7, Message: "spec: generation 7 is invalid"}
		r.reject(rejected)
		r.reconcile()
		if c.count() != 1 {
			t.Fatalf("applies = %d, want 1", c.count())
		}
		if err := c.dyn.Tracker().Update(crBindings, storedBinding(sreAlerts, 1), "sre"); err != nil { // made again: generation 1, no status
			t.Fatal(err)
		}
		for range 5 {
			r.advance(time.Minute)
			r.reconcile()
		}
		if c.count() != 1 || len(c.stored(t, sreAlerts)) != 0 {
			t.Errorf("applies = %d, status %v: a rejection of generation 7 must not be written on an object at generation 1", c.count(), c.stored(t, sreAlerts))
		}
		r.reject(config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonInvalidSpec, Generation: 1, Message: "spec: generation 1 is invalid"})
		r.reconcile()
		if c.count() != 2 || c.stored(t, sreAlerts)["Accepted"].ObservedGeneration != 1 {
			t.Errorf("applies = %d, Accepted %+v, want the new object's own rejection written", c.count(), c.stored(t, sreAlerts)["Accepted"])
		}
	})
}

// Two brokers that both report one Binding: the one that serves it has the last word, whichever wrote first. A reporter writes when
// it first sees the Binding and never answers back, and the server puts its status back once each gap, so the two do not fight, and
// a broker whose view of the connection was out of date (its poll fell in the gap of a ChatConnection that was replaced) leaves a
// wrong status for the length of a watch event and not for ever.
func TestTheServingBrokersWordsWinOverAReportersWhicheverWroteFirst(t *testing.T) {
	served := config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonInvalidSpec, Generation: 2, Served: 1,
		Message: "spec: Binding sre/alerts generation 2 is invalid, serving generation 1: spec.chat.channel: wrong"}
	reported := config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonInvalidSpec, Generation: 2, Served: 0,
		Message: "spec: Binding sre/alerts generation 2 is invalid, with no earlier generation to serve: spec.chat.channel: wrong"}
	for name, serverFirst := range map[string]bool{"the server wrote first": true, "the reporter wrote first": false} {
		t.Run(name, func(t *testing.T) {
			c := newStatusCluster(t, storedBinding(sreAlerts, 2))
			server := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
			server.reject(served)
			server.set(sreAlerts, probe(1)...)
			reporter := newStatusRig(t, c, nil)
			reporter.reject(reported)

			first, second := reporter, server
			if serverFirst {
				first, second = server, reporter
			}
			first.reconcile()
			second.reconcile()
			for range 30 { // passes of both, far apart and close together
				for _, r := range []*statusRig{server, reporter} {
					r.advance(11 * time.Second)
					r.reconcile()
					r.reconcile()
				}
			}
			got := c.stored(t, sreAlerts)
			if !strings.Contains(got["Accepted"].Message, "serving generation 1") {
				t.Errorf("Accepted = %+v, want the serving broker's words", got["Accepted"])
			}
			if got["BackendReachable"].Type == "" {
				t.Error("the status lost the probes of the serving broker")
			}
			if n := c.count(); n > 3 {
				t.Errorf("applies = %d, want at most 3: the reporter writes once and the server puts it back once", n)
			}
		})
	}

	t.Run("a connection that was replaced", func(t *testing.T) {
		// The server serves the Binding with every probe True. Another broker's poll fell in the gap of a ChatConnection that was
		// made again with --force, so for a poll it reports ConnectionNotFound on a Binding it does not serve.
		c := newStatusCluster(t, storedBinding(sreAlerts, 1))
		server := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
		server.set(sreAlerts, probe(1)...)
		reporter := newStatusRig(t, c, nil)
		reporter.reject(config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonConnectionNotFound, Generation: 1,
			Message: "spec.chat.connectionRef: ChatConnection sre/slack not found"})
		server.reconcile()
		reporter.reconcile()
		if got := c.stored(t, sreAlerts)["Accepted"]; got.Reason != "ConnectionNotFound" {
			t.Fatalf("after the reporter: Accepted = %+v, want its ConnectionNotFound", got)
		}
		server.advance(11 * time.Second)
		server.reconcile()
		if got := c.stored(t, sreAlerts); got["Accepted"].Status != metav1.ConditionTrue || got["Ready"].Status != metav1.ConditionTrue {
			t.Errorf("status = %+v after the server's next pass, want Accepted and Ready True again", got)
		}
		for range 50 {
			for _, r := range []*statusRig{server, reporter} {
				r.advance(time.Minute)
				r.reconcile()
			}
		}
		if got := c.stored(t, sreAlerts); got["Ready"].Status != metav1.ConditionTrue || c.count() != 3 {
			t.Errorf("Ready = %+v, applies = %d after fifty passes, want Ready True and 3 applies: the server's, the reporter's, the server's", got["Ready"], c.count())
		}
	})
}

// What the status says of a Binding that is rejected: Accepted is False with the reason and the message of the rejection, Ready
// is False with that reason after "Accepted:", and a Binding that is still served from a last good generation keeps its probes, of
// the generation it serves, while Accepted says the generation that was refused.
func TestARejectedBindingIsNotReadyAndAServedOneKeepsItsProbes(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 3), storedBinding("sre/gone", 7))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 2})
	r.reject(
		config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonInvalidSpec, Generation: 3, Served: 2, Message: "spec: generation 3 is invalid, serving generation 2"},
		config.Rejection{Namespace: "sre", Name: "gone", Reason: config.ReasonConnectionNotFound, Generation: 7, Message: "spec.chat.connectionRef: ChatConnection sre/nope not found"},
	)
	r.set(sreAlerts, probe(2)...)
	r.set("sre/gone", probe(7)...) // the Prober has not yet pruned what it knew of a Binding that is no longer served
	r.reconcile()

	served := c.stored(t, sreAlerts)
	if a := served["Accepted"]; a.Status != metav1.ConditionFalse || a.Reason != "InvalidSpec" || a.ObservedGeneration != 3 || !strings.Contains(a.Message, "serving generation 2") {
		t.Errorf("served Accepted = %+v, want False/InvalidSpec at generation 3 naming the generation served", a)
	}
	if rd := served["Ready"]; rd.Status != metav1.ConditionFalse || rd.Reason != "Accepted:InvalidSpec" || rd.ObservedGeneration != 3 {
		t.Errorf("served Ready = %+v, want False/Accepted:InvalidSpec at generation 3", rd)
	}
	if b := served["BackendReachable"]; b.Status != metav1.ConditionTrue || b.ObservedGeneration != 2 {
		t.Errorf("served BackendReachable = %+v, want True at generation 2, the one probed and served", b)
	}

	unserved := c.stored(t, "sre/gone")
	if a := unserved["Accepted"]; a.Status != metav1.ConditionFalse || a.Reason != "ConnectionNotFound" || a.ObservedGeneration != 7 {
		t.Errorf("unserved Accepted = %+v, want False/ConnectionNotFound at generation 7", a)
	}
	if rd := unserved["Ready"]; rd.Reason != "Accepted:ConnectionNotFound" || rd.Status != metav1.ConditionFalse {
		t.Errorf("unserved Ready = %+v, want False/Accepted:ConnectionNotFound", rd)
	}
	if len(unserved) != 2 {
		t.Errorf("unserved status has %d conditions, want Accepted and Ready only (nothing is probed for it)", len(unserved))
	}
}

// Several Rejections of one Binding give one Accepted reason, the same whatever order the source lists them in, with every message
// in that order. A reason the table does not know is still a reason that fits the schema, and gives way to the ones it knows.
func TestSeveralRejectionsGiveOneAcceptedReasonInAFixedOrder(t *testing.T) {
	rej := func(reason config.RejectionReason, generation int64) config.Rejection {
		msg := "spec." + strings.ToLower(string(reason))
		if reason == config.ReasonChannelConflict { // in the words of resolution, which the writer reads: a winner of the Binding's own namespace is named
			msg = channelConflictMessage
		}
		return config.Rejection{Namespace: "sre", Name: "alerts", Reason: reason, Generation: generation, Message: msg}
	}
	for name, tc := range map[string]struct {
		in         []config.Rejection
		served     int64
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string
		wantGen    int64
	}{
		"none": {nil, 4, metav1.ConditionTrue, "OK", "", 4},
		"one":  {[]config.Rejection{rej(config.ReasonChannelConflict, 2)}, 0, metav1.ConditionFalse, "ChannelConflict", channelConflictMessage, 2},
		"the object itself first": {
			[]config.Rejection{rej(config.ReasonChannelConflict, 2), rej(config.ReasonBackendNotFound, 2), rej(config.ReasonInvalidSpec, 2)}, 0,
			metav1.ConditionFalse, "InvalidSpec", "spec.invalidspec; spec.backendnotfound; " + channelConflictMessage, 2,
		},
		"backend before connection": {
			[]config.Rejection{rej(config.ReasonConnectionRejected, 2), rej(config.ReasonBackendRejected, 2)}, 0,
			metav1.ConditionFalse, "BackendRejected", "spec.backendrejected; spec.connectionrejected", 2,
		},
		"not found before not allowed": {
			[]config.Rejection{rej(config.ReasonNamespaceNotAllowed, 2), rej(config.ReasonConnectionNotFound, 2)}, 0,
			metav1.ConditionFalse, "ConnectionNotFound", "spec.connectionnotfound; spec.namespacenotallowed", 2,
		},
		"a reason that is not known": {
			[]config.Rejection{rej("Weird Reason!", 2)}, 0, metav1.ConditionFalse, "Rejected", "spec.weird reason!", 2,
		},
		"a known reason beats an unknown one": {
			[]config.Rejection{rej("Weird", 2), rej(config.ReasonChannelConflict, 2)}, 0, metav1.ConditionFalse, "ChannelConflict", channelConflictMessage + "; spec.weird", 2,
		},
		"the newest generation is observed": {
			[]config.Rejection{rej(config.ReasonBackendNotFound, 2), rej(config.ReasonInvalidSpec, 5)}, 3, metav1.ConditionFalse, "InvalidSpec", "spec.invalidspec; spec.backendnotfound", 5,
		},
		"no generation known: the one served": {
			[]config.Rejection{rej(config.ReasonBackendNotFound, 0)}, 3, metav1.ConditionFalse, "BackendNotFound", "spec.backendnotfound", 3,
		},
		"the messages of one reason are ordered by their text": {
			[]config.Rejection{
				{Namespace: "sre", Name: "alerts", Reason: config.ReasonNamespaceNotAllowed, Generation: 2, Message: "spec.chat.connectionRef: b"},
				{Namespace: "sre", Name: "alerts", Reason: config.ReasonNamespaceNotAllowed, Generation: 2, Message: "spec.agent.backendRef: a"},
			}, 0, metav1.ConditionFalse, "NamespaceNotAllowed", "spec.agent.backendRef: a; spec.chat.connectionRef: b", 2,
		},
		"a message told twice is told once": {
			[]config.Rejection{rej(config.ReasonBackendNotFound, 2), rej(config.ReasonBackendNotFound, 2)}, 0, metav1.ConditionFalse, "BackendNotFound", "spec.backendnotfound", 2,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Every order of the Rejections: the answer is one.
			for _, in := range permutations(tc.in) {
				got := acceptedCondition(in, tc.served)
				if got.Status != tc.wantStatus || got.Reason != tc.wantReason || got.Message != tc.wantMsg || got.ObservedGeneration != tc.wantGen || got.Type != "Accepted" {
					t.Fatalf("acceptedCondition(%v) = %s/%s %q gen %d, want %s/%s %q gen %d", reasonsOf(in), got.Status, got.Reason, got.Message, got.ObservedGeneration,
						tc.wantStatus, tc.wantReason, tc.wantMsg, tc.wantGen)
				}
			}
		})
	}
}

// The reasons that Accepted orders are the reasons the config package gives and the metric counts, no more and no fewer.
func TestAcceptedReasonOrderListsEveryReasonOfTheConfig(t *testing.T) {
	if !slices.Equal(slices.Sorted(slices.Values(acceptedReasonOrder)), slices.Sorted(slices.Values(rejectionReasons))) {
		t.Errorf("acceptedReasonOrder = %v, the metric counts %v: a reason with no place in the order would be reported as Rejected", acceptedReasonOrder, rejectionReasons)
	}
}

// channelConflictMessage is a ChannelConflict as resolution words it, with a winner of the losing Binding's own namespace.
const channelConflictMessage = "spec.chat.channel: C1 on ChatConnection sre/slack is already bound by Binding sre/winner"

func reasonsOf(rs []config.Rejection) []config.RejectionReason {
	var out []config.RejectionReason
	for _, r := range rs {
		out = append(out, r.Reason)
	}
	return out
}

func permutations(in []config.Rejection) [][]config.Rejection {
	if len(in) <= 1 {
		return [][]config.Rejection{in}
	}
	var out [][]config.Rejection
	for i := range in {
		rest := slices.Concat(in[:i:i], in[i+1:])
		for _, p := range permutations(rest) {
			out = append(out, append([]config.Rejection{in[i]}, p...))
		}
	}
	return out
}

// What a status says is redacted and cut to a line: a message that holds a credential shows none, and a long one is cut.
func TestMessagesAreRedactedAndCut(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, nil)
	r.reject(config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonBackendNotFound, Generation: 1,
		Message: "spec.agent.backendRef: password=hunter2-not-real and " + strings.Repeat("padding ", 400)})
	r.reconcile()
	msg := c.stored(t, sreAlerts)["Accepted"].Message
	if strings.Contains(msg, "hunter2") {
		t.Errorf("the message shows the credential: %q", msg)
	}
	if len(msg) > maxStatusMessage {
		t.Errorf("the message is %d bytes, want at most %d", len(msg), maxStatusMessage)
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("the message has a line break: %q", msg)
	}

	// A message of the Prober is redacted too, though it should be so already.
	r2 := newStatusRig(t, newStatusCluster(t, storedBinding(sreAlerts, 1)), map[string]int64{sreAlerts: 1})
	conds := probe(1)
	conds[0].Status, conds[0].Reason, conds[0].Message = bindingcheck.False, bindingcheck.ReasonUnreachable, "token=hunter3-not-real"
	r2.set(sreAlerts, conds...)
	r2.reconcile()
	if got := r2.cluster.stored(t, sreAlerts)["BackendReachable"].Message; strings.Contains(got, "hunter3") {
		t.Errorf("BackendReachable message shows the credential: %q", got)
	}
}

// The writer writes only the types it owns, which are fewer than the 16 that the schema takes, and a type that is not one of them,
// from whatever the Prober holds, is left out: a status of 17 conditions would be refused whole.
func TestOnlyTheOwnedTypesAreWrittenAndTheyAreUnderTheSchemasCap(t *testing.T) {
	if len(statusTypes) > maxStatusConditions {
		t.Fatalf("the writer owns %d types, the schema takes %d", len(statusTypes), maxStatusConditions)
	}
	if maxStatusConditions != 16 {
		t.Errorf("maxStatusConditions = %d: the schema caps conditions at 16 (BindingStatus.Conditions MaxItems)", maxStatusConditions)
	}
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	conds := probe(1)
	for i := range 20 {
		conds = append(conds, bindingcheck.Condition{Type: bindingcheck.ConditionType(fmt.Sprintf("Extra%d", i)), Status: bindingcheck.True, Reason: bindingcheck.ReasonOK, ObservedGeneration: 1})
	}
	r.set(sreAlerts, conds...)
	r.reconcile()
	got := c.last(t).conds()
	if len(got) != len(statusTypes)-1 { // ApproversReadable is not set: nothing sets it yet
		t.Errorf("conditions written = %d, want %d", len(got), len(statusTypes)-1)
	}
	for typ := range got {
		if strings.HasPrefix(typ, "Extra") {
			t.Errorf("%s was written: it is not a type the writer owns", typ)
		}
	}
}

// A status write runs every rule of the schema and carries a Warning for each that the stored spec breaks. The writer counts them
// and says so once for the Binding, however many writes follow, and never lets them reach the log one by one.
func TestWarningsOfAStoredInvalidSpecAreCountedAndSaidOnce(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	c.warnN = 40
	for i := range 4 {
		conds := probe(1)
		conds[0].Status, conds[0].Reason = bindingcheck.False, bindingcheck.ReasonAgentNotFound
		conds[0].Message = fmt.Sprintf("answer %d", i) // every pass changes what is written
		r.set(sreAlerts, conds...)
		r.reconcile()
		r.advance(time.Minute)
	}
	if c.count() != 4 {
		t.Fatalf("applies = %d, want 4", c.count())
	}
	if n := strings.Count(r.logs.String(), "the API server warned"); n != 1 {
		t.Errorf("warnings were said %d times for 160 of them, want once:\n%s", n, r.logs)
	}
	if !strings.Contains(r.logs.String(), "warnings_per_write=40") {
		t.Errorf("the line does not say how many:\n%s", r.logs)
	}
	var h warningCounter
	for range 100 {
		h.HandleWarningHeader(299, "kube-apiserver", "a rule is broken")
	}
	h.HandleWarningHeader(199, "x", "not a warning of this kind")
	if n, first := h.take(); n != 100 || first != "a rule is broken" {
		t.Errorf("take = %d %q, want 100 and the first text", n, first)
	}
	if n, _ := h.take(); n != 0 {
		t.Errorf("take twice = %d, want it to start again", n)
	}
	if n, _ := (*warningCounter)(nil).take(); n != 0 {
		t.Error("a nil counter has warnings")
	}
}

// A Binding that leaves the report is forgotten, and one that comes back is a new one: it is compared with the cluster again.
func TestABindingThatLeavesTheReportIsForgotten(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...)
	r.reconcile()
	r.serve(map[string]int64{"sre/other": 1})
	r.reconcile()
	if _, ok := r.w.keys[sreAlerts]; ok {
		t.Error("the writer keeps a Binding that is no longer in the report")
	}
}

// Nothing is written while no config has loaded: an empty report is not every Binding removed, and writes nothing for one.
func TestNothingIsWrittenBeforeTheConfigHasLoaded(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.mu.Lock()
	r.report = config.Report{}
	r.mu.Unlock()
	r.reconcile()
	if c.count() != 0 {
		t.Errorf("applies = %d with no snapshot, want none", c.count())
	}
}

// The loop reconciles when it is told and at each tick, and ends with its context, with nothing left of it.
func TestRunWritesWhenToldAndEndsWithItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newStatusCluster(t, storedBinding(sreAlerts, 1))
		r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
		r.w.cfg.Now = time.Now // the bubble's clock
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); r.w.Run(ctx) }()
		synctest.Wait()
		if c.count() != 1 {
			t.Fatalf("applies at start = %d, want 1: the first pass writes a Binding that has no status", c.count())
		}
		change := func(reason bindingcheck.Reason) {
			conds := probe(1)
			conds[0].Status, conds[0].Reason = bindingcheck.False, reason
			conds[2].Status, conds[2].Reason = bindingcheck.False, "BackendReachable:"+reason
			r.mu.Lock()
			r.probed[sreAlerts] = conds
			r.mu.Unlock()
		}
		change(bindingcheck.ReasonAgentNotFound)
		time.Sleep(statusGap + time.Second) // a tick passes with no word from the Prober, and the gap is over
		synctest.Wait()
		if c.count() != 2 {
			t.Fatalf("applies = %d after a tick, want 2: the loop looks at each tick", c.count())
		}
		time.Sleep(statusGap + time.Second) // nothing changed: nothing is written, and the gap is over again
		synctest.Wait()
		change(bindingcheck.ReasonAgentNotReady)
		r.w.notify()
		synctest.Wait() // no time passes: only a word from the Prober can have woken it
		if c.count() != 3 {
			t.Errorf("applies after notify = %d, want 3", c.count())
		}
		cancel()
		<-done
	})
}

// The hooks of the Prober tell the writer, and a verification without one tells nothing and does not fail: that is the files route.
func TestVerificationTellsTheStatusWriterOfTransitionsAndPasses(t *testing.T) {
	r := newStatusRig(t, newStatusCluster(t), nil)
	v := verification{Status: r.w}
	if len(r.w.wake) != 0 {
		t.Fatal("a new writer has been told something moved")
	}
	v.notifyStatus()
	if len(r.w.wake) != 1 {
		t.Error("notifyStatus did not tell the writer")
	}
	verification{}.notifyStatus() // no writer: nothing to tell, and no panic
}

// A writer is not built without what it needs, and the error names what is missing.
func TestNewStatusWriterNamesWhatIsMissing(t *testing.T) {
	ok := statusConfig{Client: crClusterOf(), Report: func() config.Report { return config.Report{} },
		Conditions: func() map[string][]bindingcheck.Condition { return nil }, Log: slog.New(slog.DiscardHandler),
		Observed: func(string, string) (crsource.BindingStatus, bool, error) {
			return crsource.BindingStatus{}, false, nil
		}}
	for name, tc := range map[string]struct {
		edit func(*statusConfig)
		want string
	}{
		"client":     {func(c *statusConfig) { c.Client = nil }, "Client"},
		"report":     {func(c *statusConfig) { c.Report = nil }, "Report"},
		"conditions": {func(c *statusConfig) { c.Conditions = nil }, "Conditions"},
		"observed":   {func(c *statusConfig) { c.Observed = nil }, "Observed"},
		"log":        {func(c *statusConfig) { c.Log = nil }, "Log"},
	} {
		t.Run(name, func(t *testing.T) {
			c := ok
			tc.edit(&c)
			if _, err := newStatusWriter(c); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %s", err, tc.want)
			}
		})
	}
	if _, err := newStatusWriter(ok); err != nil {
		t.Errorf("a complete config: %v", err)
	}
}

// A condition that is Pending when it is written says it of the generation served, not of an older one that the Prober saw the
// Binding at when it first listed it: Pending is the truth about the generation that is served now.
func TestAPendingConditionIsWrittenAtTheGenerationServed(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 2))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 2})
	r.set(sreAlerts, probe(1, bindingcheck.BackendReachable, bindingcheck.ChannelJoined)...) // listed at generation 1, edited since
	r.reconcile()
	got := c.stored(t, sreAlerts)
	for _, typ := range []string{"BackendReachable", "ChannelJoined", "Ready"} {
		if got[typ].ObservedGeneration != 2 || got[typ].Status != metav1.ConditionUnknown {
			t.Errorf("%s = %s at generation %d, want Unknown at 2", typ, got[typ].Status, got[typ].ObservedGeneration)
		}
	}
}

// The end of a pass tells the status writer: a probe that finds what it found before is no transition, and moves only the
// generation it observed, which the status must say. The first pass is told by its transitions as well, so the writer's flag is
// cleared and the next pass, which has none, is what sets it.
func TestTheBrokersProberTellsTheStatusWriterAtTheEndOfEachPass(t *testing.T) {
	current := &snaps{}
	current.p.Store(twoConnections(t, "ops/oncall"))
	var calls atomic.Int64
	r := newStatusRig(t, newStatusCluster(t), nil)
	p := verification{
		Bindings: probeFunc(func(context.Context, string) error { calls.Add(1); return nil }), Platform: &accessFake{access: chat.Access{Member: true}},
		Snaps: current, Revision: func(context.Context) string { return "r" }, Status: r.w,
		Conn:    Connection{Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-a"}},
		Metrics: mustMetrics(t, nil), Log: slog.New(slog.DiscardHandler), Interval: 20 * time.Millisecond, Tick: 2 * time.Millisecond,
	}.newProber()
	runProber(t, p)
	waitSettled(t, p, 1)
	eventually(t, 10*time.Second, "no second pass", func() bool { return calls.Load() >= 3 })
	select { // clear what the transitions of the first pass said
	case <-r.w.wake:
	default:
	}
	eventually(t, 10*time.Second, "the end of a pass without a transition did not tell the writer", func() bool { return len(r.w.wake) == 1 })
}

// The Prober is given each Binding's generation, so that what it stamps is the generation the Binding had when the pass began.
func TestBindingTargetsCarryTheGeneration(t *testing.T) {
	got := bindingTargets(snapshotWithUIDs(t, map[string]int64{sreAlerts: 7}, map[string]string{sreAlerts: "uid-7"}), v1alpha1.ObjectRef{Namespace: "sre", Name: "slack"})
	if want := []bindingcheck.Target{{Binding: sreAlerts, Channel: "C-alerts", Generation: 7, UID: "uid-7"}}; !slices.Equal(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
}

// A probed condition that is not about this object is no finding about it: one that the Prober made for another object of the name
// (deleted and made again, with a new uid) or at a generation above the one served is Pending, at the generation served. What
// the status says is never of a generation above the one served, and a kubectl wait that takes a True of generation 7 for the object
// at generation 1 would say the condition was met.
func TestAProbedConditionThatIsNotAboutThisObjectIsPending(t *testing.T) {
	for name, tc := range map[string]struct {
		condGen  int64
		condUID  string
		wantTrue bool
	}{
		"a generation above the one served": {7, "uid-new", false},
		"another object":                    {1, "uid-old", false},
		"this object":                       {1, "uid-new", true},
	} {
		t.Run(name, func(t *testing.T) {
			c := newStatusCluster(t, withUID(storedBinding(sreAlerts, 1), "uid-new"))
			r := newStatusRig(t, c, nil)
			r.serveWith(map[string]int64{sreAlerts: 1}, map[string]string{sreAlerts: "uid-new"})
			conds := probe(tc.condGen)
			for i := range conds {
				conds[i].UID = tc.condUID
			}
			r.set(sreAlerts, conds...)
			r.reconcile()
			for typ, got := range c.stored(t, sreAlerts) {
				if got.ObservedGeneration > 1 {
					t.Errorf("%s observedGeneration = %d, want it never above the generation served, 1", typ, got.ObservedGeneration)
				}
				if typ == "Accepted" {
					continue
				}
				if isTrue := got.Status == metav1.ConditionTrue; isTrue != tc.wantTrue {
					t.Errorf("%s = %s/%s, want True %v", typ, got.Status, got.Reason, tc.wantTrue)
				}
			}
		})
	}
}

// What a ChannelConflict says to the tenant who lost the channel: not which Binding of another namespace holds it, which the tenant
// has no right to know. A winner in the tenant's own namespace is named, and the rest of the message is as resolution wrote it
// (the log keeps all of it, and so does the report). The sentence is the one that resolution really writes.
func TestAChannelConflictDoesNotNameABindingOfAnotherNamespace(t *testing.T) {
	conflict := func(winnerNamespace string) config.Rejection {
		t.Helper()
		binding := func(ns, name string, created time.Time) v1alpha1.Binding {
			b := v1alpha1.Binding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(created)}}
			b.Spec.Agent.BackendRef = v1alpha1.ObjectRef{Namespace: "platform", Name: "agent"}
			b.Spec.Chat.ConnectionRef = v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}
			b.Spec.Chat.Channel = "C0123456789"
			return b
		}
		set := &config.Set{
			AgentBackends: []v1alpha1.AgentBackend{{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "agent"},
				Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A, AllowedNamespaces: []string{"team-a", "team-b"}}}},
			ChatConnections: []v1alpha1.ChatConnection{{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "slack"},
				Spec: v1alpha1.ChatConnectionSpec{AllowedNamespaces: []string{"team-a", "team-b"}}}},
			Bindings: []v1alpha1.Binding{binding(winnerNamespace, "secret-project-payroll", statusT0), binding("team-a", "alerts", statusT0.Add(time.Hour))},
		}
		_, rejected := config.ResolvePartial(set)
		for _, r := range rejected {
			if r.Reason == config.ReasonChannelConflict && r.Namespace == "team-a" && r.Name == "alerts" {
				return r
			}
		}
		t.Fatalf("resolution did not reject team-a/alerts for its channel: %+v", rejected)
		return config.Rejection{}
	}

	other := conflict("team-b")
	if !strings.Contains(other.Message, "team-b/secret-project-payroll") {
		t.Fatalf("resolution's message = %q: the sentence this test pins has changed", other.Message)
	}
	got := acceptedCondition([]config.Rejection{other}, 0)
	if strings.Contains(got.Message, "team-b") || strings.Contains(got.Message, "payroll") {
		t.Errorf("the status says %q: it names a Binding of another namespace", got.Message)
	}
	if want := "spec.chat.channel: C0123456789 on ChatConnection platform/slack is already bound by a Binding in another namespace"; got.Message != want {
		t.Errorf("message = %q, want %q", got.Message, want)
	}
	if !strings.Contains(other.Message, "team-b/secret-project-payroll") {
		t.Error("the Rejection itself was changed: the log and the report keep the whole text")
	}

	own := conflict("team-a")
	if got := acceptedCondition([]config.Rejection{own}, 0); got.Message != own.Message || !strings.Contains(got.Message, "team-a/secret-project-payroll") {
		t.Errorf("same-namespace message = %q, want resolution's, naming the winner", got.Message)
	}
}

// The writer that looks before the Prober's first Tick finds no probed conditions, which are Pending and not gone: SSA would remove
// what it left out. A good status is kept (Pending is carried over it), and one that has none gets Pending for each.
func TestAServedBindingTheProberHasNotListedYetIsPendingNotGone(t *testing.T) {
	good := []metav1.Condition{
		cond("Accepted", metav1.ConditionTrue, "OK", 1, statusT0), cond("BackendReachable", metav1.ConditionTrue, "OK", 1, statusT0),
		cond("ChannelJoined", metav1.ConditionTrue, "OK", 1, statusT0), cond("Ready", metav1.ConditionTrue, "OK", 1, statusT0),
	}
	c := newStatusCluster(t, storedBinding(sreAlerts, 1, good...))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1}) // the Prober has found nothing
	r.reconcile()
	if c.count() != 0 {
		t.Errorf("applies = %d, want none: a good status is not removed because the Prober has not listed the Binding", c.count())
	}
	if got := c.stored(t, sreAlerts); len(got) != 4 || got["BackendReachable"].Status != metav1.ConditionTrue {
		t.Errorf("status = %+v, want it kept", got)
	}

	c = newStatusCluster(t, storedBinding(sreAlerts, 1))
	r = newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.reconcile()
	got := c.stored(t, sreAlerts)
	for _, typ := range []string{"BackendReachable", "ChannelJoined", "Ready"} {
		if got[typ].Status != metav1.ConditionUnknown || got[typ].Reason != "Pending" || got[typ].ObservedGeneration != 1 {
			t.Errorf("%s = %s/%s at %d, want Unknown/Pending at 1", typ, got[typ].Status, got[typ].Reason, got[typ].ObservedGeneration)
		}
	}
}

// timedClient is a dynamic client that notes the deadline of each apply, and holds those of the Bindings that hang until their
// context ends.
type timedClient struct {
	dynamic.Interface
	hang      func(name string) bool
	slow      func() // called as each apply begins, for a test that moves a clock
	uncut     bool   // a call that should have been cut had to be let go after two seconds
	mu        sync.Mutex
	deadlines []time.Duration // the time left when each apply began, 0 for a call with no deadline
}

func (c *timedClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return timedResource{c.Interface.Resource(gvr), c}
}

type timedResource struct {
	dynamic.NamespaceableResourceInterface
	c *timedClient
}

func (r timedResource) Namespace(ns string) dynamic.ResourceInterface {
	return timedNamespace{r.NamespaceableResourceInterface.Namespace(ns), r.c}
}

type timedNamespace struct {
	dynamic.ResourceInterface
	c *timedClient
}

func (n timedNamespace) ApplyStatus(ctx context.Context, name string, obj *unstructured.Unstructured, opts metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	left := time.Duration(0)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	n.c.mu.Lock()
	n.c.deadlines = append(n.c.deadlines, left)
	n.c.mu.Unlock()
	if n.c.slow != nil {
		n.c.slow()
	}
	if n.c.hang != nil && n.c.hang(name) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second): // a call with no deadline would wait for ever
			n.c.mu.Lock()
			n.c.uncut = true
			n.c.mu.Unlock()
			return nil, errors.New("the call was not cut: it had no deadline")
		}
	}
	return n.ResourceInterface.ApplyStatus(ctx, name, obj, opts)
}

// A call to the API server is cut at the timeout, which is less than the 5 s that the lease leaves between its renew deadline and its
// duration, so that a write that the fence let through at the deadline ends before another replica leads; and one request that hangs
// holds up no other Binding's write, and is a failed write like any other.
func TestAWriteIsCutAtTheTimeoutAndAHungOneHoldsUpNoOther(t *testing.T) {
	// The timeout a leader uses stays inside the margin its election leaves, whatever the chart's values: half of it, or the default.
	for name, tc := range map[string]struct {
		election ElectionConfig
		elected  bool
		want     time.Duration
	}{
		"no election (SQLite)":    {ElectionConfig{}, false, statusTimeout},
		"the defaults":            {ElectionConfig{}, true, 2500 * time.Millisecond},
		"a wide margin":           {ElectionConfig{LeaseDuration: 60 * time.Second, RenewDeadline: 10 * time.Second}, true, statusTimeout},
		"a margin of two seconds": {ElectionConfig{LeaseDuration: 12 * time.Second, RenewDeadline: 10 * time.Second}, true, time.Second},
		"a margin of one second":  {ElectionConfig{LeaseDuration: 11 * time.Second, RenewDeadline: 10 * time.Second}, true, 500 * time.Millisecond},
	} {
		if got := statusCallTimeout(tc.election, tc.elected); got != tc.want {
			t.Errorf("%s: statusCallTimeout = %v, want %v", name, got, tc.want)
		}
		if e := tc.election.withDefaults(); tc.elected && statusCallTimeout(tc.election, true) >= e.LeaseDuration-e.RenewDeadline {
			t.Errorf("%s: the timeout is not under the margin of the election", name)
		}
	}
	c := newStatusCluster(t, storedBinding("sre/a", 1), storedBinding("sre/b", 1))
	r := newStatusRig(t, c, map[string]int64{"sre/a": 1, "sre/b": 1})
	timed := &timedClient{Interface: c.dyn, hang: func(name string) bool { return name == "a" }}
	r.w.cfg.Client, r.w.cfg.Timeout = timed, 30*time.Millisecond
	r.set("sre/a", probe(1)...)
	r.set("sre/b", probe(1)...)
	r.reconcile()

	timed.mu.Lock()
	deadlines, uncut := slices.Clone(timed.deadlines), timed.uncut
	timed.mu.Unlock()
	if uncut {
		t.Fatal("a call that hung was not cut by a deadline of the writer's own")
	}
	if len(deadlines) != 2 {
		t.Fatalf("applies attempted = %d, want 2: the hung one did not stop the other", len(deadlines))
	}
	for _, d := range deadlines {
		if d <= 0 || d > 30*time.Millisecond {
			t.Errorf("an apply began with %v left, want a deadline of the timeout", d)
		}
	}
	if got := c.stored(t, "sre/b")["Accepted"]; got.Status != metav1.ConditionTrue {
		t.Errorf("b's Accepted = %+v, want it written", got)
	}
	if want := []string{"failed", "written"}; !slices.Equal(r.writes, want) {
		t.Errorf("results = %v, want %v", r.writes, want)
	}

	// The default is the constant, and there is a deadline on the real call.
	w, err := newStatusWriter(statusConfig{Client: c.dyn, Report: r.w.cfg.Report, Conditions: r.w.cfg.Conditions, Observed: c.observed, Log: slog.New(slog.DiscardHandler)})
	if err != nil || w.cfg.Timeout != statusTimeout {
		t.Errorf("default timeout = %v (%v), want %v", w.cfg.Timeout, err, statusTimeout)
	}
}

// When a condition heals it is dated when the writer saw it heal, not by the Prober's own time, which can be older than the status it
// replaces: a condition that was False at one hour and True at the next must not say it has been True since before it was False.
func TestAHealedConditionIsDatedWhenItHealed(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1,
		cond("Accepted", metav1.ConditionTrue, "OK", 1, statusT0), cond("BackendReachable", metav1.ConditionFalse, "AgentNotFound", 1, statusT0.Add(time.Hour)),
		cond("ChannelJoined", metav1.ConditionTrue, "OK", 1, statusT0), cond("Ready", metav1.ConditionFalse, "BackendReachable:AgentNotFound", 1, statusT0.Add(time.Hour))))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...) // True, by the Prober's account since a minute after the start
	r.advance(2 * time.Hour)
	r.reconcile()
	healed := statusT0.Add(2 * time.Hour).Format(time.RFC3339)
	got := c.last(t).conds()
	if got["BackendReachable"]["lastTransitionTime"] != healed || got["Ready"]["lastTransitionTime"] != healed {
		t.Errorf("healed at %v and %v, want %s", got["BackendReachable"]["lastTransitionTime"], got["Ready"]["lastTransitionTime"], healed)
	}
	if got["ChannelJoined"]["lastTransitionTime"] != statusT0.Format(time.RFC3339) {
		t.Errorf("ChannelJoined lastTransitionTime = %v, want %s: it did not change", got["ChannelJoined"]["lastTransitionTime"], statusT0.Format(time.RFC3339))
	}
}

// A probe that finds the same with another cause (a connection that reset, on another ephemeral port each pass) is no change of the
// status: the cause is for the log, the message is one sentence, and nothing is written for it.
func TestAProbeThatFindsTheSameWithAnotherCauseWritesNothing(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	failing := func(port string) []bindingcheck.Condition {
		conds := probe(1)
		conds[0].Status, conds[0].Reason, conds[0].Message = bindingcheck.False, bindingcheck.ReasonUnreachable, msgCardRequestFailed
		conds[0].Cause = "read tcp 10.1.2.3:" + port + "->10.0.0.5:8080: read: connection reset by peer"
		conds[2].Status, conds[2].Reason, conds[2].Message = bindingcheck.False, "BackendReachable:Unreachable", msgCardRequestFailed
		return conds
	}
	r.set(sreAlerts, failing("48122")...)
	r.reconcile()
	for i := range 5 {
		r.advance(time.Minute)
		r.set(sreAlerts, failing(fmt.Sprint(50000+i))...)
		r.reconcile()
	}
	if c.count() != 1 {
		t.Errorf("applies = %d across passes that differ only in the port, want 1", c.count())
	}
	if msg := c.stored(t, sreAlerts)["BackendReachable"].Message; strings.Contains(msg, "10.") || strings.Contains(msg, "48122") {
		t.Errorf("the status says %q: it carries an address or a port", msg)
	}
}

// The message of a status is at most maxStatusMessage bytes whatever it started as, redaction that lengthens text included, is
// valid UTF-8 and one line, and is cut with a mark; a short one is as it was.
func TestAMessageIsAtMostTheCapAfterRedaction(t *testing.T) {
	for name, in := range map[string]string{
		"long words":      strings.Repeat("padding ", 1000),
		"redaction grows": strings.Repeat("access_token=abc ", 300),
		"multibyte":       strings.Repeat("é ", 3000),
		"lines":           strings.Repeat("a line\nanother line\n", 400),
	} {
		got := statusMessage(in)
		if len(got) > maxStatusMessage || !utf8.ValidString(got) || strings.Contains(got, "\n") {
			t.Errorf("%s: %d bytes, valid %v, one line %v; want at most %d, valid and one line", name, len(got), utf8.ValidString(got), !strings.Contains(got, "\n"), maxStatusMessage)
		}
		if !strings.HasSuffix(got, ellipsis) && !strings.HasSuffix(got, "(truncated)") {
			t.Errorf("%s: %q does not end in the mark of a cut", name, got[max(0, len(got)-20):])
		}
	}
	if got := statusMessage("a short one"); got != "a short one" {
		t.Errorf("a short message = %q", got)
	}
	if statusMessage("") != "" {
		t.Error("an empty message is not empty")
	}
}

// The gap between two writes of a Binding is counted from the end of a write, not from the start of the pass: a write that takes
// 5 s, in a pass that began at 0, leaves the Binding closed to another until 15 s, and one at 12 s is held back.
func TestTheGapIsCountedFromTheEndOfAWrite(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	timed := &timedClient{Interface: c.dyn, slow: func() { r.advance(5 * time.Second) }}
	r.w.cfg.Client = timed
	r.set(sreAlerts, probe(1)...)
	r.reconcile() // begins at 0, ends at 5 s
	r.advance(7 * time.Second)
	conds := probe(1)
	conds[0].Status, conds[0].Reason = "False", "AgentNotFound"
	conds[2].Status, conds[2].Reason = "False", "BackendReachable:AgentNotFound"
	r.set(sreAlerts, conds...)
	timed.slow = nil
	r.reconcile() // 12 s: inside the gap of the write that ended at 5 s
	if c.count() != 1 {
		t.Fatalf("applies = %d at 12 s, want 1: the gap runs from the end of the first write", c.count())
	}
	r.advance(4 * time.Second)
	r.reconcile() // 16 s
	if c.count() != 2 {
		t.Errorf("applies = %d at 16 s, want 2", c.count())
	}
}

// A reporter puts back the words of a rejection that can only be its own: a Binding that is refused for a backend that is not found
// (or a channel that is taken, a namespace that is not allowed) names a connection that exists, so only this broker reports it, and
// another broker's stray ConnectionNotFound must not stay over it. A reporter of a refused spec (InvalidSpec) or of a connection
// that does not exist (ConnectionNotFound) puts nothing back, since another broker may report those too and the two would go on
// at each other: the stray words stay until its own change.
func TestAReporterPutsBackTheWordsOfARejectionThatIsItsOwn(t *testing.T) {
	stray := config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonConnectionNotFound, Generation: 1, Message: "spec.chat.connectionRef: ChatConnection sre/slack not found"}
	for name, tc := range map[string]struct {
		own        config.Rejection
		wantReason string
		applies    int
	}{
		"a backend that is not found": {config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonBackendNotFound, Generation: 1, Message: "spec.agent.backendRef: AgentBackend sre/agent not found"}, "BackendNotFound", 3},
		"a spec that is refused":      {config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonInvalidSpec, Generation: 1, Message: "spec: generation 1 is invalid"}, "ConnectionNotFound", 2},
	} {
		t.Run(name, func(t *testing.T) {
			c := newStatusCluster(t, storedBinding(sreAlerts, 1))
			mine := newStatusRig(t, c, nil)
			mine.reject(tc.own)
			other := newStatusRig(t, c, nil)
			other.reject(stray)
			mine.reconcile()
			other.reconcile() // its poll fell in the gap of a connection that was made again, and it writes over the words
			if got := c.stored(t, sreAlerts)["Accepted"]; got.Reason != "ConnectionNotFound" {
				t.Fatalf("after the stray: Accepted = %+v, want its ConnectionNotFound", got)
			}
			for range 30 {
				for _, r := range []*statusRig{mine, other} {
					r.advance(11 * time.Second)
					r.reconcile()
				}
			}
			if got := c.stored(t, sreAlerts)["Accepted"]; got.Reason != tc.wantReason || c.count() != tc.applies {
				t.Errorf("Accepted = %s after thirty passes, applies = %d; want %s and %d", got.Reason, c.count(), tc.wantReason, tc.applies)
			}
		})
	}
}

// A status that is put back is counted apart from a first write (result "put_back"), and a Binding whose status is put back three
// times in a row, each within a few gaps of the last, is said once, by name, with what it may be: another broker that serves it, or a
// controller that writes its status. One edit by a person is put back once and is no fight; two far apart are none either.
func TestAFightOverTheStatusIsCountedAndSaidOnce(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	a := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	a.set(sreAlerts, probe(1)...)
	b := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	failing := probe(1)
	failing[0].Status, failing[0].Reason = bindingcheck.False, bindingcheck.ReasonAgentNotFound
	failing[2].Status, failing[2].Reason = bindingcheck.False, "BackendReachable:AgentNotFound"
	b.set(sreAlerts, failing...) // another broker that serves the same Binding, and finds otherwise
	for range 8 {
		for _, r := range []*statusRig{a, b} {
			r.advance(11 * time.Second)
			r.reconcile()
		}
	}
	for name, r := range map[string]*statusRig{"a": a, "b": b} {
		if !slices.Contains(r.writes, "put_back") {
			t.Errorf("%s: results = %v, want put-backs counted", name, r.writes)
		}
		if n := strings.Count(r.logs.String(), "keeps changing the status of a Binding"); n != 1 || !strings.Contains(r.logs.String(), "binding=sre/alerts") {
			t.Errorf("%s: the fight was said %d times, want once and naming the Binding:\n%s", name, n, r.logs)
		}
	}

	t.Run("a person's edit", func(t *testing.T) {
		c := newStatusCluster(t, storedBinding(sreAlerts, 1))
		r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
		r.set(sreAlerts, probe(1)...)
		r.reconcile()
		for range 4 { // four edits, each far from the last
			edited := storedBinding(sreAlerts, 1, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ByHand", LastTransitionTime: metav1.NewTime(statusT0)})
			if err := c.dyn.Tracker().Update(crBindings, edited, "sre"); err != nil {
				t.Fatal(err)
			}
			r.advance(11 * time.Second)
			r.reconcile()
			for range 10 {
				r.advance(time.Minute)
				r.reconcile()
			}
		}
		if want := []string{"written", "put_back", "put_back", "put_back", "put_back"}; !slices.Equal(r.writes, want) {
			t.Errorf("results = %v, want %v", r.writes, want)
		}
		if strings.Contains(r.logs.String(), "keeps changing") {
			t.Errorf("edits far apart were called a fight:\n%s", r.logs)
		}
	})
}

// A condition of a type that the writer does not write, left by someone else (ApproversReadable, which nothing sets), is no
// difference: it can never be equal to what is wanted and SSA cannot remove it, so comparing it would write for ever. A type that
// the writer wrote and no longer says (the probes of a Binding that stopped being served) is a difference, since SSA removes it.
func TestAConditionOfATypeTheWriterDoesNotWriteIsNoDifference(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1,
		cond("ApproversReadable", metav1.ConditionFalse, "GroupNotFound", 1, statusT0)))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...)
	r.reconcile()
	for range 30 {
		r.advance(time.Minute)
		r.reconcile()
	}
	if c.count() != 1 {
		t.Errorf("applies = %d, want 1: a foreign ApproversReadable is not the writer's to compare", c.count())
	}

	// The Binding stops being served: what was written and is no longer said is a difference, and goes.
	c = newStatusCluster(t, storedBinding(sreAlerts, 1))
	r = newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...)
	r.reconcile()
	r.serve(nil)
	r.reject(config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonBackendNotFound, Generation: 1, Message: "spec.agent.backendRef: not found"})
	r.advance(11 * time.Second)
	r.reconcile()
	if got := c.stored(t, sreAlerts); len(got) != 2 || got["Accepted"].Status != metav1.ConditionFalse {
		t.Errorf("status = %v, want Accepted and Ready only: the probes of a Binding that is not served are removed", got)
	}
}

// The writer makes a whole pass only if something moved: the Prober's hook, a report that is another, an event of the Binding
// informer, a write that was held back and is due, or the slow pass. A tick with none of them reads nothing, which at thousands of
// Bindings is a core's tenth saved for nothing to do.
func TestAPassIsMadeOnlyIfSomethingMoved(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	var reads, events atomic.Int64
	r.w.cfg.Observed = func(ns, name string) (crsource.BindingStatus, bool, error) {
		reads.Add(1)
		return c.observed(ns, name)
	}
	r.w.cfg.Version = func() uint64 { return uint64(events.Load()) }
	r.w.cfg.Slow = time.Minute
	r.set(sreAlerts, probe(1)...)
	r.reconcile()
	steps := map[string]func(){
		"a tick with nothing moved":   func() { r.advance(time.Second) },
		"another tick":                func() { r.advance(time.Second) },
		"a tick before the slow pass": func() { r.advance(30 * time.Second) },
		"a Binding event":             func() { events.Add(1) },
		"the Prober's hook":           r.w.notify,
		"a report that is another":    func() { r.serve(map[string]int64{sreAlerts: 1}) },
		"the slow pass":               func() { r.advance(time.Minute) },
	}
	moves := map[string]bool{"a Binding event": true, "the Prober's hook": true, "a report that is another": true, "the slow pass": true}
	for _, name := range []string{"a tick with nothing moved", "another tick", "a tick before the slow pass", "a Binding event", "the Prober's hook", "a report that is another", "the slow pass"} {
		before := reads.Load()
		steps[name]()
		r.reconcile()
		if moved := reads.Load() > before; moved != moves[name] {
			t.Errorf("%s: a pass was made %v, want %v", name, moved, moves[name])
		}
	}

	// A write that was held back by the gap is due before the slow pass, and is tried then.
	before := reads.Load()
	conds := probe(1)
	conds[0].Status, conds[0].Reason = bindingcheck.False, bindingcheck.ReasonAgentNotFound
	conds[2].Status, conds[2].Reason = bindingcheck.False, "BackendReachable:AgentNotFound"
	r.advance(15 * time.Second)
	r.set(sreAlerts, probe(1)...) // a pass at once (the hook): it writes nothing new
	r.reconcile()
	applies := c.count()
	r.set(sreAlerts, conds...)
	r.w.cfg.Slow = time.Hour
	r.reconcile() // the hook moved it; the gap of the last write (the first) is over, so it writes
	_ = before
	if c.count() != applies+1 {
		t.Fatalf("applies = %d, want %d", c.count(), applies+1)
	}
	conds[0].Reason = bindingcheck.ReasonAgentNotReady
	conds[2].Reason = "BackendReachable:AgentNotReady"
	r.set(sreAlerts, conds...)
	r.reconcile() // held back by the gap: nothing written, and a retry is due when the gap ends
	if c.count() != applies+1 {
		t.Fatalf("applies = %d, want %d: the gap holds it", c.count(), applies+1)
	}
	r.advance(5 * time.Second)
	reads.Store(0)
	r.reconcile()
	if reads.Load() != 0 || c.count() != applies+1 {
		t.Fatalf("reads %d, applies %d: nothing is due yet", reads.Load(), c.count())
	}
	r.advance(6 * time.Second)
	r.reconcile()
	if c.count() != applies+2 {
		t.Errorf("applies = %d, want %d: the held-back write is due, though nothing else moved", c.count(), applies+2)
	}
}

// The date of a condition that the cache has not caught up with is the date this writer gave the same words before, and not the
// date of each write: with a watch that is behind, a change that is written again at each gap keeps the time it began.
func TestADateTheWriterGaveIsKeptWhileTheCacheIsBehind(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1,
		cond("Accepted", metav1.ConditionTrue, "OK", 1, statusT0), cond("BackendReachable", metav1.ConditionFalse, "AgentNotFound", 1, statusT0),
		cond("ChannelJoined", metav1.ConditionTrue, "OK", 1, statusT0), cond("Ready", metav1.ConditionFalse, "BackendReachable:AgentNotFound", 1, statusT0)))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	c.freeze(t, sreAlerts) // the cache says the backend is not found, however often it is told otherwise
	healed := statusT0.Add(time.Hour)
	r.advance(time.Hour)
	for i := range 4 {
		conds := probe(1)
		conds[1].Message = fmt.Sprint("channel answer ", i) // a real change at each gap, so that it is written again
		r.set(sreAlerts, conds...)
		r.reconcile()
		r.advance(11 * time.Second)
	}
	if c.count() != 4 {
		t.Fatalf("applies = %d, want 4", c.count())
	}
	for i, ap := range c.snapshotApplies() {
		got := ap.conds()["BackendReachable"]["lastTransitionTime"]
		if got != healed.Format(time.RFC3339) {
			t.Errorf("apply %d dated BackendReachable %v, want %s: the date this writer chose first", i, got, healed.Format(time.RFC3339))
		}
	}
}

// A ChannelConflict whose message no longer has the words that name the winner is not shown whole: the writer cannot tell which
// namespace the winner is in, so it names none.
func TestAChannelConflictFailsClosedWhenItsWordsMove(t *testing.T) {
	got := ownNamespaceText(config.Rejection{Namespace: "team-a", Name: "alerts", Reason: config.ReasonChannelConflict,
		Message: "spec.chat.channel: C1 is held by team-b/secret-project-payroll"})
	if strings.Contains(got, "team-b") || strings.Contains(got, "payroll") || !strings.Contains(got, "already bound by another Binding") {
		t.Errorf("message = %q, want one that names no Binding", got)
	}
}

// A Pending condition takes what the status says of it only when that is of the generation served: one of a generation above
// it cannot be of this object (a generation never goes down), and is not carried, nor is any written above the generation served.
func TestAStatusOfAGenerationAboveTheOneServedIsNotCarried(t *testing.T) {
	at := statusT0.Add(-time.Hour)
	c := newStatusCluster(t, storedBinding(sreAlerts, 4,
		cond("Accepted", metav1.ConditionTrue, "OK", 5, at), cond("BackendReachable", metav1.ConditionTrue, "OK", 5, at),
		cond("ChannelJoined", metav1.ConditionTrue, "OK", 5, at), cond("Ready", metav1.ConditionTrue, "OK", 5, at)))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 4})
	r.set(sreAlerts, probe(4, bindingcheck.BackendReachable, bindingcheck.ChannelJoined)...)
	r.reconcile()
	for typ, got := range c.stored(t, sreAlerts) {
		if got.ObservedGeneration > 4 {
			t.Errorf("%s observedGeneration = %d, want none above the generation served, 4", typ, got.ObservedGeneration)
		}
		if typ != "Accepted" && got.Status == metav1.ConditionTrue {
			t.Errorf("%s = True: a status of generation 5 was carried over for an object served at 4", typ)
		}
	}
}
