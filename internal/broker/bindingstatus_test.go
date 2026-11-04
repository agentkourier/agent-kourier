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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/redact"
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
	lists   int
	listErr error
	applied chan struct{} // told, without waiting, of each apply taken: for a test that runs a whole broker
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
	c.dyn.PrependReactor("list", "bindings", func(k8stesting.Action) (bool, runtime.Object, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lists++
		return c.listErr != nil, nil, c.listErr
	})
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
		u.Object["status"] = ap.Body["status"]
		if err := c.dyn.Tracker().Update(crBindings, u, ap.Namespace); err != nil {
			return true, nil, err
		}
		return true, u, nil
	})
	return c
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
	return byType(conditionsOf(obj.(*unstructured.Unstructured)))
}

// storedBinding is a Binding of the cluster at generation, with the status conds if there are any.
func storedBinding(key string, generation int64, conds ...metav1.Condition) *unstructured.Unstructured {
	ns, name, _ := strings.Cut(key, "/")
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.Group + "/" + v1alpha1.Version, "kind": "Binding",
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     map[string]any{},
	}}
	u.SetUID(types.UID("uid-" + key))
	u.SetGeneration(generation)
	if len(conds) > 0 {
		u.Object["status"] = statusObject(ns, name, conds).Object["status"]
	}
	return u
}

// cond is a condition of the cluster's status.
func cond(typ string, status metav1.ConditionStatus, reason string, generation int64, at time.Time) metav1.Condition {
	return metav1.Condition{Type: typ, Status: status, Reason: reason, ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(at)}
}

// snapshotOf resolves one Binding for each key, each at the generation given, with its own channel on one connection.
func snapshotOf(t *testing.T, generations map[string]int64) *config.Snapshot {
	t.Helper()
	set := &config.Set{
		AgentBackends:   []v1alpha1.AgentBackend{{ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "agent"}, Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A}}},
		ChatConnections: []v1alpha1.ChatConnection{{ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "slack"}}},
	}
	for _, key := range slices.Sorted(maps.Keys(generations)) {
		ns, name, _ := strings.Cut(key, "/")
		b := v1alpha1.Binding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: generations[key]}}
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
		Warnings:   cluster.warn, Log: slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		OnWrite: func(result string) { r.writes = append(r.writes, result) },
		Gap:     10 * time.Second, Tick: time.Second, Resync: 10 * time.Minute,
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
	// A probe that finds a thing false afterwards is written, and the time of the transition is the Prober's.
	bad := probe(4)
	bad[0].Status, bad[0].Reason, bad[0].LastTransitionTime = bindingcheck.False, bindingcheck.ReasonAgentNotFound, statusT0.Add(2*time.Minute)
	bad[2].Status, bad[2].Reason, bad[2].LastTransitionTime = bindingcheck.False, "BackendReachable:AgentNotFound", statusT0.Add(2*time.Minute)
	r.advance(time.Minute)
	r.set(sreAlerts, bad...)
	r.reconcile()
	if c.count() != 1 {
		t.Fatalf("applies after a failing probe = %d, want 1", c.count())
	}
	got := c.last(t).conds()
	if got["BackendReachable"]["status"] != "False" || got["BackendReachable"]["reason"] != "AgentNotFound" ||
		got["BackendReachable"]["lastTransitionTime"] != statusT0.Add(2*time.Minute).Format(time.RFC3339) {
		t.Errorf("BackendReachable = %v, want False/AgentNotFound at the transition", got["BackendReachable"])
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
		c := newStatusCluster(t) // the cluster no longer has the Binding the report has
		r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
		r.set(sreAlerts, probe(1)...)
		r.reconcile()
		for range 8 { // the source's next reload, up to 10 s away, drops the Binding from the report
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

// Nothing is written before the writer knows what the cluster holds: a list that fails must not turn into Pending over a good
// status, and the writer writes as soon as a list succeeds.
func TestNothingIsWrittenUntilTheClusterWasListed(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1,
		cond("Accepted", metav1.ConditionTrue, "OK", 1, statusT0), cond("BackendReachable", metav1.ConditionTrue, "OK", 1, statusT0),
		cond("ChannelJoined", metav1.ConditionTrue, "OK", 1, statusT0), cond("Ready", metav1.ConditionTrue, "OK", 1, statusT0)))
	c.listErr = apierrors.NewServiceUnavailable("the API server is down")
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1, bindingcheck.BackendReachable, bindingcheck.ChannelJoined)...)
	r.reconcile()
	r.advance(statusListRetry / 2)
	r.reconcile()
	if c.count() != 0 {
		t.Fatalf("applies = %d with no list, want none", c.count())
	}
	if c.lists != 1 {
		t.Errorf("lists = %d inside the retry wait, want 1", c.lists)
	}
	c.mu.Lock()
	c.listErr = nil
	c.mu.Unlock()
	r.advance(statusListRetry)
	r.reconcile()
	if c.lists != 2 || c.count() != 0 {
		t.Errorf("lists = %d, applies = %d; want the list to succeed and the good status to be kept (no write)", c.lists, c.count())
	}
}

// The status of an object that was deleted and made again holds nothing, and the writer, which wrote the old one, writes the new
// one at its next list. A status that someone changed by hand is not put back, which keeps two brokers that report one Binding
// from taking it from each other at every list.
func TestAStatusThatIsGoneIsWrittenAgainAndOneThatDiffersIsNot(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	r.set(sreAlerts, probe(1)...)
	r.reconcile()
	if c.count() != 1 {
		t.Fatalf("applies = %d, want 1", c.count())
	}

	// Someone edits a condition's message: not put back.
	edited := storedBinding(sreAlerts, 1, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", Message: "edited by hand", LastTransitionTime: metav1.NewTime(statusT0)})
	if err := c.dyn.Tracker().Update(crBindings, edited, "sre"); err != nil {
		t.Fatal(err)
	}
	r.advance(11 * time.Minute) // a resync passes
	r.reconcile()
	if c.count() != 1 {
		t.Errorf("applies after a list that finds a different status = %d, want 1: the writer's own words have not changed", c.count())
	}

	// The object is made again: no status at all.
	if err := c.dyn.Tracker().Update(crBindings, storedBinding(sreAlerts, 1), "sre"); err != nil {
		t.Fatal(err)
	}
	r.advance(11 * time.Minute)
	r.reconcile()
	if c.count() != 2 {
		t.Errorf("applies after a list that finds no status = %d, want 2: the status is written again", c.count())
	}
	if got := c.stored(t, sreAlerts)["Ready"]; got.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True again", got)
	}
}

// Two writers over one Binding that say different things: the first leader says it serves generation 1 from its memory while
// generation 2 is invalid, the second, a new leader or another broker that reports the Binding as nobody's, says it serves none.
// A new writer sets right what it finds, which is what a failover should do. Two writers that live at the same time do not
// fight: each writes once, whatever number of lists and passes follow, and the last writer's words stay until one of them changes.
func TestTwoWritersThatDisagreeWriteOnceEachAndDoNotFight(t *testing.T) {
	x := config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonInvalidSpec, Generation: 2, Served: 1,
		Message: "spec: Binding sre/alerts generation 2 is invalid, serving generation 1: spec.chat.channel: wrong"}
	y := config.Rejection{Namespace: "sre", Name: "alerts", Reason: config.ReasonInvalidSpec, Generation: 2, Served: 0,
		Message: "spec: Binding sre/alerts generation 2 is invalid, with no earlier generation to serve: spec.chat.channel: wrong"}
	c := newStatusCluster(t, storedBinding(sreAlerts, 2))

	// X serves generation 1; Y serves nothing. Each has its own report.
	rx := newStatusRig(t, c, map[string]int64{sreAlerts: 1})
	rx.reject(x)
	rx.set(sreAlerts, probe(1)...)
	ry := newStatusRig(t, c, nil)
	ry.reject(y)

	rx.reconcile()
	if got := c.stored(t, sreAlerts)["Accepted"]; got.Status != metav1.ConditionFalse || !strings.Contains(got.Message, "serving generation 1") {
		t.Fatalf("after X: Accepted = %+v, want False, serving generation 1", got)
	}
	ry.reconcile() // Y takes over, or reports a Binding that no broker owns
	if got := c.stored(t, sreAlerts)["Accepted"]; !strings.Contains(got.Message, "no earlier generation") {
		t.Fatalf("after Y: Accepted = %+v, want Y's words: a new writer sets right what it finds", got)
	}
	if rdy := c.stored(t, sreAlerts)["Ready"]; rdy.Status != metav1.ConditionFalse || rdy.Reason != "Accepted:InvalidSpec" {
		t.Errorf("Ready = %s/%s, want False/Accepted:InvalidSpec: a Binding that is not accepted is not ready", rdy.Status, rdy.Reason)
	}
	if c.stored(t, sreAlerts)["BackendReachable"].Type != "" {
		t.Error("Y's status has BackendReachable, though Y serves no Binding: a Binding that is not served has no probes, and X's are gone")
	}
	for range 5 { // lists and passes of both, long after
		for _, r := range []*statusRig{rx, ry} {
			r.advance(11 * time.Minute)
			r.reconcile()
		}
	}
	if c.count() != 2 {
		t.Errorf("applies = %d after both writers listed and passed five times, want 2: one each, no fight", c.count())
	}
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
		return config.Rejection{Namespace: "sre", Name: "alerts", Reason: reason, Generation: generation, Message: "spec." + strings.ToLower(string(reason))}
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
		"one":  {[]config.Rejection{rej(config.ReasonChannelConflict, 2)}, 0, metav1.ConditionFalse, "ChannelConflict", "spec.channelconflict", 2},
		"the object itself first": {
			[]config.Rejection{rej(config.ReasonChannelConflict, 2), rej(config.ReasonBackendNotFound, 2), rej(config.ReasonInvalidSpec, 2)}, 0,
			metav1.ConditionFalse, "InvalidSpec", "spec.invalidspec; spec.backendnotfound; spec.channelconflict", 2,
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
			[]config.Rejection{rej("Weird", 2), rej(config.ReasonChannelConflict, 2)}, 0, metav1.ConditionFalse, "ChannelConflict", "spec.channelconflict; spec.weird", 2,
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
	if len(msg) > maxStatusMessage+len(redact.Truncated) {
		t.Errorf("the message is %d bytes, want at most %d", len(msg), maxStatusMessage+len(redact.Truncated))
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
		conds := probe(1)
		conds[0].Status, conds[0].Reason = bindingcheck.False, bindingcheck.ReasonAgentNotFound
		conds[2].Status, conds[2].Reason = bindingcheck.False, "BackendReachable:AgentNotFound"
		r.mu.Lock()
		r.probed[sreAlerts] = conds
		r.mu.Unlock()
		time.Sleep(statusGap + time.Second) // the gap is over, and a tick has passed without a word from the Prober
		synctest.Wait()
		if c.count() != 1 {
			t.Fatalf("applies = %d, want 1 until the writer is told", c.count())
		}
		r.w.notify()
		synctest.Wait()
		if c.count() != 2 {
			t.Errorf("applies after notify = %d, want 2", c.count())
		}
		cancel()
		<-done
	})
}

// The hooks of the Prober tell the writer, and a verification without one tells nothing and does not fail: that is the files route.
func TestVerificationTellsTheStatusWriterOfTransitionsAndPasses(t *testing.T) {
	r := newStatusRig(t, newStatusCluster(t), nil)
	v := verification{Status: r.w}
	if r.w.dirty.Load() {
		t.Fatal("a new writer is dirty")
	}
	v.notifyStatus()
	if !r.w.dirty.Load() {
		t.Error("notifyStatus did not tell the writer")
	}
	verification{}.notifyStatus() // no writer: nothing to tell, and no panic
}

// A writer is not built without what it needs, and the error names what is missing.
func TestNewStatusWriterNamesWhatIsMissing(t *testing.T) {
	ok := statusConfig{Client: crClusterOf(), Report: func() config.Report { return config.Report{} },
		Conditions: func() map[string][]bindingcheck.Condition { return nil }, Log: slog.New(slog.DiscardHandler)}
	for name, tc := range map[string]struct {
		edit func(*statusConfig)
		want string
	}{
		"client":     {func(c *statusConfig) { c.Client = nil }, "Client"},
		"report":     {func(c *statusConfig) { c.Report = nil }, "Report"},
		"conditions": {func(c *statusConfig) { c.Conditions = nil }, "Conditions"},
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
	r.w.dirty.Store(false)
	eventually(t, 10*time.Second, "the end of a pass without a transition did not tell the writer", func() bool { return r.w.dirty.Load() })
}

// The Prober is given each Binding's generation, so that what it stamps is the generation the Binding had when the pass began.
func TestBindingTargetsCarryTheGeneration(t *testing.T) {
	got := bindingTargets(snapshotOf(t, map[string]int64{sreAlerts: 7}), v1alpha1.ObjectRef{Namespace: "sre", Name: "slack"})
	if want := []bindingcheck.Target{{Binding: sreAlerts, Channel: "C-alerts", Generation: 7}}; !slices.Equal(got, want) {
		t.Errorf("targets = %v, want %v", got, want)
	}
}
