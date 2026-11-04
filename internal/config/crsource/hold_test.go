package crsource_test

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// held returns the Invalid entries that are held after a deletion, as "Kind ns/name@generation", sorted.
func held(s *config.Set) []string {
	var out []string
	for _, inv := range s.Invalid {
		if inv.Deleted {
			out = append(out, inv.Kind+" "+inv.Namespace+"/"+inv.Name+"@"+strconv.FormatInt(inv.Served, 10))
		}
	}
	slices.Sort(out)
	return out
}

// startLogged is start with the source's log in a sink the test can read.
func (c *cluster) startLogged(t *testing.T) (*crsource.Source, *logSink) {
	t.Helper()
	logs := &logSink{}
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Stop)
	if err := src.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return src, logs
}

// deleteForRollback deletes what Helm deletes when it takes the rendered objects away, in the order that no read can split:
// the connection first, so that no read sees a Binding gone and its connection still there.
func (c *cluster) deleteForRollback(objs []*unstructured.Unstructured) {
	c.t.Helper()
	for _, name := range []string{"slack-main", "kagent-pilot", "chat-only", "sre-alerts"} {
		c.remove(find(objs, name))
	}
}

const allHeld = "AgentBackend agent-kourier-system/kagent-pilot@1 Binding sre/chat-only@1 Binding sre/sre-alerts@1 ChatConnection agent-kourier-system/slack-main@1"

// While the own connection is deleted and held, the Bindings that named it and are gone, and the backend they name, are held
// with it at their last good generation, and resolve: a rollback that deletes all of them together keeps routing.
func TestTheBindingsAndBackendsOfAHeldConnectionAreHeldWithIt(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src, logs := c.startLogged(t)
	set(t, src) // the broker has read them: that is what makes them the last good
	until(t, func() { c.deleteForRollback(objs) }, func() bool { s, _ := set(t, src); return len(held(s)) == 4 })
	s, _ := set(t, src)
	if got := strings.Join(held(s), " "); got != allHeld {
		t.Errorf("held = %s; want %s", got, allHeld)
	}
	if got := bindingNames(s); len(got) != 2 || len(s.AgentBackends) != 1 || len(s.ChatConnections) != 1 {
		t.Errorf("Bindings %v, backends %d, connections %d; want both Bindings, the backend and the connection", got, len(s.AgentBackends), len(s.ChatConnections))
	}
	// They resolve: through a Reloader nothing is rejected, so no Binding falls to BackendNotFound.
	rep := report(t, src)
	if n := len(rep.Snapshot.Bindings()); n != 2 || len(rep.Rejections) != 0 {
		t.Errorf("%d Bindings served, rejections %+v; want both served and nothing rejected", n, rep.Rejections)
	}
	// Said once, at Warn, with how many; and not again by the reads that follow.
	set(t, src)
	if n := strings.Count(logs.String(), "was deleted: it is held"); n != 1 || !strings.Contains(logs.String(), "held_bindings=2") || !strings.Contains(logs.String(), "held_backends=1") {
		t.Errorf("the hold was said %d times; want once, with 2 Bindings and 1 backend\n%s", n, logs.String())
	}
}

// Only the backends that the held Bindings name are held, and only Bindings that named the connection: one that named none
// that exists was rejected, and stays so.
func TestOnlyWhatTheHeldBindingsNeedIsHeld(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	unused := find(objs, "kagent-pilot")
	unused.SetName("unused")
	unused.SetUID("uid-unused")
	dangling := find(objs, "chat-only")
	dangling.SetName("dangling")
	dangling.SetUID("uid-dangling")
	_ = unstructured.SetNestedField(dangling.Object, "C0999999990", "spec", "chat", "channel")
	_ = unstructured.SetNestedField(dangling.Object, "nowhere", "spec", "chat", "connectionRef", "name")
	all := slices.Concat(objs, []*unstructured.Unstructured{unused, dangling})
	c := newCluster(t, all...)
	src := c.start(t)
	s, _ := set(t, src)
	if len(bindingNames(s)) != 3 || len(s.AgentBackends) != 2 {
		t.Fatalf("Bindings %v, backends %d; want three and two before the hold", bindingNames(s), len(s.AgentBackends))
	}
	until(t, func() {
		c.deleteForRollback(objs)
		c.remove(unused)
		c.remove(dangling)
	}, func() bool {
		s, _ := set(t, src) // all four are held, and the two that are not wanted are gone from the cache too
		return len(held(s)) == 4 && src.CachedGenerationForTest("AgentBackend", "agent-kourier-system", "unused") < 0 &&
			src.CachedGenerationForTest("Binding", "sre", "dangling") < 0
	})
	s, _ = set(t, src)
	if got := strings.Join(held(s), " "); got != allHeld {
		t.Errorf("held = %s; want %s: not the backend nothing used, and not the Binding of no connection", got, allHeld)
	}
}

// A Binding deleted alone, while the connection exists, stops at the next read, and is not brought back when the connection
// is deleted later: the hold is for the connection, and the kill switch of a Binding is its deletion.
func TestABindingDeletedAloneIsNotHeld(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	set(t, src)
	until(t, func() { c.remove(find(objs, "chat-only")) }, func() bool { s, _ := set(t, src); return len(s.Bindings) == 1 })
	s, _ := set(t, src)
	if len(held(s)) != 0 || len(s.Invalid) != 0 {
		t.Fatalf("held %v, Invalid %+v with the connection there; want nothing held", held(s), s.Invalid)
	}
	until(t, func() {
		c.remove(find(objs, "slack-main"))
		c.remove(find(objs, "kagent-pilot"))
		c.remove(find(objs, "sre-alerts"))
	}, func() bool { s, _ := set(t, src); return len(held(s)) == 3 })
	s, _ = set(t, src)
	want := "AgentBackend agent-kourier-system/kagent-pilot@1 Binding sre/sre-alerts@1 ChatConnection agent-kourier-system/slack-main@1"
	if got := strings.Join(held(s), " "); got != want {
		t.Errorf("held = %s; want %s: chat-only was stopped, and stays stopped", got, want)
	}
}

// A Binding made again during the hold is the new object, with its new uid and generation, and never the held one; and one
// that is made again invalid is refused, not served from the generation held under the old uid.
func TestABindingMadeAgainDuringTheHoldIsNotTheHeldOne(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	set(t, src)
	until(t, func() { c.deleteForRollback(objs) }, func() bool { s, _ := set(t, src); return len(held(s)) == 4 })

	fresh := find(objs, "chat-only")
	fresh.SetUID("uid-new")
	fresh.SetGeneration(1)
	_ = unstructured.SetNestedField(fresh.Object, "C0999999991", "spec", "chat", "channel")
	bad := find(objs, "sre-alerts")
	bad.SetUID("uid-bad")
	_ = unstructured.SetNestedField(bad.Object, "#general", "spec", "chat", "channel")
	until(t, func() { c.apply(fresh); c.apply(bad) }, func() bool {
		return src.CachedGenerationForTest("Binding", "sre", "chat-only") == 1 && src.CachedGenerationForTest("Binding", "sre", "sre-alerts") == 1
	})
	s, _ := set(t, src)
	got := ""
	for _, b := range s.Bindings {
		if b.Name == "chat-only" {
			got = string(b.UID) + " " + b.Spec.Chat.Channel
		}
		if b.Name == "sre-alerts" {
			t.Errorf("sre-alerts is served although it was made again invalid: it must not be served from the generation held under the old uid")
		}
	}
	if got != "uid-new C0999999991" {
		t.Errorf("chat-only served as %q; want the new object, uid-new on C0999999991", got)
	}
	for _, h := range held(s) {
		if strings.HasPrefix(h, "Binding") {
			t.Errorf("%s is still held: it is listed, and is no held object", h)
		}
	}
	refused := false
	for _, inv := range s.Invalid {
		refused = refused || (inv.Name == "sre-alerts" && !inv.Deleted && inv.Served == 0)
	}
	if !refused {
		t.Errorf("Invalid = %+v; want sre-alerts refused and not served", s.Invalid)
	}
}

// When the connection is back the hold ends: what is still gone goes, with a word at Info, and what was made again stays.
func TestTheHoldEndsWhenTheConnectionIsBack(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src, logs := c.startLogged(t)
	set(t, src)
	until(t, func() { c.deleteForRollback(objs) }, func() bool { s, _ := set(t, src); return len(held(s)) == 4 })

	conn := find(objs, "slack-main")
	conn.SetUID("uid-conn-2")
	back := find(objs, "kagent-pilot") // the backend returns too, and one Binding with it
	back.SetUID("uid-be-2")
	one := find(objs, "chat-only")
	one.SetUID("uid-b-2")
	until(t, func() { c.apply(conn); c.apply(back); c.apply(one) }, func() bool {
		return src.CachedGenerationForTest("ChatConnection", "agent-kourier-system", "slack-main") >= 0 &&
			src.CachedGenerationForTest("Binding", "sre", "chat-only") >= 0 && src.CachedGenerationForTest("AgentBackend", "agent-kourier-system", "kagent-pilot") >= 0
	})
	s, _ := set(t, src)
	if got := bindingNames(s); !slices.Equal(got, []string{"sre/chat-only"}) {
		t.Errorf("Bindings = %v; want only chat-only: sre-alerts is still gone, and goes now the connection is back", got)
	}
	if len(s.Invalid) != 0 || len(held(s)) != 0 {
		t.Errorf("Invalid = %+v; want nothing held once the connection is back", s.Invalid)
	}
	if !strings.Contains(logs.String(), "is back") {
		t.Errorf("the end of the hold was not said:\n%s", logs.String())
	}
	// Deleted again, it is held again, and said again.
	until(t, func() { c.remove(find(objs, "slack-main")) }, func() bool { s, _ := set(t, src); return len(held(s)) > 0 })
	if n := strings.Count(logs.String(), "was deleted: it is held"); n != 2 {
		t.Errorf("a second hold was said %d times in all; want 2", n)
	}
}

// A Binding that is still there, and was edited onto another broker's connection, has gone to that broker: it is not gone, and
// the hold does not bring it back when the own connection is deleted.
func TestABindingMovedToAnotherBrokerIsNotHeld(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	other := find(objs, "slack-main")
	other.SetName("slack-other")
	other.SetUID("uid-other")
	c := newCluster(t, append(objs, other)...)
	src := c.start(t)
	set(t, src)
	moved := find(objs, "sre-alerts")
	moved.SetGeneration(2)
	_ = unstructured.SetNestedField(moved.Object, "slack-other", "spec", "chat", "connectionRef", "name")
	until(t, func() { c.apply(moved) }, func() bool { return src.CachedGenerationForTest("Binding", "sre", "sre-alerts") == 2 })
	until(t, func() {
		c.remove(find(objs, "slack-main"))
		c.remove(find(objs, "chat-only"))
		c.remove(find(objs, "kagent-pilot"))
	}, func() bool { s, _ := set(t, src); return len(held(s)) == 3 })
	s, _ := set(t, src)
	want := "AgentBackend agent-kourier-system/kagent-pilot@1 Binding sre/chat-only@1 ChatConnection agent-kourier-system/slack-main@1"
	if got := strings.Join(held(s), " "); got != want {
		t.Errorf("held = %s; want %s: sre-alerts is another broker's now", got, want)
	}
}

// A broker that starts with its connection already deleted has no last good to hold: it holds nothing, and the start finds the
// connection missing, as it always did.
func TestAFreshSourceHoldsNothing(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs[:1]...) // only the backend: no connection, no Binding
	s, _ := set(t, c.start(t))
	if len(s.Invalid) != 0 || len(s.ChatConnections) != 0 || len(s.Bindings) != 0 {
		t.Errorf("a source with no last good held something: connections %d, Bindings %d, Invalid %+v", len(s.ChatConnections), len(s.Bindings), s.Invalid)
	}
}

// report resolves what src reads through a per-object Reloader and returns its report.
func report(t *testing.T, src config.Source) config.Report {
	t.Helper()
	r := config.NewReloader(config.ReloaderConfig{Source: src, Secrets: noSecrets{}, PollInterval: time.Hour, Logger: slog.New(slog.DiscardHandler), PerObject: true})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	r.Wait()
	return r.Report()
}
