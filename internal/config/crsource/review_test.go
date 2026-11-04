package crsource_test

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	clienttesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// A running broker keeps its own connection through anything that is done to the object: deleted and made again, which is
// another uid, and invalid, it still serves the last good generation, and says so. Without that a takeover would find no
// connection and exit.
func TestTheOwnConnectionIsHeldWhenDeletedAndMadeAgainInvalid(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	set(t, src)
	conn := find(objs, "slack-main")
	until(t, func() { c.remove(conn) }, func() bool { s, _ := set(t, src); return len(s.Invalid) == 1 && s.Invalid[0].Deleted })

	bad := conn.DeepCopy()
	bad.SetUID("uid-recreated")
	bad.SetGeneration(1)
	_ = unstructured.SetNestedField(bad.Object, "teams", "spec", "platform")
	until(t, func() { c.apply(bad) }, func() bool { s, _ := set(t, src); return len(s.Invalid) == 1 && !s.Invalid[0].Deleted })
	s, _ := set(t, src)
	inv := s.Invalid[0]
	if len(s.ChatConnections) != 1 || inv.Kind != config.KindChatConnection || inv.Served != 1 || inv.Generation != 1 || !strings.Contains(inv.Message, "platform") {
		t.Errorf("connections %d, Invalid %+v; want the last good connection held and the new one refused for its platform", len(s.ChatConnections), inv)
	}
	if got := bindingNames(s); len(got) != 2 {
		t.Errorf("Bindings = %v; want both, which name the connection", got)
	}
}

// An edit that is invalid cannot say whose Binding it is: the last good generation does. Served by this broker and edited
// into an invalid spec that names another broker's connection, a Binding stays served and is reported; the other direction,
// a Binding that is another broker's and is edited invalid onto this one's, is reported by this broker, which has no last
// good for it, and one that stays on the other's is none of its business.
func TestAnInvalidEditDoesNotMoveABindingBetweenBrokers(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	other := find(objs, "slack-main")
	other.SetName("slack-other")
	other.SetUID("uid-other")
	foreign := find(objs, "chat-only")
	foreign.SetName("foreign")
	foreign.SetUID("uid-foreign")
	_ = unstructured.SetNestedField(foreign.Object, "slack-other", "spec", "chat", "connectionRef", "name")
	_ = unstructured.SetNestedField(foreign.Object, "C0999999990", "spec", "chat", "channel")
	c := newCluster(t, append(objs, other, foreign)...)
	src := c.start(t)
	_, fp := set(t, src)

	away := find(objs, "sre-alerts") // served here, edited invalid and onto the other connection
	away.SetGeneration(2)
	_ = unstructured.SetNestedField(away.Object, "#general", "spec", "chat", "channel")
	_ = unstructured.SetNestedField(away.Object, "slack-other", "spec", "chat", "connectionRef", "name")
	until(t, func() { c.apply(away) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	s, _ := set(t, src)
	if got := bindingNames(s); len(got) != 2 || got[1] != "sre/sre-alerts" {
		t.Fatalf("Bindings = %v; want sre-alerts still served from its last good generation", got)
	}
	if len(s.Invalid) != 1 || s.Invalid[0].Name != "sre-alerts" || s.Invalid[0].Served != 1 {
		t.Errorf("Invalid = %+v; want sre-alerts refused, with generation 1 served", s.Invalid)
	}

	_, fp = set(t, src) // the other broker's Binding, edited invalid and onto this one: this broker has no good for it
	onto := foreign.DeepCopy()
	onto.SetGeneration(2)
	_ = unstructured.SetNestedField(onto.Object, "#general", "spec", "chat", "channel")
	_ = unstructured.SetNestedField(onto.Object, "slack-main", "spec", "chat", "connectionRef", "name")
	_ = unstructured.SetNestedField(onto.Object, "agent-kourier-system", "spec", "chat", "connectionRef", "namespace")
	until(t, func() { c.apply(onto) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	s, _ = set(t, src)
	i := -1
	for n, inv := range s.Invalid {
		if inv.Name == "foreign" {
			i = n
		}
	}
	if i < 0 || s.Invalid[i].Served != 0 || len(bindingNames(s)) != 2 {
		t.Errorf("Invalid = %+v, Bindings %v; want foreign reported, not served, now that it names this broker's connection", s.Invalid, bindingNames(s))
	}

	_, fp = set(t, src) // and one that stays on the other's connection while invalid is none of this broker's
	stays := foreign.DeepCopy()
	stays.SetGeneration(3)
	_ = unstructured.SetNestedField(stays.Object, "#general", "spec", "chat", "channel")
	until(t, func() { c.apply(stays) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	s, _ = set(t, src)
	for _, inv := range s.Invalid {
		if inv.Name == "foreign" {
			t.Errorf("Invalid = %+v; a Binding that names another broker's connection is that broker's to report", s.Invalid)
		}
	}
}

// A Binding that names another broker's connection without a namespace means the connection in its own namespace, and is
// left to that broker.
func TestAConnectionRefWithoutANamespaceMeansTheBindingsOwn(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	other := find(objs, "slack-main")
	other.SetNamespace("sre")
	other.SetName("slack-other")
	other.SetUID("uid-other")
	foreign := find(objs, "chat-only")
	foreign.SetName("foreign")
	foreign.SetUID("uid-foreign")
	_ = unstructured.SetNestedMap(foreign.Object, map[string]any{"name": "slack-other"}, "spec", "chat", "connectionRef")
	s, _ := set(t, newCluster(t, append(objs, other, foreign)...).start(t))
	if got := bindingNames(s); len(got) != 2 {
		t.Errorf("Bindings = %v; want only the two of this broker's connection: foreign names sre/slack-other", got)
	}
}

// Start tries a watch as well as a list, so that RBAC that allows the one and not the other is the operator's to fix at
// start, with the code of RBAC, and not a Warn line that repeats.
func TestStartWithoutTheRightToWatch(t *testing.T) {
	leakCheck(t)
	c := newCluster(t)
	c.client.PrependWatchReactor("agentbackends", func(clienttesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewForbidden(backends.GroupResource(), "", errors.New("no"))
	})
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Stop)
	err = src.Start(t.Context())
	if !errors.Is(err, crsource.ErrAccessDenied) || !strings.Contains(err.Error(), "agentbackends.agentkourier.dev") {
		t.Errorf("Start = %v; want ErrAccessDenied naming agentbackends.agentkourier.dev", err)
	}
}

// While an informer cannot list or watch, the cache is kept, which is right, and the source says it is stale, and stops
// saying so a while after the failures stop.
func TestStaleWhileAnInformerFails(t *testing.T) {
	leakCheck(t)
	c := newCluster(t, corpus(t)...)
	var mu sync.Mutex
	failing, opened := false, 0
	c.client.PrependWatchReactor("bindings", func(clienttesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		defer mu.Unlock()
		opened++
		if opened > 1 && failing { // the probe's watch is the first
			return true, nil, apierrors.NewForbidden(bindings.GroupResource(), "", errors.New("RBAC revoked"))
		}
		return false, nil, nil
	})
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	var clockMu sync.Mutex
	src.SetClockForTest(func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock })
	mu.Lock()
	failing = true
	mu.Unlock()
	t.Cleanup(src.Stop)
	if err := src.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	until(t, func() {}, func() bool { return src.Stale() != nil })
	if err := src.Stale(); !strings.Contains(err.Error(), "bindings.agentkourier.dev") {
		t.Errorf("Stale = %v; want it to name the resource", err)
	}
	if s, _ := set(t, src); len(s.Bindings) != 2 {
		t.Errorf("the cache was not kept while stale: %d Bindings", len(s.Bindings))
	}
	mu.Lock()
	failing = false
	mu.Unlock()
	clockMu.Lock()
	clock = clock.Add(5 * time.Minute)
	clockMu.Unlock()
	if err := src.Stale(); err != nil {
		t.Errorf("Stale = %v a while after the failures stopped; want nil", err)
	}
}

// reflectors counts the goroutines of informers that are running now.
func reflectors() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	return strings.Count(string(buf), "tools/cache.(*Reflector).ListAndWatch")
}

// Stop returns when the informers' goroutines are gone, not when they are on their way: checked at once, not by a check
// that retries.
func TestStopWaitsForTheInformers(t *testing.T) {
	for range 10 {
		src, err := crsource.New(crsource.Options{Client: newCluster(t, corpus(t)...).client, Connection: own})
		if err != nil {
			t.Fatal(err)
		}
		if err := src.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if reflectors() == 0 {
			t.Fatal("no informer is running after Start, so this test proves nothing")
		}
		src.Stop()
		if n := reflectors(); n != 0 {
			t.Fatalf("%d informers still run when Stop has returned", n)
		}
	}
}
