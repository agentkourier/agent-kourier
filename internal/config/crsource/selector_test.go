package crsource_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	clienttesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// admitLabel is the label the tests' selector matches, with the value "yes".
const admitLabel = "kourier-admitted"

var admitSelector = labels.SelectorFromSet(labels.Set{admitLabel: "yes"})

// namespaceObject returns the Namespace name at the resourceVersion given, with the labels given as key, value pairs.
func namespaceObject(name, resourceVersion string, kv ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("Namespace")
	u.SetName(name)
	u.SetUID(types.UID("uid-ns-" + name))
	u.SetResourceVersion(resourceVersion)
	if len(kv) > 0 {
		l := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			l[kv[i]] = kv[i+1]
		}
		u.SetLabels(l)
	}
	return u
}

// startWith starts a Source over the cluster with opts, its Client filled in, its log in a sink and, when clock is not nil, a clock the
// test moves. It is stopped when the test ends.
func (c *cluster) startWith(t *testing.T, clock *testClock, opts crsource.Options) (*crsource.Source, *logSink) {
	t.Helper()
	logs := &logSink{}
	opts.Client, opts.Logger = c.client, slog.New(slog.NewTextHandler(logs, nil))
	src, err := crsource.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if clock != nil {
		src.SetClockForTest(clock.Now)
	}
	t.Cleanup(src.Stop)
	if err := src.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return src, logs
}

// namespaceRequests counts the requests that were made of namespaces through the cluster's client.
func (c *cluster) namespaceRequests() int {
	n := 0
	for _, a := range c.client.Actions() {
		if a.GetResource() == namespaces {
			n++
		}
	}
	return n
}

// logLines counts the lines of the log that hold every one of the parts.
func logLines(logs *logSink, parts ...string) int {
	n := 0
	for line := range strings.Lines(logs.String()) {
		if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) }) {
			n++
		}
	}
	return n
}

const (
	admittedMsg    = "a namespace is admitted by the connection namespace selector"
	notAdmittedMsg = "a namespace is no longer admitted by the connection namespace selector"
)

// A namespace that gains the selector's label is admitted at the next read: its ChatConnection and the Bindings on it are in the Set,
// and the fingerprint moved so that the Reloader reads it at its next poll. One that loses the label, or is deleted, is no longer
// admitted at the next read, its connection and Bindings leave the Set at once (they are another broker's, not a deletion), and the
// connection is not listed as unserved for the cap: ConnectionBindings still names its Bindings, for the broker to tell them. Each
// change is said once at Info, by name.
func TestASelectorAdmitsANamespaceByItsLabelsAtTheNextRead(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	nsA := namespaceObject("team-a", "1")
	extra := []*unstructured.Unstructured{
		teamConnection(objs, "team-a", "slack", created),
		teamConnection(objs, "team-b", "slack", created),
		bindingOn(objs, "team-a", "on-a", "team-a", "slack"),
		bindingOn(objs, "team-b", "on-b", "team-b", "slack"),
		nsA,
		namespaceObject("team-b", "1", admitLabel, "yes"),
		namespaceObject("agent-kourier-system", "1"),
	}
	c := newCluster(t, append(objs, extra...)...)
	src, logs := c.startWith(t, nil, crsource.Options{ConnectionNamespaceSelector: admitSelector})

	s, fp := set(t, src)
	if got := connectionNames(s); !slices.Equal(got, []string{"team-b/slack"}) {
		t.Fatalf("connections = %v; want team-b's alone: team-a is not labelled, and slack-main's namespace neither", got)
	}
	if got := bindingNames(s); !slices.Equal(got, []string{"team-b/on-b"}) {
		t.Errorf("Bindings = %v; want team-b's", got)
	}

	labelled := namespaceObject("team-a", "2", admitLabel, "yes")
	until(t, func() { c.apply(labelled) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	s, fp = set(t, src)
	if got := connectionNames(s); !slices.Equal(got, []string{"team-a/slack", "team-b/slack"}) {
		t.Errorf("connections = %v after the label; want team-a's and team-b's", got)
	}
	if got := bindingNames(s); !slices.Equal(got, []string{"team-a/on-a", "team-b/on-b"}) {
		t.Errorf("Bindings = %v after the label; want team-a's and team-b's", got)
	}
	set(t, src)
	if n := logLines(logs, admittedMsg, "namespace=team-a", "level=INFO"); n != 1 {
		t.Errorf("team-a was said admitted %d times; want once, at Info\n%s", n, logs.String())
	}
	if n := logLines(logs, admittedMsg, "namespace=team-b", "level=INFO"); n != 1 {
		t.Errorf("team-b was said admitted %d times; want once, at the first read\n%s", n, logs.String())
	}

	until(t, func() { c.apply(namespaceObject("team-a", "3")) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	s, fp = set(t, src)
	if got := connectionNames(s); !slices.Equal(got, []string{"team-b/slack"}) {
		t.Errorf("connections = %v after the label is removed; want team-b's alone", got)
	}
	if got := bindingNames(s); !slices.Equal(got, []string{"team-b/on-b"}) {
		t.Errorf("Bindings = %v after the label is removed; want team-b's alone", got)
	}
	if len(s.UnservedConnections) != 0 || len(s.Invalid) != 0 {
		t.Errorf("unserved %+v, Invalid %+v; want nothing: a namespace no longer admitted is not the cap's", s.UnservedConnections, s.Invalid)
	}
	if got, err := src.ConnectionBindings("team-a", "slack"); err != nil || !got.Exists || !slices.Equal(got.Bindings, []string{"team-a/on-a"}) {
		t.Errorf("ConnectionBindings(team-a/slack) = %+v, %v; want it to exist with team-a/on-a, for the broker to tell", got, err)
	}
	set(t, src)
	if n := logLines(logs, notAdmittedMsg, "namespace=team-a", "level=INFO"); n != 1 {
		t.Errorf("team-a was said no longer admitted %d times; want once, at Info\n%s", n, logs.String())
	}

	// A namespace that is deleted is admitted no more.
	until(t, func() { c.remove(namespaceObject("team-b", "1")) }, func() bool { return src.Fingerprint(t.Context()) != fp })
	if s, _ := set(t, src); len(s.ChatConnections) != 0 || len(s.Bindings) != 0 {
		t.Errorf("connections %v, Bindings %v after team-b is deleted; want none", connectionNames(s), bindingNames(s))
	}
	if n := logLines(logs, notAdmittedMsg, "namespace=team-b"); n != 1 {
		t.Errorf("team-b was said no longer admitted %d times; want once\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "agent-kourier-system") {
		t.Errorf("the log names a namespace the selector never admitted:\n%s", logs.String())
	}
}

// The fingerprint moves with what the selector admits, and with nothing else of a namespace: a write that changes no admission (a label
// the selector does not read, on a namespace that is admitted or one that is not, or a new resourceVersion alone) leaves it as it was.
// The cache keeps a namespace's name and labels and drops the rest.
func TestANamespaceUpdateThatChangesNoAdmissionKeepsTheFingerprint(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	nsA := namespaceObject("team-a", "1", admitLabel, "yes")
	nsA.SetAnnotations(map[string]string{"note": "dropped"})
	c := newCluster(t, append(objs, teamConnection(objs, "team-a", "slack", created), nsA, namespaceObject("team-c", "1"))...)
	src, _ := c.startWith(t, nil, crsource.Options{ConnectionNamespaceSelector: admitSelector})
	_, fp := set(t, src)
	if got := src.CachedNamespaceForTest("team-a"); got == nil || got.GetAnnotations() != nil || got.GetLabels()[admitLabel] != "yes" {
		t.Fatalf("the cached namespace = %+v; want its labels kept and its annotations dropped", got)
	}

	for _, tc := range []struct {
		name string
		ns   *unstructured.Unstructured
	}{
		{"another label on an admitted namespace", namespaceObject("team-a", "2", admitLabel, "yes", "owner", "a")},
		{"a resourceVersion alone", namespaceObject("team-a", "3", admitLabel, "yes", "owner", "a")},
		{"another label on a namespace that is not admitted", namespaceObject("team-c", "2", "owner", "c")},
		{"the selector's label with another value", namespaceObject("team-c", "3", admitLabel, "no")},
	} {
		until(t, func() { c.apply(tc.ns) }, func() bool {
			got := src.CachedNamespaceForTest(tc.ns.GetName())
			return got != nil && got.GetResourceVersion() == tc.ns.GetResourceVersion()
		})
		if got := src.Fingerprint(t.Context()); got != fp {
			t.Errorf("%s: the fingerprint moved", tc.name)
		}
	}
	if s, got := set(t, src); got != fp || !slices.Equal(connectionNames(s), []string{"team-a/slack"}) {
		t.Errorf("connections %v, fingerprint moved %v; want team-a's and the fingerprint as it was", connectionNames(s), got != fp)
	}
}

// The admission table: every namespace when nothing is set or the selector is empty (with an own connection too, which is served once);
// the list alone; the own connection alone. Only a selector that reads labels reads namespaces: the others need no RBAC on them.
func TestTheAdmissionRules(t *testing.T) {
	emptySelector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{})
	if err != nil {
		t.Fatal(err)
	}
	// {"matchLabels":{}} written by hand, which the chart's schema refuses and the variable may still hold: empty too, so "all".
	emptyMatchLabels, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{}, MatchExpressions: []metav1.LabelSelectorRequirement{}})
	if err != nil {
		t.Fatal(err)
	}
	every := []string{"agent-kourier-system/slack-main", "team-a/slack", "team-b/slack"}
	everyBinding := []string{"sre/chat-only", "sre/sre-alerts", "team-a/on-a", "team-b/on-b"}
	for _, tc := range []struct {
		name        string
		opts        crsource.Options
		connections []string
		bindings    []string
	}{
		{"no own connection, no list, no selector: every namespace", crsource.Options{}, every, everyBinding},
		{"no own connection, labels.Everything: every namespace", crsource.Options{ConnectionNamespaceSelector: labels.Everything()}, every, everyBinding},
		{"no own connection, an empty LabelSelector: every namespace", crsource.Options{ConnectionNamespaceSelector: emptySelector}, every, everyBinding},
		{"no own connection, empty matchLabels and matchExpressions: every namespace", crsource.Options{ConnectionNamespaceSelector: emptyMatchLabels}, every, everyBinding},
		{"an own connection and labels.Everything: the own and every namespace", crsource.Options{Connection: own, ConnectionNamespaceSelector: labels.Everything()}, every, everyBinding},
		{"an own connection, a list and labels.Everything: the own and every namespace",
			crsource.Options{Connection: own, ConnectionNamespaces: []string{"team-a"}, ConnectionNamespaceSelector: labels.Everything()}, every, everyBinding},
		{"an own connection and a list: the own and the list", crsource.Options{Connection: own, ConnectionNamespaces: []string{"team-a"}},
			[]string{"agent-kourier-system/slack-main", "team-a/slack"}, []string{"sre/chat-only", "sre/sre-alerts", "team-a/on-a"}},
		{"no own connection and a list: the list", crsource.Options{ConnectionNamespaces: []string{"team-b"}}, []string{"team-b/slack"}, []string{"team-b/on-b"}},
		{"an own connection alone: the own alone", crsource.Options{Connection: own}, []string{"agent-kourier-system/slack-main"}, []string{"sre/chat-only", "sre/sre-alerts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leakCheck(t)
			objs := corpus(t)
			extra := []*unstructured.Unstructured{
				teamConnection(objs, "team-a", "slack", created),
				teamConnection(objs, "team-b", "slack", created),
				bindingOn(objs, "team-a", "on-a", "team-a", "slack"),
				bindingOn(objs, "team-b", "on-b", "team-b", "slack"),
				namespaceObject("team-a", "1", admitLabel, "yes"),
				namespaceObject("team-b", "1", admitLabel, "yes"),
			}
			c := newCluster(t, append(objs, extra...)...)
			src, logs := c.startWith(t, nil, tc.opts)
			s, _ := set(t, src)
			if got := connectionNames(s); !slices.Equal(got, tc.connections) {
				t.Errorf("connections = %v; want %v", got, tc.connections)
			}
			if got := bindingNames(s); !slices.Equal(got, tc.bindings) {
				t.Errorf("Bindings = %v; want %v", got, tc.bindings)
			}
			if len(s.UnservedConnections) != 0 || len(s.Invalid) != 0 {
				t.Errorf("unserved %+v, Invalid %+v; want nothing", s.UnservedConnections, s.Invalid)
			}
			if n := c.namespaceRequests(); n != 0 {
				t.Errorf("%d requests of namespaces; want none: no selector reads labels", n)
			}
			if strings.Contains(logs.String(), "selector") {
				t.Errorf("the log speaks of a selector:\n%s", logs.String())
			}
		})
	}
}

// The list and the selector admit together: a namespace on the list is admitted whatever its labels, and one the selector matches is
// admitted too. A namespace on the list is not said to be admitted by the selector, whatever its labels.
func TestTheListAndTheSelectorAdmitTheirUnion(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	extra := []*unstructured.Unstructured{
		teamConnection(objs, "team-a", "slack", created),
		teamConnection(objs, "team-b", "slack", created),
		teamConnection(objs, "team-c", "slack", created),
		teamConnection(objs, "team-d", "slack", created),
		namespaceObject("team-a", "1"),                    // on the list, not labelled
		namespaceObject("team-b", "1", admitLabel, "yes"), // labelled
		namespaceObject("team-c", "1"),                    // neither
		namespaceObject("team-d", "1", admitLabel, "yes"), // both
	}
	src, logs := newCluster(t, append(objs, extra...)...).startWith(t, nil,
		crsource.Options{Connection: own, ConnectionNamespaces: []string{"team-a", "team-d"}, ConnectionNamespaceSelector: admitSelector})
	s, _ := set(t, src)
	if got, want := connectionNames(s), []string{"agent-kourier-system/slack-main", "team-a/slack", "team-b/slack", "team-d/slack"}; !slices.Equal(got, want) {
		t.Errorf("connections = %v; want %v", got, want)
	}
	if logLines(logs, admittedMsg, "namespace=team-b") != 1 || logLines(logs, admittedMsg) != 1 {
		t.Errorf("want team-b alone said admitted by the selector:\n%s", logs.String())
	}
}

// On "all" the cap is cluster-wide: every namespace's connections are ordered by age, then namespace and name, and the youngest past the
// cap is left out. The own connection, when there is one, takes the first place and is never a candidate: it is not counted twice and
// never left out.
func TestTheCapIsClusterWideOnAll(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts crsource.Options
	}{
		{"no own connection", crsource.Options{}},
		{"an own connection and labels.Everything", crsource.Options{Connection: own, ConnectionNamespaceSelector: labels.Everything()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leakCheck(t)
			objs := corpus(t) // slack-main has no creationTimestamp: it is the oldest
			var extra []*unstructured.Unstructured
			for i := 1; i <= crsource.MaxServedConnections; i++ { // ns01 is the oldest, ns16 the youngest
				ns := fmt.Sprintf("ns%02d", i)
				extra = append(extra, teamConnection(objs, ns, "slack", created.Add(time.Duration(i)*time.Hour)))
			}
			src, _ := newCluster(t, append(objs, extra...)...).startWith(t, nil, tc.opts)
			s, _ := set(t, src)
			got := connectionNames(s)
			if len(got) != crsource.MaxServedConnections || slices.Contains(got, "ns16/slack") || !slices.Contains(got, "ns15/slack") {
				t.Errorf("connections = %v; want %d: slack-main and ns01 to ns15", got, crsource.MaxServedConnections)
			}
			if n := slices.Index(got, "agent-kourier-system/slack-main"); n < 0 || slices.Contains(got[n+1:], got[n]) {
				t.Errorf("connections = %v; want slack-main once", got)
			}
			if got := unservedNames(s.UnservedConnections); !slices.Equal(got, []string{"ns16/slack cap"}) {
				t.Errorf("unserved connections = %v; want ns16, for the cap", got)
			}
		})
	}
}

// A connection whose deletion is deferred is served, and keeps its place under the cap, only while its namespace is admitted: one of a
// namespace that is no longer admitted is not a candidate, so it is not served and the place is the next connection's at once.
func TestADeferredDeletionInANamespaceNoLongerAdmittedTakesNoPlace(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	old := teamConnection(objs, "old-team", "slack", created.Add(-time.Hour))
	extra := []*unstructured.Unstructured{old, namespaceObject("old-team", "1", admitLabel, "yes"), namespaceObject("teams", "1", admitLabel, "yes")}
	for i := 1; i <= crsource.MaxServedConnections; i++ { // c16 is the youngest, and is left out
		extra = append(extra, teamConnection(objs, "teams", fmt.Sprintf("c%02d", i), created.Add(time.Duration(i)*time.Hour)))
	}
	c := newCluster(t, append(objs, extra...)...)
	clock := &testClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	src, _ := c.startWith(t, clock, crsource.Options{ConnectionNamespaceSelector: admitSelector})
	if s, _ := read(t, src); !slices.Contains(connectionNames(s), "old-team/slack") || !slices.Equal(unservedNames(s.UnservedConnections), []string{"teams/c16 cap"}) {
		t.Fatalf("connections %v, unserved %v; want old-team's served and c16 left out", connectionNames(s), unservedNames(s.UnservedConnections))
	}

	until(t, func() { c.remove(old); c.apply(namespaceObject("old-team", "2")) }, func() bool {
		ns := src.CachedNamespaceForTest("old-team")
		return src.CachedGenerationForTest("ChatConnection", "old-team", "slack") < 0 && ns != nil && ns.GetLabels() == nil
	})
	s, _ := read(t, src) // the deletion is deferred, but old-team is no longer admitted
	if got := connectionNames(s); slices.Contains(got, "old-team/slack") || !slices.Contains(got, "teams/c16") || len(s.UnservedConnections) != 0 {
		t.Errorf("connections %v, unserved %v; want old-team's not served and c16 in its place", got, unservedNames(s.UnservedConnections))
	}
}

// A selector needs list and watch on namespaces, and Start says so when either is denied, as it does for the three kinds.
func TestStartWithoutTheRightToReadNamespaces(t *testing.T) {
	forbidden := apierrors.NewForbidden(namespaces.GroupResource(), "", errors.New("no"))
	for name, deny := range map[string]func(*cluster){
		"list": func(c *cluster) {
			c.client.PrependReactor("list", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, forbidden })
		},
		"watch": func(c *cluster) {
			c.client.PrependWatchReactor("namespaces", func(clienttesting.Action) (bool, watch.Interface, error) { return true, nil, forbidden })
		},
	} {
		t.Run(name, func(t *testing.T) {
			leakCheck(t)
			c := newCluster(t)
			deny(c)
			src, err := crsource.New(crsource.Options{Client: c.client, ConnectionNamespaceSelector: admitSelector})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(src.Stop)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second) // without the probe, Start would wait out its sync
			defer cancel()
			err = src.Start(ctx)
			if !errors.Is(err, crsource.ErrAccessDenied) || !strings.Contains(err.Error(), "namespaces") || errors.Is(err, crsource.ErrCRDsAbsent) {
				t.Errorf("Start = %v; want ErrAccessDenied naming namespaces", err)
			}
		})
	}
}

// While the API server cannot be reached the namespace cache keeps its last state, as the caches of the three kinds do: an outage is
// not every label removed. The probe lists namespaces too, so Stale says so, naming them.
func TestAStaleNamespaceCacheKeepsTheLastAdmittedSet(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, append(objs, teamConnection(objs, "team-a", "slack", created), namespaceObject("team-a", "1", admitLabel, "yes"))...)
	var mu sync.Mutex
	down := false
	refused := errors.New(`Get "https://10.96.0.1:443/api/v1/namespaces?limit=1": dial tcp 10.96.0.1:443: connect: connection refused`)
	c.client.PrependReactor("list", "namespaces", func(clienttesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return true, nil, refused
		}
		return false, nil, nil
	})
	logs := &logSink{}
	src, err := crsource.New(crsource.Options{Client: c.client, ConnectionNamespaceSelector: admitSelector, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	src.SetProbeForTest(20*time.Millisecond, time.Second)
	t.Cleanup(src.Stop)
	if err := src.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, fp := set(t, src)
	mu.Lock()
	down = true
	mu.Unlock()
	until(t, func() {}, func() bool { return src.Stale() != nil })
	if err := src.Stale(); !strings.Contains(err.Error(), "namespaces") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Stale = %v; want it to name namespaces and say connection refused", err)
	}
	if s, got := set(t, src); !slices.Equal(connectionNames(s), []string{"team-a/slack"}) || got != fp {
		t.Errorf("connections %v during the outage, fingerprint moved %v; want team-a's kept", connectionNames(s), got != fp)
	}
	if logLines(logs, notAdmittedMsg) != 0 {
		t.Errorf("the outage was taken for a label removed:\n%s", logs.String())
	}
	mu.Lock()
	down = false
	mu.Unlock()
	until(t, func() {}, func() bool { return src.Stale() == nil })
}

// The admitted set is read once per listing, so a Set and its fingerprint agree: the namespaces the fingerprint names are those whose
// connections the Set serves. A read with the label and a read without it never share a fingerprint.
func TestASetAndItsFingerprintAgreeOnTheAdmittedNamespaces(t *testing.T) {
	leakCheck(t)
	objs := corpus(t)
	c := newCluster(t, append(objs, teamConnection(objs, "team-a", "slack", created), namespaceObject("team-a", "1"))...)
	src, _ := c.startWith(t, nil, crsource.Options{ConnectionNamespaceSelector: admitSelector})
	seen := map[string][]string{} // fingerprint: the connections of a Set read with it
	record := func() {
		s, fp := set(t, src)
		got := connectionNames(s)
		if prev, ok := seen[fp]; ok && !slices.Equal(prev, got) {
			t.Fatalf("one fingerprint for two Sets: %v and %v", prev, got)
		}
		seen[fp] = got
	}
	for i := range 6 {
		labelsOf := []string{}
		if i%2 == 0 {
			labelsOf = []string{admitLabel, "yes"}
		}
		ns := namespaceObject("team-a", fmt.Sprint(i+2), labelsOf...)
		until(t, func() { c.apply(ns) }, func() bool {
			got := src.CachedNamespaceForTest("team-a")
			return got != nil && got.GetResourceVersion() == ns.GetResourceVersion()
		})
		record()
	}
	if len(seen) != 2 {
		t.Errorf("fingerprints %v; want two, one with team-a admitted and one without", slices.Collect(maps.Keys(seen)))
	}
}
