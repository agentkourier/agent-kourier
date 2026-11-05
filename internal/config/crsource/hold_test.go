package crsource_test

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

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

// deleteForRollback deletes what Helm deletes when it takes the rendered objects away, in its order as measured: the
// AgentBackend, then the Bindings by name, the ChatConnection last.
func (c *cluster) deleteForRollback(objs []*unstructured.Unstructured) {
	c.t.Helper()
	for _, name := range []string{"kagent-pilot", "chat-only", "sre-alerts", "slack-main"} {
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

// seenGone makes the cluster delete the object and waits until the informer's cache has seen it go, so that the read that follows
// finds it gone.
func (c *cluster) seenGone(t *testing.T, src *crsource.Source, kind string, objs []*unstructured.Unstructured, name string) {
	t.Helper()
	u := find(objs, name)
	until(t, func() { c.remove(u) }, func() bool { return src.CachedGenerationForTest(kind, u.GetNamespace(), u.GetName()) < 0 })
}

// read is Set on the source, which a test makes at the moment of its choosing on its clock.
func read(t *testing.T, src *crsource.Source) (*config.Set, string) {
	t.Helper()
	s, fp, err := src.Set(t.Context())
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	return s, fp
}

// A Binding deleted alone, while the connection exists, is still served at the first read that finds it gone, still served at a
// read that comes sooner than the floor after it, and stopped at the first read past the floor that finds nothing newly gone.
// While it is served it has no word in Invalid and no object for its status; and the Fingerprint says, at the poll after the
// floor, that the deletion is due, so that the Reloader reads again then and does not wait for its slow recheck.
func TestABindingDeletedAloneStopsAtTheFirstReadPastTheFloorThatFindsNothingNew(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src := c.startWithClock(t, clock)
	read(t, src)
	c.seenGone(t, src, "Binding", objs, "chat-only")

	s, fp := read(t, src)
	if len(s.Bindings) != 2 || len(s.Invalid) != 0 {
		t.Fatalf("at the first read that finds it gone: Bindings %v, Invalid %+v; want it still served and no word of it", bindingNames(s), s.Invalid)
	}
	if _, ok, _ := src.BindingStatus("sre", "chat-only"); ok {
		t.Error("a Binding that is deferred has an object for its status")
	}
	for _, wait := range []time.Duration{time.Second, 6 * time.Second} { // reads that come sooner than the floor
		clock.advance(wait)
		if s, _ := read(t, src); len(s.Bindings) != 2 {
			t.Fatalf("a read %s after the last that found it gone stopped it", wait)
		}
	}
	if got := src.Fingerprint(t.Context()); got != fp {
		t.Errorf("Fingerprint = %q before the floor; want the one of the last read, %q", got, fp)
	}
	clock.advance(time.Second) // 8 s after the read that found it gone
	due := src.Fingerprint(t.Context())
	if due == fp {
		t.Error("the Fingerprint does not say that the deletion is due, so the Reloader would wait for its recheck of a minute")
	}
	s, settled := read(t, src)
	if got := bindingNames(s); !slices.Equal(got, []string{"sre/sre-alerts"}) || len(s.Invalid) != 0 {
		t.Errorf("at the first read past the floor: Bindings %v, Invalid %+v; want it stopped", got, s.Invalid)
	}
	if got := src.Fingerprint(t.Context()); got != settled {
		t.Errorf("Fingerprint = %q after the deletion took effect; want the one that read returned, %q", got, settled)
	}
}

// A read that comes just after a deletion, as the minute's recheck or any caller's can, does not make it take effect: Helm may
// be between two deletes.
func TestAForcedReadJustAfterADeletionDoesNotSettleIt(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src := c.startWithClock(t, clock)
	read(t, src)
	c.seenGone(t, src, "Binding", objs, "chat-only")
	read(t, src)
	for _, wait := range []time.Duration{100 * time.Millisecond, 3 * time.Second, 4*time.Second + 800*time.Millisecond} { // 7.9 s in all
		clock.advance(wait)
		if s, _ := read(t, src); len(s.Bindings) != 2 {
			t.Fatalf("a read %s after the deletion was found settled it", clock.now.Sub(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)))
		}
	}
	clock.advance(100 * time.Millisecond)
	if s, _ := read(t, src); len(s.Bindings) != 1 {
		t.Errorf("a read at the floor did not settle it: Bindings %v", bindingNames(s))
	}
}

// Helm's order, with a read after the backend and a read after the Bindings, which is what a slow API server gives: at every one
// of those reads the Bindings and the backend are all served, with no word in Invalid, and when the connection goes they are
// held; nothing is rejected at any read.
func TestHelmsOrderWithReadsBetweenKeepsEverythingServedAtEveryRead(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src, logs := c.startLoggedWithClock(t, clock)
	read(t, src)
	servedAll := func(when string) {
		t.Helper()
		s, _ := read(t, src)
		if len(s.Bindings) != 2 || len(s.AgentBackends) != 1 || len(s.ChatConnections) != 1 || len(held(s)) != 0 {
			t.Fatalf("%s: Bindings %v, backends %d, held %v; want all served and nothing held", when, bindingNames(s), len(s.AgentBackends), held(s))
		}
		if rep := report(t, src); len(rep.Snapshot.Bindings()) != 2 || len(rep.Rejections) != 0 {
			t.Fatalf("%s: %d Bindings resolve, rejections %+v", when, len(rep.Snapshot.Bindings()), rep.Rejections)
		}
	}

	c.seenGone(t, src, "AgentBackend", objs, "kagent-pilot")
	servedAll("after the backend")
	clock.advance(time.Second)
	c.seenGone(t, src, "Binding", objs, "chat-only")
	c.seenGone(t, src, "Binding", objs, "sre-alerts")
	servedAll("after the Bindings")
	clock.advance(5 * time.Second)
	servedAll("a read inside the floor")
	clock.advance(2 * time.Second) // 8 s after the backend went, but 7 after the last that went: the floor runs from that one
	servedAll("a read 8 s after the backend and 7 after the Bindings")
	c.seenGone(t, src, "ChatConnection", objs, "slack-main")

	s, _ := read(t, src)
	if got := strings.Join(held(s), " "); got != allHeld {
		t.Errorf("held = %s; want %s", got, allHeld)
	}
	rep := report(t, src)
	if n := len(rep.Snapshot.Bindings()); n != 2 || len(rep.Rejections) != 0 {
		t.Errorf("%d Bindings served, rejections %+v; want both served and nothing rejected", n, rep.Rejections)
	}
	if !strings.Contains(logs.String(), "held_bindings=2") || !strings.Contains(logs.String(), "held_backends=1") || !strings.Contains(logs.String(), "held_without_backend=0") {
		t.Errorf("the Warn does not say what is held:\n%s", logs.String())
	}
}

// A steady stream of deletions does not keep a deletion deferred for ever: the one first found gone goneGrace ago takes effect
// at the read that is that late, whatever else that read finds, while the later ones are still served.
func TestAStreamOfDeletionsSettlesTheOldestAtTheGrace(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	var streamed []*unstructured.Unstructured
	for i := range 8 {
		b := find(objs, "chat-only")
		b.SetName("s" + strconv.Itoa(i))
		b.SetUID(types.UID("uid-s" + strconv.Itoa(i)))
		_ = unstructured.SetNestedField(b.Object, "C099999999"+strconv.Itoa(i), "spec", "chat", "channel")
		streamed = append(streamed, b)
	}
	all := slices.Concat(objs, streamed)
	c := newCluster(t, all...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src := c.startWithClock(t, clock)
	read(t, src)
	served := func(s *config.Set) map[string]bool {
		m := map[string]bool{}
		for _, n := range bindingNames(s) {
			m[n] = true
		}
		return m
	}
	for i := range 7 { // one a read, every 10 s: each read finds something newly gone
		c.seenGone(t, src, "Binding", all, "s"+strconv.Itoa(i))
		s, _ := read(t, src)
		if got := served(s); i < 6 && !got["sre/s0"] {
			t.Fatalf("at +%ds s0 is not served: a stream of deletions must defer it until the grace, and not past it", i*10)
		}
		if i == 6 {
			if got := served(s); got["sre/s0"] {
				t.Errorf("at +60s s0 is still served: it has been gone for the grace, and takes effect whatever else is found")
			} else if !got["sre/s1"] || !got["sre/s6"] {
				t.Errorf("at +60s the later deletions are not served: %v", bindingNames(s))
			}
			break
		}
		clock.advance(10 * time.Second)
	}
}

// A deletion that a read deferred and that comes back with the same uid, which a list that missed it does, clears: it is served,
// and nothing makes it take effect later.
func TestADeferredObjectThatComesBackWithTheSameUIDClears(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src, logs := c.startLoggedWithClock(t, clock)
	read(t, src)
	c.seenGone(t, src, "Binding", objs, "chat-only")
	read(t, src)
	back := find(objs, "chat-only")
	until(t, func() { c.apply(back) }, func() bool { return src.CachedGenerationForTest("Binding", "sre", "chat-only") >= 0 })
	for _, wait := range []time.Duration{time.Second, 30 * time.Second, 2 * time.Minute} {
		clock.advance(wait)
		if s, _ := read(t, src); len(s.Bindings) != 2 || len(s.Invalid) != 0 {
			t.Fatalf("%s after it came back: Bindings %v, Invalid %+v; want it served, nothing deferred", wait, bindingNames(s), s.Invalid)
		}
	}
	if strings.Contains(logs.String(), "deleted") {
		t.Errorf("a deletion was logged for an object that came back:\n%s", logs.String())
	}
}

// A Binding made again under the name while the deletion is deferred is the new object: served as itself with its own uid, and
// not as the generation that was deferred; and one made again invalid is refused and not served from the deferred generation.
func TestABindingMadeAgainWhileItsDeletionIsDeferredIsTheNewOne(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src := c.startWithClock(t, clock)
	read(t, src)
	c.seenGone(t, src, "Binding", objs, "chat-only")
	c.seenGone(t, src, "Binding", objs, "sre-alerts")
	read(t, src)

	fresh := find(objs, "chat-only")
	fresh.SetUID("uid-new")
	_ = unstructured.SetNestedField(fresh.Object, "C0999999991", "spec", "chat", "channel")
	bad := find(objs, "sre-alerts")
	bad.SetUID("uid-bad")
	_ = unstructured.SetNestedField(bad.Object, "#general", "spec", "chat", "channel")
	until(t, func() { c.apply(fresh); c.apply(bad) }, func() bool {
		return src.CachedGenerationForTest("Binding", "sre", "chat-only") >= 0 && src.CachedGenerationForTest("Binding", "sre", "sre-alerts") >= 0
	})
	s, _ := read(t, src)
	got := ""
	for _, b := range s.Bindings {
		if b.Name == "chat-only" {
			got = string(b.UID) + " " + b.Spec.Chat.Channel
		}
		if b.Name == "sre-alerts" {
			t.Error("sre-alerts is served although it was made again invalid: not from the deferred generation")
		}
	}
	if got != "uid-new C0999999991" {
		t.Errorf("chat-only served as %q; want the new object", got)
	}
	refused := false
	for _, inv := range s.Invalid {
		refused = refused || (inv.Name == "sre-alerts" && !inv.Deleted && inv.Served == 0)
	}
	if !refused {
		t.Errorf("Invalid = %+v; want sre-alerts refused and not served", s.Invalid)
	}
}

// A deferred object that comes back with the same uid and is refused is served from its last good generation, as any refused
// object is, and its deletion is the news again when it goes the second time: it is deferred anew, not settled at once because
// the first deletion was found long ago.
func TestADeferredObjectThatComesBackRefusedIsDeferredAgainWhenItGoesAgain(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src := c.startWithClock(t, clock)
	read(t, src)
	c.seenGone(t, src, "Binding", objs, "sre-alerts")
	read(t, src)
	clock.advance(20 * time.Second)
	back := find(objs, "sre-alerts") // the same uid, as a list that missed it gives, and now refused
	back.SetGeneration(2)
	_ = unstructured.SetNestedField(back.Object, "#general", "spec", "chat", "channel")
	until(t, func() { c.apply(back) }, func() bool { return src.CachedGenerationForTest("Binding", "sre", "sre-alerts") == 2 })
	if s, _ := read(t, src); len(s.Bindings) != 2 || len(s.Invalid) != 1 || s.Invalid[0].Served != 1 {
		t.Fatalf("Bindings %v, Invalid %+v; want it served from its last good generation and refused", bindingNames(s), s.Invalid)
	}
	c.seenGone(t, src, "Binding", []*unstructured.Unstructured{back}, "sre-alerts")
	clock.advance(time.Second)
	if s, _ := read(t, src); len(s.Bindings) != 2 {
		t.Errorf("Bindings %v one second after it went the second time; want it deferred anew and still served", bindingNames(s))
	}
}

// A Binding made again under the name with a new uid and refused is another object than the one that was last good, which is gone
// for good: when it too is deleted, the deferral does not serve the generation of the one before.
func TestADeferredGenerationOfAnotherObjectIsNotServedWhenTheNewOneGoes(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src := c.startWithClock(t, clock)
	read(t, src)
	c.seenGone(t, src, "Binding", objs, "sre-alerts")
	read(t, src)
	bad := find(objs, "sre-alerts")
	bad.SetUID("uid-bad")
	_ = unstructured.SetNestedField(bad.Object, "#general", "spec", "chat", "channel")
	until(t, func() { c.apply(bad) }, func() bool { return src.CachedGenerationForTest("Binding", "sre", "sre-alerts") >= 0 })
	read(t, src)
	c.seenGone(t, src, "Binding", []*unstructured.Unstructured{bad}, "sre-alerts")
	if s, _ := read(t, src); slices.Contains(bindingNames(s), "sre/sre-alerts") {
		t.Errorf("Bindings %v; the generation of the object before is served after the one that replaced it went", bindingNames(s))
	}
}

// The trade of the grace: a Binding deleted alone is kept, not served, for goneGrace after its deletion takes effect, and if its
// connection is deleted inside it, it comes back, held. After the grace it does not, and a read inside the grace does not start
// the grace again.
func TestABindingDeletedAloneComesBackHeldOnlyInsideTheGrace(t *testing.T) {
	const notHeld = "AgentBackend agent-kourier-system/kagent-pilot@1 Binding sre/sre-alerts@1 ChatConnection agent-kourier-system/slack-main@1"
	for _, tc := range []struct {
		name  string
		reads []time.Duration // the reads after the one that found it gone, each this long after the one before: the first settles it
		want  string
	}{
		{"inside the grace", []time.Duration{10 * time.Second, 30 * time.Second}, allHeld},
		{"late inside the grace", []time.Duration{10 * time.Second, 55 * time.Second}, allHeld}, // 65 s after it was found gone, 55 after it took effect
		{"after the grace", []time.Duration{10 * time.Second, 61 * time.Second}, notHeld},
		// The grace is counted from the read at which the deletion took effect, not from the latest: a read inside it does not start it again.
		{"read again inside it, then past it", []time.Duration{10 * time.Second, 30 * time.Second, 31 * time.Second}, notHeld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leakCheck(t)
			objs := corpus(t)
			c := newCluster(t, objs...)
			clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
			src := c.startWithClock(t, clock)
			read(t, src)
			c.seenGone(t, src, "Binding", objs, "chat-only")
			read(t, src) // found gone: deferred
			for _, step := range tc.reads {
				clock.advance(step)
				read(t, src)
			}
			until(t, func() {
				c.remove(find(objs, "kagent-pilot"))
				c.remove(find(objs, "sre-alerts"))
				c.remove(find(objs, "slack-main"))
			}, func() bool { s, _ := read(t, src); return len(held(s)) >= 3 })
			s, _ := read(t, src)
			if got := strings.Join(held(s), " "); got != tc.want {
				t.Errorf("held = %s; want %s", got, tc.want)
			}
		})
	}
}

// What is held does not expire during the hold, however long it lasts; and a backend that is gone is not served while its
// connection is there, once its deletion has taken effect.
func TestWhatIsHeldDoesNotExpire(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src := c.startWithClock(t, clock)
	read(t, src)
	c.seenGone(t, src, "AgentBackend", objs, "kagent-pilot")
	read(t, src)
	clock.advance(10 * time.Second)
	if s, _ := read(t, src); len(s.AgentBackends) != 0 {
		t.Fatalf("a backend that is gone is served while its connection is there, after its deletion took effect")
	}
	until(t, func() { c.deleteForRollback(objs) }, func() bool { s, _ := read(t, src); return len(held(s)) == 4 })
	for range 3 {
		clock.advance(10 * time.Minute)
		if s, _ := read(t, src); strings.Join(held(s), " ") != allHeld {
			t.Fatalf("held = %v after the clock moved; want it all, for as long as the connection is held", held(s))
		}
	}
}

// A hold whose Bindings name a backend that is gone for longer than the grace says so: the Bindings are held and cannot be
// served, and the Warn counts them.
func TestTheWarnCountsHeldBindingsWithoutABackend(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src, logs := c.startLoggedWithClock(t, clock)
	read(t, src)
	c.seenGone(t, src, "AgentBackend", objs, "kagent-pilot")
	read(t, src)
	clock.advance(10 * time.Second)
	read(t, src) // its deletion takes effect
	clock.advance(61 * time.Second)
	read(t, src) // and the grace after it has run: the backend is forgotten
	until(t, func() {
		c.remove(find(objs, "chat-only"))
		c.remove(find(objs, "sre-alerts"))
		c.remove(find(objs, "slack-main"))
	},
		func() bool { s, _ := read(t, src); return len(held(s)) == 3 })
	if !strings.Contains(logs.String(), "held_bindings=2") || !strings.Contains(logs.String(), "held_backends=0") || !strings.Contains(logs.String(), "held_without_backend=2") {
		t.Errorf("the Warn does not count the Bindings that have no backend:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "bringing the connection back and then deleting the Binding") {
		t.Errorf("the Warn does not say how to stop a Binding during the hold:\n%s", logs.String())
	}
}

// The backend of a Binding of the held connection that is still there is held when it goes: it is a tenant's Binding on a
// backend that was rendered with the release.
func TestTheBackendOfALiveBindingOfAHeldConnectionIsHeld(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, objs...)
	src := c.start(t)
	set(t, src)
	until(t, func() { c.remove(find(objs, "kagent-pilot")); c.remove(find(objs, "slack-main")) },
		func() bool { s, _ := set(t, src); return len(held(s)) == 2 })
	s, _ := set(t, src)
	want := "AgentBackend agent-kourier-system/kagent-pilot@1 ChatConnection agent-kourier-system/slack-main@1"
	if got := strings.Join(held(s), " "); got != want || len(s.Bindings) != 2 {
		t.Errorf("held = %s, Bindings %v; want %s held and both Bindings still there, which resolve", got, bindingNames(s), want)
	}
	if rep := report(t, src); len(rep.Snapshot.Bindings()) != 2 {
		t.Errorf("%d Bindings served; want both, on the held backend", len(rep.Snapshot.Bindings()))
	}
}

// The references of the chart's own objects name no namespace, since they are all in the release's: a hold that read a
// reference without the Binding's namespace would find neither the connection nor the backend.
func TestAHoldReadsReferencesThatNameNoNamespace(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	for _, name := range []string{"kagent-pilot", "slack-main"} {
		_ = unstructured.SetNestedStringSlice(find(objs, name).Object, nil, "spec", "allowedNamespaces")
	}
	b := find(objs, "chat-only")
	b.SetNamespace("agent-kourier-system")
	_ = unstructured.SetNestedMap(b.Object, map[string]any{"name": "slack-main"}, "spec", "chat", "connectionRef")
	_ = unstructured.SetNestedMap(b.Object, map[string]any{"name": "kagent-pilot"}, "spec", "agent", "backendRef")
	all := []*unstructured.Unstructured{find(objs, "kagent-pilot"), find(objs, "slack-main"), b}
	c := newCluster(t, all...)
	src := c.start(t)
	if s, _ := set(t, src); len(s.Bindings) != 1 {
		t.Fatalf("Bindings = %v; the Binding with references of no namespace is not read", bindingNames(s))
	}
	until(t, func() {
		c.remove(find(all, "kagent-pilot"))
		c.remove(find(all, "chat-only"))
		c.remove(find(all, "slack-main"))
	}, func() bool { s, _ := set(t, src); return len(held(s)) == 3 })
	if rep := report(t, src); len(rep.Snapshot.Bindings()) != 1 || len(rep.Rejections) != 0 {
		t.Errorf("%d Bindings served, rejections %+v; want the one held, resolved against the held backend and connection", len(rep.Snapshot.Bindings()), rep.Rejections)
	}
}

// testClock is a clock a test moves.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *testClock) advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func (c *cluster) startWithClock(t *testing.T, clock *testClock) *crsource.Source {
	t.Helper()
	src, _ := c.startLoggedWithClock(t, clock)
	return src
}

func (c *cluster) startLoggedWithClock(t *testing.T, clock *testClock) (*crsource.Source, *logSink) {
	t.Helper()
	logs := &logSink{}
	src, err := crsource.New(crsource.Options{Client: c.client, Connection: own, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	src.SetClockForTest(clock.Now)
	t.Cleanup(src.Stop)
	if err := src.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return src, logs
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
	// What went while the first hold lasted and was dropped when the connection came back is not brought back by the second.
	s, _ = set(t, src)
	if got := strings.Join(held(s), " "); got != "ChatConnection agent-kourier-system/slack-main@1" || len(s.Bindings) != 1 {
		t.Errorf("held = %s, Bindings %v; want the connection alone: sre-alerts went with the first hold and stays gone", got, bindingNames(s))
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

// A Binding that went to another broker and is made invalid back onto this one has no last good here: the generation that went
// away is not served again under a new hold of the name.
func TestAMovedBindingMadeInvalidBackOnTheOwnConnectionHasNoLastGood(t *testing.T) {
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
	if s, _ := set(t, src); slices.Contains(bindingNames(s), "sre/sre-alerts") {
		t.Fatal("the Binding that went to another broker is still served")
	}
	back := find(objs, "sre-alerts")
	back.SetGeneration(3)
	_ = unstructured.SetNestedField(back.Object, "#general", "spec", "chat", "channel")
	until(t, func() { c.apply(back) }, func() bool { return src.CachedGenerationForTest("Binding", "sre", "sre-alerts") == 3 })
	s, _ := set(t, src)
	if slices.Contains(bindingNames(s), "sre/sre-alerts") {
		t.Errorf("Bindings = %v; sre-alerts is served from a generation that went to another broker", bindingNames(s))
	}
	refused := false
	for _, inv := range s.Invalid {
		refused = refused || (inv.Name == "sre-alerts" && inv.Served == 0 && !inv.Deleted)
	}
	if !refused {
		t.Errorf("Invalid = %+v; want sre-alerts refused with no generation served", s.Invalid)
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
