package crsource_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// firstTrigger is the address of the first trigger of the Binding, which two Bindings that share their triggers share.
func firstTrigger(t *testing.T, s *config.Set, name string) *v1alpha1.Trigger {
	t.Helper()
	i := slices.IndexFunc(s.Bindings, func(b v1alpha1.Binding) bool { return b.Name == name })
	if i < 0 || len(s.Bindings[i].Spec.Chat.Triggers) == 0 {
		t.Fatalf("the Set has no Binding %s with a trigger: %v", name, bindingNames(s))
	}
	return &s.Bindings[i].Spec.Chat.Triggers[0]
}

// A read decodes and validates only the objects whose uid or generation changed since the last read, and its Set shares what the
// decoded object holds with the one before, so that the cache that makes this possible is the object that is served and not a
// second copy of it.
func TestAReadDecodesOnlyWhatChanged(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)

	first, fp := set(t, src)
	decoded := src.DecodesForTest()
	if decoded != len(objs) {
		t.Fatalf("the first read decoded %d objects; want the %d of the cluster", decoded, len(objs))
	}
	for range 3 {
		if _, again := set(t, src); again != fp {
			t.Fatalf("a read of the same state returned fingerprint %q, want %q", again, fp)
		}
	}
	if n := src.DecodesForTest(); n != decoded {
		t.Errorf("three more reads of an unchanged cluster decoded %d more objects; want none", n-decoded)
	}
	second, _ := set(t, src)
	if firstTrigger(t, first, "sre-alerts") != firstTrigger(t, second, "sre-alerts") {
		t.Error("two reads of an unchanged Binding hold separate copies of its triggers; want the one the cache holds")
	}
	if cached, ok := src.DecodedForTest(config.KindBinding, "sre", "sre-alerts").(*v1alpha1.Binding); !ok || &cached.Spec.Chat.Triggers[0] != firstTrigger(t, second, "sre-alerts") {
		t.Error("the Set serves a copy of the decoded Binding that the cache holds; want the cache's own triggers")
	}

	edited := find(objs, "sre-alerts")
	edited.SetGeneration(2)
	_ = unstructured.SetNestedField(edited.Object, "C0123456780", "spec", "chat", "channel")
	until(t, func() { c.apply(edited) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	third, _ := set(t, src)
	if n := src.DecodesForTest(); n != decoded+1 {
		t.Errorf("a read after one Binding was edited decoded %d objects; want 1", n-decoded)
	}
	if i := slices.IndexFunc(third.Bindings, func(b v1alpha1.Binding) bool { return b.Name == "sre-alerts" }); i < 0 || third.Bindings[i].Spec.Chat.Channel != "C0123456780" {
		t.Errorf("the edit is not in the Set: %v", third.Bindings)
	}
	if firstTrigger(t, third, "chat-only") != firstTrigger(t, second, "chat-only") {
		t.Error("the Binding that was not edited was decoded again")
	}
}

// An object that was refused is not decoded at every read, and an invalid object that is edited valid is.
func TestARefusedObjectIsNotDecodedAgainAtEveryRead(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	bad := find(objs, "sre-alerts")
	_ = unstructured.SetNestedField(bad.Object, "#general", "spec", "chat", "channel")
	objs[slices.IndexFunc(objs, func(u *unstructured.Unstructured) bool { return u.GetName() == "sre-alerts" })] = bad
	c := newCluster(t, objs...)
	src := c.start(t)

	first, _ := set(t, src)
	decoded := src.DecodesForTest()
	second, _ := set(t, src)
	if n := src.DecodesForTest(); n != decoded {
		t.Errorf("a second read decoded %d more objects; want none, the refusal is remembered", n-decoded)
	}
	if len(first.Invalid) != 1 || len(second.Invalid) != 1 || !reflect.DeepEqual(first.Invalid, second.Invalid) {
		t.Errorf("the refusal reads differently the second time: %+v then %+v", first.Invalid, second.Invalid)
	}

	fixed := find(objs, "sre-alerts")
	fixed.SetGeneration(2)
	_ = unstructured.SetNestedField(fixed.Object, "C0123456789", "spec", "chat", "channel")
	until(t, func() { c.apply(fixed) }, func() bool { s, _ := set(t, src); return len(s.Invalid) == 0 })
}

// What a read remembered of an object is dropped with the object, so that a cluster that creates and deletes objects does not
// grow the cache, and an object made again under the name is decoded as the new object it is.
func TestTheDecodedObjectsOfDeletedObjectsAreDropped(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	set(t, src)
	if n := src.DecodedEntriesForTest(); n != len(objs) {
		t.Fatalf("after a read the cache holds %d decoded objects; want the %d of the cluster", n, len(objs))
	}

	gone := find(objs, "chat-only")
	until(t, func() { c.remove(gone) }, func() bool { return src.CachedGenerationForTest(config.KindBinding, "sre", "chat-only") == -1 })
	// A deletion takes effect at a read that finds nothing newly gone, no sooner than settleFloor, and the object is kept
	// goneGrace more: drive the clock past both.
	now := time.Now()
	src.SetClockForTest(func() time.Time { return now })
	set(t, src)
	now = now.Add(time.Minute)
	set(t, src)
	now = now.Add(2 * time.Minute)
	set(t, src)
	if n := src.DecodedEntriesForTest(); n != len(objs)-1 {
		t.Errorf("after an object was deleted and forgotten the cache holds %d decoded objects; want %d", n, len(objs)-1)
	}

	again := find(objs, "chat-only")
	again.SetUID("uid-again")
	before := src.DecodesForTest()
	until(t, func() { c.apply(again) }, func() bool { s, _ := set(t, src); return slices.Contains(bindingNames(s), "sre/chat-only") })
	if n := src.DecodesForTest(); n <= before {
		t.Error("an object made again under its name, with a new uid, was served from what its predecessor left")
	}
}
