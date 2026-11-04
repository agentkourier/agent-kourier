package crsource_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// withStatus gives the object one condition, as the leader's status writer does.
func withStatus(t *testing.T, u *unstructured.Unstructured, reason string) *unstructured.Unstructured {
	t.Helper()
	if err := unstructured.SetNestedSlice(u.Object, []any{map[string]any{
		"type": "Ready", "status": "True", "reason": reason, "message": "", "lastTransitionTime": "2026-11-04T12:00:00Z", "observedGeneration": int64(1),
	}}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	return u
}

// The status of a Binding is read from the cache of the informer, with the object's uid and generation, as a copy, and reading
// it changes nothing the source serves: not the Set, not the fingerprint.
func TestBindingStatusReadsTheCacheAndChangesNothingTheSourceServes(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	alerts := withStatus(t, find(objs, "sre-alerts"), "Ready")
	objs[slicesIndex(objs, "sre-alerts")] = alerts
	src := newCluster(t, objs...).start(t)
	before, fpBefore := set(t, src)

	got, ok, err := src.BindingStatus(alerts.GetNamespace(), "sre-alerts")
	if err != nil || !ok {
		t.Fatalf("BindingStatus = %+v, %v, %v; want the cached status", got, ok, err)
	}
	if got.UID != alerts.GetUID() || got.Generation != 1 || len(got.Conditions) != 1 || got.Conditions[0].Type != "Ready" ||
		got.Conditions[0].Reason != "Ready" || got.Conditions[0].ObservedGeneration != 1 || got.Conditions[0].LastTransitionTime.IsZero() {
		t.Errorf("BindingStatus = %+v, want the object's uid, generation 1 and its condition", got)
	}
	got.Conditions[0].Reason = "Changed by the caller"
	if again, _, _ := src.BindingStatus(alerts.GetNamespace(), "sre-alerts"); again.Conditions[0].Reason != "Ready" {
		t.Errorf("a caller changed the cache through the status it was given: %q", again.Conditions[0].Reason)
	}

	if none, ok, err := src.BindingStatus(alerts.GetNamespace(), "chat-only-with-no-status-does-not-exist"); ok || err != nil || len(none.Conditions) != 0 {
		t.Errorf("BindingStatus of a Binding that is not cached = %+v, %v, %v; want none and no error", none, ok, err)
	}
	if bare, ok, err := src.BindingStatus(alerts.GetNamespace(), "chat-only"); err != nil || !ok || len(bare.Conditions) != 0 || bare.UID == "" {
		t.Errorf("BindingStatus of a Binding with no status = %+v, %v, %v; want it cached, with its uid and no conditions", bare, ok, err)
	}

	after, fpAfter := set(t, src)
	if fpBefore != fpAfter || !reflect.DeepEqual(before, after) {
		t.Error("reading a status changed what the source serves")
	}
}

// A status that changes in the cluster is seen at watch latency, with no resync: the status a re-created Binding has is its
// own (the uid moves), and a status that was edited is the one read.
func TestBindingStatusFollowsTheClusterAndTellsAnObjectMadeAgain(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	alerts := find(objs, "sre-alerts")
	ns := alerts.GetNamespace()

	edited := withStatus(t, alerts.DeepCopy(), "Edited")
	until(t, func() { c.apply(edited) }, func() bool {
		s, ok, err := src.BindingStatus(ns, "sre-alerts")
		return err == nil && ok && len(s.Conditions) == 1 && s.Conditions[0].Reason == "Edited"
	})

	old, _, _ := src.BindingStatus(ns, "sre-alerts")
	again := alerts.DeepCopy() // made again: a new uid, generation 1 again, no status
	again.SetUID("a-new-uid")
	until(t, func() { c.remove(alerts); c.apply(again) }, func() bool {
		s, ok, err := src.BindingStatus(ns, "sre-alerts")
		return err == nil && ok && s.UID == "a-new-uid"
	})
	if s, _, _ := src.BindingStatus(ns, "sre-alerts"); len(s.Conditions) != 0 || s.UID == old.UID || s.Generation != old.Generation {
		t.Errorf("status of the object made again = %+v (was %+v); want no conditions, another uid, the same generation", s, old)
	}
}

// Before the informer has synced the cache says nothing, which is no status, and a stopped source says so.
func TestBindingStatusRefusesBeforeTheCacheHasSyncedAndAfterStop(t *testing.T) {
	leakCheck(t)
	c := newCluster(t, corpus(t)...)
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := src.BindingStatus("sre", "sre-alerts"); ok || !errors.Is(err, crsource.ErrNotSynced) {
		t.Errorf("before Start: found %v, err %v; want ErrNotSynced", ok, err)
	}
	if err := src.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.Stop()
	if _, ok, err := src.BindingStatus("sre", "sre-alerts"); ok || !errors.Is(err, crsource.ErrStopped) {
		t.Errorf("after Stop: found %v, err %v; want ErrStopped", ok, err)
	}
}

func slicesIndex(objs []*unstructured.Unstructured, name string) int {
	for i, u := range objs {
		if u.GetName() == name {
			return i
		}
	}
	return -1
}

// The count of Binding events grows with each Binding added, changed or deleted, and with nothing else: it is what lets a reader of
// the statuses skip a pass when nothing moved. A change of an AgentBackend is no Binding event.
func TestBindingEventsCountTheChangesOfBindingsOnly(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	alerts := find(objs, "sre-alerts")
	grew := func(from uint64) func() bool { return func() bool { return src.BindingEvents() > from } }

	before := src.BindingEvents()
	until(t, func() { c.apply(withStatus(t, alerts.DeepCopy(), "Updated")) }, grew(before))

	before = src.BindingEvents()
	extra := find(objs, "chat-only")
	extra.SetName("added")
	extra.SetUID("uid-added")
	until(t, func() { c.apply(extra) }, grew(before))

	before = src.BindingEvents()
	until(t, func() { c.remove(extra) }, grew(before))

	before = src.BindingEvents()
	backend := find(objs, "kagent-pilot")
	backend.SetGeneration(2)
	until(t, func() { c.apply(backend) }, func() bool {
		return src.CachedGenerationForTest("AgentBackend", backend.GetNamespace(), backend.GetName()) == 2
	})
	if after := src.BindingEvents(); after != before {
		t.Errorf("events went from %d to %d for a change of an AgentBackend", before, after)
	}
}
