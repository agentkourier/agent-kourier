package crsource_test

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
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

// The probe sees an outage that no informer reports: a list that is refused fails, and the source is stale from then until a
// list passes, with no clock to wait out.
func TestStaleWhileTheAPIRefusesTheConnection(t *testing.T) {
	leakCheck(t)
	c := newCluster(t, corpus(t)...)
	var mu sync.Mutex
	down := false
	c.client.PrependReactor("list", "chatconnections", func(clienttesting.Action) (bool, apiruntime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return true, nil, errors.New(`Get "https://10.96.0.1:443/apis/agentkourier.dev/v1alpha1/chatconnections?limit=1": dial tcp 10.96.0.1:443: connect: connection refused`)
		}
		return false, nil, nil
	})
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own})
	if err != nil {
		t.Fatal(err)
	}
	src.SetProbeForTest(20*time.Millisecond, time.Second)
	t.Cleanup(src.Stop)
	if err := src.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := src.Stale(); err != nil {
		t.Fatalf("Stale = %v while the API answers", err)
	}
	mu.Lock()
	down = true
	mu.Unlock()
	until(t, func() {}, func() bool { return src.Stale() != nil })
	if err := src.Stale(); !strings.Contains(err.Error(), "chatconnections.agentkourier.dev") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Stale = %v; want it to name the resource and the refusal", err)
	}
	if s, _ := set(t, src); len(s.Bindings) != 2 {
		t.Errorf("the cache was not kept during the outage: %d Bindings", len(s.Bindings))
	}
	mu.Lock()
	down = false
	mu.Unlock()
	until(t, func() {}, func() bool { return src.Stale() == nil })
}

// A list that is never answered fails the probe at its timeout, and neither holds up the other kinds nor Stop.
func TestStaleWhileTheAPIDoesNotAnswerAndStopIsNotHeldUp(t *testing.T) {
	leakCheck(t)
	c := newCluster(t, corpus(t)...)
	release := make(chan struct{})
	var mu sync.Mutex
	hang, hung := false, 0
	c.client.PrependReactor("list", "bindings", func(clienttesting.Action) (bool, apiruntime.Object, error) {
		mu.Lock()
		h := hang
		if h {
			hung++
		}
		mu.Unlock()
		if h {
			<-release // a client that ignores its context, as the fake does
		}
		return false, nil, nil
	})
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own})
	if err != nil {
		t.Fatal(err)
	}
	src.SetProbeForTest(20*time.Millisecond, 50*time.Millisecond)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); src.Stop() })
	if err := src.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	hang = true
	mu.Unlock()
	until(t, func() {}, func() bool { return src.Stale() != nil })
	if err := src.Stale(); !strings.Contains(err.Error(), "no answer in 50ms") || !strings.Contains(err.Error(), "bindings.agentkourier.dev") {
		t.Errorf("Stale = %v; want it to say that bindings did not answer in 50ms", err)
	}
	stopped := make(chan struct{})
	go func() { src.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop is held up by a list that is never answered")
	}
	once.Do(func() { close(release) })
}

// The start's watch is closed: it was only to see whether the Role allows one.
func TestStartClosesTheWatchItOpenedToProbe(t *testing.T) {
	leakCheck(t)
	c := newCluster(t, corpus(t)...)
	var mu sync.Mutex
	var fakes []*watch.FakeWatcher
	c.client.PrependWatchReactor("agentbackends", func(clienttesting.Action) (bool, watch.Interface, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(fakes) > 0 { // the informer's own
			return false, nil, nil
		}
		fw := watch.NewFake()
		fakes = append(fakes, fw)
		return true, fw, nil
	})
	c.start(t)
	mu.Lock()
	defer mu.Unlock()
	if len(fakes) != 1 || !fakes[0].IsStopped() {
		t.Errorf("%d probe watches opened, stopped %v; want one, stopped", len(fakes), len(fakes) == 1 && fakes[0].IsStopped())
	}
}

// A valid edit that moves a served Binding onto another broker's connection is that broker's now, and this one lets it go
// without a word: no Invalid, and no routing.
func TestAValidEditOntoAnotherBrokersConnectionLetsTheBindingGo(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	other := find(objs, "slack-main")
	other.SetName("slack-other")
	other.SetUID("uid-other")
	c := newCluster(t, append(objs, other)...)
	src := c.start(t)
	_, fp := set(t, src)
	moved := find(objs, "sre-alerts")
	moved.SetGeneration(2)
	_ = unstructured.SetNestedField(moved.Object, "slack-other", "spec", "chat", "connectionRef", "name")
	until(t, func() { c.apply(moved) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	s, _ := set(t, src)
	if got := bindingNames(s); len(got) != 1 || got[0] != "sre/chat-only" || len(s.Invalid) != 0 {
		t.Errorf("Bindings %v, Invalid %+v; want only chat-only and nothing reported", got, s.Invalid)
	}
}

// A Binding on a connection that does not exist is served here, as one that someone must report. When that connection appears
// as another broker's, and the Binding is edited invalid in the same breath, the last good generation says it was ours and
// the connection now says it is not: it is left out, and not reported.
func TestABindingWhoseConnectionBecomesAnotherBrokersAndIsEditedInvalidIsLeftOut(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	late := find(objs, "chat-only")
	late.SetName("late")
	late.SetUID("uid-late")
	_ = unstructured.SetNestedField(late.Object, "C0999999990", "spec", "chat", "channel")
	_ = unstructured.SetNestedField(late.Object, "slack-later", "spec", "chat", "connectionRef", "name")
	c := newCluster(t, append(objs, late)...)
	src := c.start(t)
	s, fp := set(t, src)
	if got := bindingNames(s); len(got) != 3 {
		t.Fatalf("Bindings = %v; want late served, its connection being none that exists", got)
	}
	later := find(objs, "slack-main")
	later.SetName("slack-later")
	later.SetUID("uid-later")
	bad := late.DeepCopy()
	bad.SetGeneration(2)
	_ = unstructured.SetNestedField(bad.Object, "#general", "spec", "chat", "channel")
	until(t, func() { c.apply(later); c.apply(bad) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	time.Sleep(300 * time.Millisecond) // both are in the cache before the next read
	s, _ = set(t, src)
	if got := bindingNames(s); len(got) != 2 || len(s.Invalid) != 0 {
		t.Errorf("Bindings %v, Invalid %+v; want late left out and not reported", got, s.Invalid)
	}
}

// The namespaces that a refused object allows are read from the raw object, which could not be decoded: through the
// Reloader, a Binding that it allows is told the object is rejected, and one that it does not allow is told what a valid
// object would tell it.
func TestAllowedNamespacesOfARefusedObjectAreReadFromTheRawObject(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	refused := find(objs, "kagent-pilot")
	refused.SetName("refused")
	refused.SetUID("uid-refused")
	_ = unstructured.SetNestedField(refused.Object, "ftp://internal.example/?token=s3cret", "spec", "url")
	_ = unstructured.SetNestedStringSlice(refused.Object, []string{"sre"}, "spec", "allowedNamespaces")
	allowed := find(objs, "chat-only")
	allowed.SetName("allowed")
	allowed.SetUID("uid-allowed")
	_ = unstructured.SetNestedField(allowed.Object, "C0999999991", "spec", "chat", "channel")
	_ = unstructured.SetNestedField(allowed.Object, "refused", "spec", "agent", "backendRef", "name")
	denied := find(objs, "chat-only")
	denied.SetNamespace("payments")
	denied.SetName("denied")
	denied.SetUID("uid-denied")
	_ = unstructured.SetNestedField(denied.Object, "C0999999992", "spec", "chat", "channel")
	_ = unstructured.SetNestedField(denied.Object, "refused", "spec", "agent", "backendRef", "name")
	src := newCluster(t, append(objs, refused, allowed, denied)...).start(t)

	r := config.NewReloader(config.ReloaderConfig{Source: src, Secrets: noSecrets{}, PollInterval: time.Hour, Logger: slog.New(slog.DiscardHandler), PerObject: true})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]config.RejectionReason{}
	for _, rej := range r.Report().Rejections {
		got[rej.Name] = rej.Reason
		if strings.Contains(rej.Message, "s3cret") {
			t.Errorf("%s: %s quotes the owner's text", rej.Name, rej.Message)
		}
	}
	if got["allowed"] != config.ReasonBackendRejected || got["denied"] != config.ReasonNamespaceNotAllowed {
		t.Errorf("reasons = %v; want allowed BackendRejected and denied NamespaceNotAllowed", got)
	}
}

type noSecrets struct{}

func (noSecrets) Keys(context.Context, string, string) ([]string, error) { return nil, nil }
