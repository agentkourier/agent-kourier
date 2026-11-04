package crsource_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
	"github.com/agentkourier/agent-kourier/internal/config/sourcetest"
)

var (
	bindings    = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "bindings"}
	backends    = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "agentbackends"}
	connections = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "chatconnections"}
)

var own = v1alpha1.ObjectRef{Namespace: sourcetest.ConnectionNamespace, Name: sourcetest.ConnectionName}

// leakCheck fails the test if a goroutine that it started outlives it. The baseline is taken here, before any informer
// exists: a baseline taken after a sync would miss a reflector that has listed and not yet opened its watch. It registers
// first so that it runs after every other cleanup, Stop among them.
func leakCheck(t *testing.T) {
	t.Helper()
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
}

// object makes the object a cluster would hold for a document: its uid and generation are given, as an API server gives
// them.
func object(t *testing.T, doc string, uid string, generation int64) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
		t.Fatal(err)
	}
	u.SetUID(types.UID(uid))
	u.SetGeneration(generation)
	u.SetResourceVersion("1")
	return u
}

func gvrOf(t *testing.T, u *unstructured.Unstructured) schema.GroupVersionResource {
	t.Helper()
	switch u.GetKind() {
	case "Binding":
		return bindings
	case "AgentBackend":
		return backends
	case "ChatConnection":
		return connections
	}
	t.Fatalf("unknown kind %q", u.GetKind())
	return schema.GroupVersionResource{}
}

// cluster is a fake API server with the three resources.
type cluster struct {
	t      *testing.T
	client *dynamicfake.FakeDynamicClient
}

func newCluster(t *testing.T, objs ...*unstructured.Unstructured) *cluster {
	t.Helper()
	rt := make([]runtime.Object, len(objs))
	for i, o := range objs {
		rt[i] = o
	}
	c := &cluster{t: t}
	c.client = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		bindings: "BindingList", backends: "AgentBackendList", connections: "ChatConnectionList",
	}, rt...)
	return c
}

// start returns a Source over the cluster that is started and is stopped when the test ends, serving own.
func (c *cluster) start(t *testing.T) *crsource.Source {
	t.Helper()
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := src.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return src
}

func (c *cluster) resource(u *unstructured.Unstructured) dynamic.ResourceInterface {
	return c.client.Resource(gvrOf(c.t, u)).Namespace(u.GetNamespace())
}

// apply makes the cluster hold u: it creates it, or updates it when it exists. Callers repeat it until they see the
// effect (see until), so an error that says the work was done is not one.
func (c *cluster) apply(u *unstructured.Unstructured) {
	c.t.Helper()
	if _, err := c.resource(u).Create(c.t.Context(), u.DeepCopy(), metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		_, err = c.resource(u).Update(c.t.Context(), u.DeepCopy(), metav1.UpdateOptions{})
		if err != nil {
			c.t.Fatal(err)
		}
	} else if err != nil {
		c.t.Fatal(err)
	}
}

func (c *cluster) remove(u *unstructured.Unstructured) {
	c.t.Helper()
	if err := c.resource(u).Delete(c.t.Context(), u.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		c.t.Fatal(err)
	}
}

// until repeats act until cond holds, because a fake client delivers only to a watch that is open, and the informer's may
// not be yet; it fails the test after five seconds.
func until(t *testing.T, act func(), cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		act()
		time.Sleep(10 * time.Millisecond)
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the source did not see the change within 5s")
		}
	}
}

// corpus returns the objects of the valid corpus, as a cluster holds them.
func corpus(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	for i, d := range sourcetest.Valid(t) {
		out = append(out, object(t, d, fmt.Sprintf("uid-%d", i), 1))
	}
	return out
}

// binding returns the corpus object named name, edited by edit.
func find(objs []*unstructured.Unstructured, name string) *unstructured.Unstructured {
	i := slices.IndexFunc(objs, func(u *unstructured.Unstructured) bool { return u.GetName() == name })
	return objs[i].DeepCopy()
}

func set(t *testing.T, src *crsource.Source) (*config.Set, string) {
	t.Helper()
	s, fp, err := src.Set(t.Context())
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	return s, fp
}

func names[T any](items []T, name func(T) string) []string {
	var out []string
	for _, i := range items {
		out = append(out, name(i))
	}
	slices.Sort(out)
	return out
}

func bindingNames(s *config.Set) []string {
	return names(s.Bindings, func(b v1alpha1.Binding) string { return b.Namespace + "/" + b.Name })
}

// The custom resource source runs the same conformance suite as the files.
func TestConformance(t *testing.T) {
	sourcetest.Run(t, func(t *testing.T, docs []string) config.Source {
		leakCheck(t)
		var objs []*unstructured.Unstructured
		for i, d := range docs {
			objs = append(objs, object(t, d, fmt.Sprintf("uid-%d", i), 1))
		}
		return newCluster(t, objs...).start(t)
	}, sourcetest.Options{PerObject: true})
}

// The objects of a cluster carry what an API server adds: a uid, a generation, a creation time. The Set keeps them, since
// the age of a Binding decides a channel and its generation is what the status says was served.
func TestSetKeepsMetadata(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	b := find(objs, "sre-alerts")
	b.SetGeneration(7)
	b.SetCreationTimestamp(metav1.NewTime(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)))
	objs[slices.IndexFunc(objs, func(u *unstructured.Unstructured) bool { return u.GetName() == "sre-alerts" })] = b
	s, _ := set(t, newCluster(t, objs...).start(t))
	i := slices.IndexFunc(s.Bindings, func(x v1alpha1.Binding) bool { return x.Name == "sre-alerts" })
	got := s.Bindings[i]
	if got.Generation != 7 || got.CreationTimestamp.IsZero() || got.UID == "" {
		t.Errorf("Binding metadata = generation %d, created %v, uid %q; want 7, a time and a uid", got.Generation, got.CreationTimestamp, got.UID)
	}
}

// A Binding may carry a status, as an API server's object does, and is not refused for it, which a file is.
func TestABindingMayCarryAStatus(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	b := find(objs, "sre-alerts")
	if err := unstructured.SetNestedSlice(b.Object, []any{map[string]any{
		"type": "Ready", "status": "True", "reason": "Ready", "message": "", "lastTransitionTime": "2026-11-04T12:00:00Z", "observedGeneration": int64(1),
	}}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	objs[slices.IndexFunc(objs, func(u *unstructured.Unstructured) bool { return u.GetName() == "sre-alerts" })] = b
	s, _ := set(t, newCluster(t, objs...).start(t))
	if len(s.Invalid) != 0 || len(s.Bindings) != 2 {
		t.Errorf("Set has %d Bindings and Invalid %+v; want both Bindings and none refused", len(s.Bindings), s.Invalid)
	}
}

// The fingerprint moves with an object's generation and uid and with nothing else. A status write changes resourceVersion
// and not generation, and must not make a reload, a probe pass and another status write.
func TestFingerprintFollowsGenerationAndUIDNotResourceVersion(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	_, fp := set(t, src)

	status := find(objs, "sre-alerts")
	status.SetResourceVersion("2")
	_ = unstructured.SetNestedField(status.Object, "True", "status", "ready")
	until(t, func() { c.apply(status) }, func() bool {
		u, err := c.resource(status).Get(t.Context(), "sre-alerts", metav1.GetOptions{})
		return err == nil && u.GetResourceVersion() == "2"
	})
	time.Sleep(200 * time.Millisecond) // the informer has had time to see it
	if now := src.Fingerprint(t.Context()); now != fp {
		t.Error("a change of resourceVersion alone moved the fingerprint")
	}

	spec := find(objs, "sre-alerts")
	spec.SetGeneration(2)
	until(t, func() { c.apply(spec) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	_, afterSpec := set(t, src)

	again := find(objs, "sre-alerts")
	again.SetGeneration(2)
	again.SetUID("uid-recreated")
	until(t, func() { c.remove(again); c.apply(again) }, func() bool { return src.Fingerprint(t.Context()) != afterSpec })
}

// The Set read and the fingerprint it returns describe the same state, and a Fingerprint taken then agrees.
func TestSetReturnsTheFingerprintOfWhatItRead(t *testing.T) {
	leakCheck(t)
	src := newCluster(t, corpus(t)...).start(t)
	_, fp := set(t, src)
	if got := src.Fingerprint(t.Context()); got != fp {
		t.Errorf("Fingerprint = %q; Set returned %q", got, fp)
	}
}

// An invalid update leaves the last good generation serving, says which it is, and a fix ends it.
func TestAnInvalidUpdateKeepsTheLastGoodGeneration(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	_, fp := set(t, src)

	bad := find(objs, "sre-alerts")
	bad.SetGeneration(2)
	_ = unstructured.SetNestedField(bad.Object, "#general", "spec", "chat", "channel")
	until(t, func() { c.apply(bad) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	s, _ := set(t, src)
	if len(s.Invalid) != 1 {
		t.Fatalf("Invalid = %+v; want one", s.Invalid)
	}
	inv := s.Invalid[0]
	if inv.Kind != config.KindBinding || inv.Name != "sre-alerts" || inv.Generation != 2 || inv.Served != 1 || !strings.Contains(inv.Message, "invalid channel") {
		t.Errorf("Invalid = %+v; want sre-alerts generation 2 refused, 1 served, for the channel", inv)
	}
	if got := bindingNames(s); !slices.Equal(got, []string{"sre/chat-only", "sre/sre-alerts"}) {
		t.Errorf("Bindings = %v; want both, the invalid one as its last good generation", got)
	}
	i := slices.IndexFunc(s.Bindings, func(b v1alpha1.Binding) bool { return b.Name == "sre-alerts" })
	if s.Bindings[i].Generation != 1 || s.Bindings[i].Spec.Chat.Channel != "C0123456789" {
		t.Errorf("the Binding served is generation %d on channel %q; want 1 on C0123456789", s.Bindings[i].Generation, s.Bindings[i].Spec.Chat.Channel)
	}

	fixed := find(objs, "sre-alerts")
	fixed.SetGeneration(3)
	_ = unstructured.SetNestedField(fixed.Object, "C0123456789", "spec", "chat", "channel")
	until(t, func() { c.apply(fixed) }, func() bool { s, _ := set(t, src); return len(s.Invalid) == 0 })
	s, _ = set(t, src)
	i = slices.IndexFunc(s.Bindings, func(b v1alpha1.Binding) bool { return b.Name == "sre-alerts" })
	if s.Bindings[i].Generation != 3 {
		t.Errorf("after the fix the Binding served is generation %d; want 3", s.Bindings[i].Generation)
	}
}

// An object that is invalid when it first appears has no earlier generation: it is not served. One that was deleted and
// made again under the same name, which is another object, does not borrow the generation of the first.
func TestAnInvalidNewObjectIsNotServedAndARecreatedOneDoesNotBorrow(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	_, fp := set(t, src)

	fresh := find(objs, "chat-only")
	fresh.SetName("brand-new")
	fresh.SetUID("uid-new")
	_ = unstructured.SetNestedField(fresh.Object, "#general", "spec", "chat", "channel")
	until(t, func() { c.apply(fresh) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	s, _ := set(t, src)
	if len(s.Invalid) != 1 || s.Invalid[0].Served != 0 || s.Invalid[0].Name != "brand-new" {
		t.Errorf("Invalid = %+v; want brand-new, not served", s.Invalid)
	}
	if slices.Contains(bindingNames(s), "sre/brand-new") {
		t.Error("an invalid new Binding is in the Set")
	}

	// sre-alerts is deleted and made again with the same name, a new uid, and a spec that is invalid.
	_, fp = set(t, src)
	reborn := find(objs, "sre-alerts")
	reborn.SetUID("uid-reborn")
	reborn.SetGeneration(1)
	_ = unstructured.SetNestedField(reborn.Object, "#general", "spec", "chat", "channel")
	until(t, func() { c.remove(reborn); c.apply(reborn) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	s, _ = set(t, src)
	if slices.Contains(bindingNames(s), "sre/sre-alerts") {
		t.Error("a Binding that was deleted and made again, invalid, was served from the first one's generation")
	}
	i := slices.IndexFunc(s.Invalid, func(i config.Invalid) bool { return i.Name == "sre-alerts" })
	if i < 0 || s.Invalid[i].Served != 0 {
		t.Errorf("Invalid = %+v; want sre-alerts not served", s.Invalid)
	}
}

// A deleted object is removed, and a Binding that is created in a namespace nobody had before is seen with nothing
// restarted.
func TestDeletedObjectsAreRemovedAndNewNamespacesAreSeen(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)

	gone := find(objs, "chat-only")
	until(t, func() { c.remove(gone) }, func() bool { s, _ := set(t, src); return len(s.Bindings) == 1 })

	fresh := find(objs, "sre-alerts")
	fresh.SetNamespace("payments-new")
	fresh.SetName("late")
	fresh.SetUID("uid-late")
	_ = unstructured.SetNestedField(fresh.Object, "C0999999999", "spec", "chat", "channel")
	until(t, func() { c.apply(fresh) }, func() bool { s, _ := set(t, src); return slices.Contains(bindingNames(s), "payments-new/late") })
}

// A Binding names the connection of this broker, or another that exists and is another broker's, or none that exists. The
// first two are the Set's to hold and to leave alone, and the last is held so that someone reports it.
func TestOnlyTheBrokersOwnConnectionAndItsBindingsAreRead(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	other := find(objs, "slack-main")
	other.SetName("slack-other")
	other.SetUID("uid-other")
	foreign := find(objs, "chat-only")
	foreign.SetName("foreign")
	foreign.SetUID("uid-foreign")
	_ = unstructured.SetNestedField(foreign.Object, "slack-other", "spec", "chat", "connectionRef", "name")
	dangling := find(objs, "chat-only")
	dangling.SetName("dangling")
	dangling.SetUID("uid-dangling")
	_ = unstructured.SetNestedField(dangling.Object, "nowhere", "spec", "chat", "connectionRef", "name")
	brokenForeign := find(objs, "chat-only")
	brokenForeign.SetName("broken-foreign")
	brokenForeign.SetUID("uid-broken-foreign")
	_ = unstructured.SetNestedField(brokenForeign.Object, "slack-other", "spec", "chat", "connectionRef", "name")
	_ = unstructured.SetNestedField(brokenForeign.Object, "#general", "spec", "chat", "channel")
	brokenOther := find(objs, "slack-main")
	brokenOther.SetName("slack-broken")
	brokenOther.SetUID("uid-slack-broken")
	_ = unstructured.SetNestedField(brokenOther.Object, "teams", "spec", "platform")

	s, _ := set(t, newCluster(t, append(objs, other, foreign, dangling, brokenForeign, brokenOther)...).start(t))
	if got := names(s.ChatConnections, func(c v1alpha1.ChatConnection) string { return c.Name }); !slices.Equal(got, []string{"slack-main"}) {
		t.Errorf("connections = %v; want only the broker's own", got)
	}
	if got := bindingNames(s); !slices.Equal(got, []string{"sre/chat-only", "sre/dangling", "sre/sre-alerts"}) {
		t.Errorf("Bindings = %v; want those of the own connection and the one that names none", got)
	}
	if len(s.Invalid) != 0 {
		t.Errorf("Invalid = %+v; an invalid object of another broker's is that broker's to report", s.Invalid)
	}
}

// A refused connection that is the broker's own is reported, and the rest of the Set is still read.
func TestTheOwnConnectionThatIsInvalidIsReported(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	bad := find(objs, "slack-main")
	_ = unstructured.SetNestedField(bad.Object, "teams", "spec", "platform")
	objs[slices.IndexFunc(objs, func(u *unstructured.Unstructured) bool { return u.GetName() == "slack-main" })] = bad
	s, _ := set(t, newCluster(t, objs...).start(t))
	if len(s.ChatConnections) != 0 || len(s.Invalid) != 1 || s.Invalid[0].Kind != config.KindChatConnection || s.Invalid[0].Served != 0 {
		t.Errorf("connections %d, Invalid %+v; want none served and the own connection refused", len(s.ChatConnections), s.Invalid)
	}
}

// A running broker keeps the last good generation of its own connection, even when the object is deleted.
func TestTheOwnConnectionIsHeldWhenDeleted(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	conn := find(objs, "slack-main")
	set(t, src) // the broker has read it once, which is what makes a generation the last good one
	until(t, func() { c.remove(conn) }, func() bool { s, _ := set(t, src); return len(s.Invalid) == 1 })
	s, _ := set(t, src)
	inv := s.Invalid[0]
	if len(s.ChatConnections) != 1 || !inv.Deleted || inv.Served != 1 || inv.Name != "slack-main" {
		t.Errorf("connections %d, Invalid %+v; want the connection held, deleted, generation 1", len(s.ChatConnections), inv)
	}
	// A backend that is deleted is not held: only the connection is.
	backend := find(objs, "kagent-pilot")
	until(t, func() { c.remove(backend) }, func() bool { s, _ := set(t, src); return len(s.AgentBackends) == 0 })
}

// With no CRD the API server does not serve the resource, and Start says so, naming it, and the error is the operator's.
func TestStartWithoutCRDs(t *testing.T) {
	leakCheck(t)
	c := newCluster(t)
	c.client.PrependReactor("list", "bindings", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(bindings.GroupResource(), "")
	})
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Stop)
	err = src.Start(t.Context())
	if !errors.Is(err, crsource.ErrCRDsAbsent) || !strings.Contains(err.Error(), "bindings.agentkourier.dev") {
		t.Errorf("Start = %v; want ErrCRDsAbsent naming bindings.agentkourier.dev", err)
	}
}

// A ServiceAccount that may not list a resource is told which right is missing.
func TestStartWithoutRBAC(t *testing.T) {
	leakCheck(t)
	c := newCluster(t)
	c.client.PrependReactor("list", "chatconnections", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(connections.GroupResource(), "", errors.New("no"))
	})
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Stop)
	err = src.Start(t.Context())
	if !errors.Is(err, crsource.ErrAccessDenied) || !strings.Contains(err.Error(), "chatconnections.agentkourier.dev") {
		t.Errorf("Start = %v; want ErrAccessDenied naming chatconnections.agentkourier.dev", err)
	}
}

// Caches that have not synced publish nothing: Set fails and Fingerprint says why, and the wait of Start is bounded by its
// context. A cache that is empty is not one that has synced.
func TestNothingIsReadBeforeTheCachesSync(t *testing.T) {
	leakCheck(t)
	c := newCluster(t, corpus(t)...)
	release := make(chan struct{})
	var once sync.Once
	var lists int
	var mu sync.Mutex
	c.client.PrependReactor("list", "bindings", func(clienttesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		lists++
		first := lists == 1
		mu.Unlock()
		if !first { // the probe passes; the informer's list is held
			<-release
		}
		return false, nil, nil
	})
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { once.Do(func() { close(release) }); src.Stop() })
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := src.Start(ctx); !errors.Is(err, crsource.ErrNotSynced) {
		t.Fatalf("Start = %v; want ErrNotSynced once its context ends", err)
	}
	if _, fp, err := src.Set(t.Context()); !errors.Is(err, crsource.ErrNotSynced) || !strings.HasPrefix(fp, "!") {
		t.Errorf("Set = fingerprint %q, error %v; want ErrNotSynced and a fingerprint that says so", fp, err)
	}
	once.Do(func() { close(release) })
	until(t, func() {}, func() bool { _, _, err := src.Set(t.Context()); return err == nil })
}

// After Stop, Set fails rather than serve a cache that no longer updates.
func TestSetAfterStop(t *testing.T) {
	leakCheck(t)
	src := newCluster(t, corpus(t)...).start(t)
	src.Stop()
	if _, _, err := src.Set(t.Context()); !errors.Is(err, crsource.ErrStopped) {
		t.Errorf("Set after Stop = %v; want ErrStopped", err)
	}
}

func TestNewRefusesWhatItCannotServe(t *testing.T) {
	c := newCluster(t)
	for name, opts := range map[string]crsource.Options{
		"no client":     {Connection: own},
		"no connection": {Client: c.client},
		"no namespace":  {Client: c.client, Connection: v1alpha1.ObjectRef{Name: "x"}},
	} {
		if _, err := crsource.New(opts); err == nil {
			t.Errorf("%s: New accepted the options", name)
		}
	}
}

func TestStartTwice(t *testing.T) {
	leakCheck(t)
	c := newCluster(t, corpus(t)...)
	src := c.start(t)
	if err := src.Start(t.Context()); err == nil {
		t.Error("a second Start was accepted")
	}
}
