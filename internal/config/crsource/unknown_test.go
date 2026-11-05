package crsource_test

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// A secret that a test puts in the value of a field the broker does not know: no log line may hold it.
const unknownValue = "value-that-must-not-be-logged-7d1e"

// withField returns u with the field at path set to value.
func withField(t *testing.T, u *unstructured.Unstructured, value any, path ...string) *unstructured.Unstructured {
	t.Helper()
	u = u.DeepCopy()
	if err := unstructured.SetNestedField(u.Object, value, path...); err != nil {
		t.Fatal(err)
	}
	return u
}

// cachedGeneration is the generation of u that the source's informer holds now.
func cachedGeneration(src *crsource.Source, u *unstructured.Unstructured) int64 {
	return src.CachedGenerationForTest(u.GetKind(), u.GetNamespace(), u.GetName())
}

// servesAll reads the Set and fails unless the four objects of the corpus are in it and none is refused, which is what a broker needs of
// the objects that a newer API server has changed under it.
func servesAll(t *testing.T, s *config.Set) {
	t.Helper()
	if len(s.Invalid) != 0 {
		t.Fatalf("Invalid = %+v; want nothing refused", s.Invalid)
	}
	if got := bindingNames(s); len(got) != 2 || len(s.AgentBackends) != 1 || len(s.ChatConnections) != 1 {
		t.Fatalf("Bindings %v, backends %d, connections %d; want both Bindings, the backend and the connection", got, len(s.AgentBackends), len(s.ChatConnections))
	}
}

// A later release adds a field with a default, and the API server adds it to every stored object it serves, with no change of generation. A
// broker that starts in that window, with CRDs newer than itself, must serve what it served before: on a Binding, on the backend, and on the
// ChatConnection it serves, which it cannot start without.
func TestAFieldANewerCRDDefaultsLeavesEveryObjectServed(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	for i, u := range objs {
		switch u.GetKind() {
		case "Binding":
			objs[i] = withField(t, u, "defaulted-by-the-api-server", "spec", "chat", "futureField")
		default:
			objs[i] = withField(t, u, "defaulted-by-the-api-server", "spec", "futureField")
		}
	}
	src, logs := newCluster(t, objs...).startLogged(t)
	s, _ := set(t, src)
	servesAll(t, s)
	if n := strings.Count(logs.String(), "fields this broker does not know"); n != len(objs) {
		t.Errorf("the log names the unknown fields %d times; want once for each of the %d objects\n%s", n, len(objs), logs.String())
	}
}

// A field of status that this broker does not have, or one of metadata that a newer API server adds, refused the whole object: it was not
// served, and a session of a Binding that was not served was not recovered and its queued replies were dropped.
func TestAFutureStatusOrMetadataFieldDoesNotUnserveAnObject(t *testing.T) {
	tests := []struct {
		name string
		edit func(*testing.T, *unstructured.Unstructured) *unstructured.Unstructured
	}{
		{"status", func(t *testing.T, u *unstructured.Unstructured) *unstructured.Unstructured {
			return withField(t, u, "bar", "status", "foo")
		}},
		{"metadata", func(t *testing.T, u *unstructured.Unstructured) *unstructured.Unstructured {
			return withField(t, u, int64(1), "metadata", "fieldOfANewerAPIServer")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			leakCheck(t)
			objs := corpus(t)
			for i, u := range objs {
				objs[i] = tc.edit(t, u)
			}
			s, _ := set(t, newCluster(t, objs...).start(t))
			servesAll(t, s)
			for _, b := range s.Bindings {
				if b.Generation != 1 {
					t.Errorf("Binding %s generation = %d; want 1", b.Name, b.Generation)
				}
			}
		})
	}
}

// The last good generation is held as it was read, with the field the broker does not know, and is served again when the next generation is
// refused: tolerance reaches the held copy too.
func TestTheLastGoodGenerationKeepsItsUnknownField(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	good := withField(t, find(objs, "sre-alerts"), "x", "spec", "chat", "futureField")
	good.SetGeneration(2)
	until(t, func() { c.apply(good) }, func() bool { return cachedGeneration(src, good) == 2 })
	s, _ := set(t, src)
	servesAll(t, s)
	bad := withField(t, good, "not-a-channel", "spec", "chat", "channel")
	bad.SetGeneration(3)
	until(t, func() { c.apply(bad) }, func() bool { return cachedGeneration(src, bad) == 3 })
	s, _ = set(t, src)
	if len(s.Invalid) != 1 || s.Invalid[0].Generation != 3 || s.Invalid[0].Served != 2 {
		t.Fatalf("Invalid = %+v; want generation 3 refused and 2 served", s.Invalid)
	}
	if got := bindingNames(s); len(got) != 2 {
		t.Errorf("Bindings = %v; want both, the refused one at its last good generation", got)
	}
}

// What is wrong with a field the broker does know is still wrong: tolerance is for fields that are not in its types.
func TestAKnownFieldWithAValueTheBrokerCannotReadIsStillRefused(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	for i, u := range objs {
		if u.GetName() == "sre-alerts" {
			objs[i] = withField(t, u, []any{"C0123456789"}, "spec", "chat", "channel")
		}
	}
	s, _ := set(t, newCluster(t, objs...).start(t))
	if len(s.Invalid) != 1 || s.Invalid[0].Name != "sre-alerts" || s.Invalid[0].Served != 0 {
		t.Fatalf("Invalid = %+v; want sre-alerts refused", s.Invalid)
	}
}

// Each field is said once for each generation of the object, with the object and the path of the field, and no value. A new generation is
// said again, and an object that is made again.
func TestUnknownFieldsAreNamedOncePerGenerationAndNeverByValue(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	b := withField(t, find(objs, "sre-alerts"), unknownValue, "spec", "chat", "futureField")
	b = withField(t, b, unknownValue, "status", "foo")
	c.apply(b)
	src, logs := c.startLogged(t)
	for range 5 {
		set(t, src)
	}
	const said = "fields this broker does not know"
	if n := strings.Count(logs.String(), said); n != 1 {
		t.Fatalf("said %d times in five reads; want once\n%s", n, logs.String())
	}
	line := logs.String()
	for _, want := range []string{`kind=Binding`, `namespace=sre`, `name=sre-alerts`, `generation=1`, `spec.chat.futureField`, `status.foo`, `unknown_fields=2`} {
		if !strings.Contains(line, want) {
			t.Errorf("the line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, unknownValue) {
		t.Errorf("the log holds the value of an unknown field:\n%s", line)
	}

	next := b.DeepCopy()
	next.SetGeneration(2)
	until(t, func() { c.apply(next) }, func() bool { return cachedGeneration(src, next) == 2 })
	for range 3 {
		set(t, src)
	}
	if n := strings.Count(logs.String(), said); n != 2 || !strings.Contains(logs.String(), "generation=2") {
		t.Errorf("said %d times after a second generation; want twice, the second naming generation 2\n%s", n, logs.String())
	}

	// A generation without the field is not said, and when it returns the next generation is said again.
	clean := withField(t, find(objs, "sre-alerts"), "C0123456789", "spec", "chat", "channel")
	clean.SetGeneration(3)
	until(t, func() { c.apply(clean) }, func() bool { return cachedGeneration(src, clean) == 3 })
	set(t, src)
	again := withField(t, clean, "x", "spec", "chat", "futureField")
	again.SetGeneration(4)
	until(t, func() { c.apply(again) }, func() bool { return cachedGeneration(src, again) == 4 })
	set(t, src)
	if n := strings.Count(logs.String(), said); n != 3 {
		t.Errorf("said %d times in all; want 3 (generations 1, 2 and 4)\n%s", n, logs.String())
	}
}

// A generation that has the field again after one that had none is said again, though it has the generation it had: the report is of
// what the object holds now, and an object that once held it and was cleaned of it is not one that was said already. A defaulted
// field comes and goes with the CRDs, not with the generation.
func TestAnUnknownFieldThatReturnsAtTheSameGenerationIsSaidAgain(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	with := withField(t, find(objs, "sre-alerts"), "x", "spec", "chat", "futureField")
	c.apply(with)
	src, logs := c.startLogged(t)
	const said = "fields this broker does not know"
	set(t, src)
	cached := func() bool {
		u := src.CachedObjectForTest("Binding", "sre", "sre-alerts")
		_, has, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "chat", "futureField")
		return has
	}
	if n := strings.Count(logs.String(), said); n != 1 {
		t.Fatalf("said %d times at the start; want once\n%s", n, logs.String())
	}
	until(t, func() { c.apply(find(objs, "sre-alerts")) }, func() bool { return !cached() })
	set(t, src)
	until(t, func() { c.apply(with) }, cached)
	set(t, src)
	if n := strings.Count(logs.String(), said); n != 2 {
		t.Errorf("said %d times; want twice, as the field came back at generation 1 after a read without it\n%s", n, logs.String())
	}
}

// A Binding that names a connection that does not exist is the broker's, and is another broker's once that connection exists, with
// no change to the Binding. What is not read says nothing and is forgotten, so when the connection goes and the Binding is the
// broker's again, its unknown fields are said again, as for an object that is new to it.
func TestAnUnknownFieldIsSaidAgainWhenTheBindingComesBackToTheBroker(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	ghost := withField(t, find(objs, "chat-only"), "x", "spec", "chat", "futureField")
	ghost.SetName("ghost")
	ghost.SetUID("uid-ghost")
	_ = unstructured.SetNestedField(ghost.Object, "C0999999990", "spec", "chat", "channel")
	_ = unstructured.SetNestedField(ghost.Object, "slack-ghost", "spec", "chat", "connectionRef", "name")
	other := find(objs, "slack-main")
	other.SetName("slack-ghost")
	other.SetUID("uid-slack-ghost")
	c := newCluster(t, append(objs, ghost)...)
	src, logs := c.startLogged(t)
	const said = "fields this broker does not know"
	set(t, src)
	if n := strings.Count(logs.String(), said); n != 1 || !strings.Contains(logs.String(), "name=ghost") {
		t.Fatalf("said %d times at the start; want once, for the ghost Binding\n%s", n, logs.String())
	}
	until(t, func() { c.apply(other) }, func() bool {
		return src.CachedGenerationForTest("ChatConnection", other.GetNamespace(), other.GetName()) >= 0
	})
	if s, _ := set(t, src); slices.Contains(bindingNames(s), "sre/ghost") {
		t.Fatalf("Bindings = %v; want the Binding that names an existing connection to be that connection's broker's", bindingNames(s))
	}
	until(t, func() { c.remove(other) }, func() bool {
		return src.CachedGenerationForTest("ChatConnection", other.GetNamespace(), other.GetName()) < 0
	})
	if s, _ := set(t, src); !slices.Contains(bindingNames(s), "sre/ghost") {
		t.Fatalf("Bindings = %v; want the Binding back, its connection gone", bindingNames(s))
	}
	if n := strings.Count(logs.String(), said); n != 2 {
		t.Errorf("said %d times; want twice, once when the Binding came back to the broker\n%s", n, logs.String())
	}
}

// An object that belongs to another broker, and so is never decoded here, is not said either.
func TestUnknownFieldsOfAnotherBrokersObjectAreNotNamed(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	other := withField(t, find(objs, "slack-main"), "x", "spec", "futureField")
	other.SetName("slack-other")
	other.SetUID("uid-other")
	elsewhere := withField(t, find(objs, "sre-alerts"), "x", "spec", "chat", "futureField")
	elsewhere.SetName("elsewhere")
	elsewhere.SetUID("uid-elsewhere")
	_ = unstructured.SetNestedField(elsewhere.Object, "slack-other", "spec", "chat", "connectionRef", "name")
	_ = unstructured.SetNestedField(elsewhere.Object, "C0123456780", "spec", "chat", "channel")
	src, logs := newCluster(t, append(objs, other, elsewhere)...).startLogged(t)
	set(t, src)
	if strings.Contains(logs.String(), "fields this broker does not know") {
		t.Errorf("the log names the unknown fields of objects of another broker:\n%s", logs.String())
	}
}

// The last good generation of an object that the cache still holds is that very object, not a copy of it: 150 large Bindings cost the
// broker one copy each, where they once cost two. A refused generation is the case that needs a copy, and it keeps the one before it.
func TestTheLastGoodGenerationIsTheCachedObjectNotACopy(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	s, _ := set(t, src)
	servesAll(t, s)
	for _, o := range objs {
		if !src.LastGoodIsCachedForTest(o.GetKind(), o.GetNamespace(), o.GetName()) {
			t.Errorf("%s %s/%s: the last good generation is not the cached object", o.GetKind(), o.GetNamespace(), o.GetName())
		}
	}
	bad := withField(t, find(objs, "sre-alerts"), "not-a-channel", "spec", "chat", "channel")
	bad.SetGeneration(2)
	until(t, func() { c.apply(bad) }, func() bool { return cachedGeneration(src, bad) == 2 })
	s, _ = set(t, src)
	if len(s.Invalid) != 1 || s.Invalid[0].Served != 1 || src.LastGoodIsCachedForTest("Binding", "sre", "sre-alerts") {
		t.Errorf("Invalid = %+v; want generation 1 served from the object the cache no longer holds", s.Invalid)
	}
}

// What the informers keep is what the broker reads: not metadata.managedFields, and not the copy of the spec that kubectl apply puts in an
// annotation, which is as large as the spec. Every other annotation stays, and the object the Set is made of is whole.
func TestTheCacheDropsWhatTheBrokerNeverReads(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	for i, u := range objs {
		u = u.DeepCopy()
		u.SetManagedFields(nil)
		_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"manager": "kubectl", "operation": "Apply", "apiVersion": "agentkourier.dev/v1alpha1"}}, "metadata", "managedFields")
		a := map[string]string{"kubectl.kubernetes.io/last-applied-configuration": strings.Repeat("{}", 100)}
		if u.GetName() == "sre-alerts" {
			a["team.example/owner"] = "sre"
		}
		u.SetAnnotations(a)
		objs[i] = u
	}
	src := newCluster(t, objs...).start(t)
	for _, name := range []string{"sre-alerts", "chat-only"} {
		u := src.CachedObjectForTest("Binding", "sre", name)
		if u == nil {
			t.Fatalf("the cache holds no Binding %s", name)
		}
		if _, has, _ := unstructured.NestedFieldNoCopy(u.Object, "metadata", "managedFields"); has {
			t.Errorf("%s: the cache holds managedFields", name)
		}
		a := u.GetAnnotations()
		if _, has := a["kubectl.kubernetes.io/last-applied-configuration"]; has {
			t.Errorf("%s: the cache holds the last-applied-configuration annotation", name)
		}
		if name == "sre-alerts" && a["team.example/owner"] != "sre" {
			t.Errorf("%s: annotations = %v; want the others kept", name, a)
		}
		if _, has, _ := unstructured.NestedFieldNoCopy(u.Object, "metadata", "annotations"); has && name == "chat-only" {
			t.Errorf("%s: an annotations field with nothing in it is left", name)
		}
	}
	s, _ := set(t, src)
	servesAll(t, s)
}
