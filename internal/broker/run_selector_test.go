package broker

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// "All" (spec D54) is served through Run: a broker with no own connection, no list and no selector, and one with an own connection and an
// empty selector, serve the ChatConnection of a namespace that nothing names, beside the example config's connection (own or not), and read
// no namespace to do it, so they need no right on namespaces.
func TestARunWithNothingSetOrAnEmptySelectorServesAConnectionNobodyNamed(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		own      bool
		selector labels.Selector
	}{
		"nothing set": {own: false, selector: nil},
		"an own connection and an empty selector": {own: true, selector: labels.Everything()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newConnWorld(t, nil)
			runsAlongside(&w.c)
			if !tc.own {
				w.c.ChatConnection = v1alpha1.ObjectRef{}
			}
			w.c.ConnectionNamespaces, w.c.ConnectionNamespaceSelector = nil, tc.selector
			w.team("team-c", "tok-c")
			r := start(t, w.c)
			r.until("/readyz", http.StatusOK)

			w.eventually("the connection of a namespace nobody named, and the example config's, to run", func() bool {
				return w.up("tok-c") == 1 && w.up(ownToken) == 1
			})
			r.waitMetric(connected("team-c/slack", 1))
			for _, a := range w.dyn.Actions() {
				if a.GetResource() == crNamespaces {
					t.Fatalf("the broker read namespaces (%s) to admit every one; want it to read none", a.GetVerb())
				}
			}
			r.cancel()
			if err := r.wait(); err != nil {
				t.Errorf("Run = %v after a signal; want nil", err)
			}
		})
	}
}

// namespace puts the Namespace name in the cluster of w, with the labels given, or replaces it.
func (w *connWorld) namespace(name string, l map[string]string) {
	w.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("Namespace")
	u.SetName(name)
	u.SetUID(types.UID("uid-ns-" + name))
	u.SetLabels(l)
	err := w.dyn.Tracker().Update(crNamespaces, u, "")
	if err != nil {
		err = w.dyn.Tracker().Create(crNamespaces, u, "")
	}
	if err != nil {
		w.t.Fatal(err)
	}
}

// A namespace that the selector admits while the broker runs and that then loses its label is no longer admitted (spec D54), with no restart:
// its connection stops, and its Binding is told that the connection is not served, with the reason of a connection this broker served before
// (UnservedStopped), because the record of the connections served, in the store, still holds it. The record keeps it after the shutdown, so a
// later start tells its Bindings too. Put back, the label admits the namespace again, and the Binding is served again.
func TestARunTellsTheBindingsOfANamespaceTheSelectorStopsAdmitting(t *testing.T) {
	t.Parallel()
	w := newConnWorld(t, nil)
	runsAlongside(&w.c)
	const label = "kourier-admitted"
	w.c.ConnectionNamespaces, w.c.ConnectionNamespaceSelector = nil, labels.SelectorFromSet(labels.Set{label: "yes"})
	cluster := newStatusClusterOn(w.dyn)
	w.c.StatusClient = cluster.dyn
	w.c.probeEvery, w.c.probeTick, w.c.probeGap = 50*time.Millisecond, 10*time.Millisecond, time.Millisecond
	admitted := map[string]string{label: "yes"}
	w.namespace("team-a", admitted)
	w.namespace("team-b", nil)
	w.team("team-a", "tok-a")
	w.binding("team-a", "on-a", "C0TEAMA0001")
	w.team("team-b", "tok-b")
	w.binding("team-b", "on-b", "C0TEAMB0001")
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)

	w.eventually("the admitted namespace's connection to run", func() bool { return w.up("tok-a") == 1 && w.up(ownToken) == 1 })
	w.eventually("its Binding to be joined", func() bool { return cluster.stored(t, "team-a/on-a")["ChannelJoined"].Status == "True" })
	if w.up("tok-b") != 0 {
		t.Errorf("the connection of a namespace without the label runs %d times; want it not to", w.up("tok-b"))
	}

	w.namespace("team-a", map[string]string{label: "no"})
	w.eventually("the connection to stop and its Binding to be told it is not served", func() bool {
		a := cluster.stored(t, "team-a/on-a")["Accepted"]
		return w.up("tok-a") == 0 && a.Status == "False" && a.Reason == "ConnectionNotServed" && strings.Contains(a.Message, stoppedMessage)
	})
	if rd := cluster.stored(t, "team-a/on-a")["Ready"]; rd.Status != "False" || rd.Reason != "Accepted:ConnectionNotServed" {
		t.Errorf("Ready = %+v; want False, Accepted:ConnectionNotServed", rd)
	}
	select {
	case err := <-r.done:
		t.Fatalf("Run returned %v; want the change taken with no restart", err)
	default:
	}
	if w.up(ownToken) != 1 {
		t.Errorf("the own connection is up %d times; want it to run on", w.up(ownToken))
	}
	if b := cluster.stored(t, "team-b/on-b"); len(b) != 0 {
		t.Errorf("the Binding of a namespace that was never admitted has the status %v; want none from this broker", b)
	}

	w.namespace("team-a", admitted)
	w.eventually("the connection to run again and its Binding to be served", func() bool {
		return w.up("tok-a") == 1 && cluster.stored(t, "team-a/on-a")["Accepted"].Status == "True"
	})

	w.namespace("team-a", nil)
	w.eventually("the Binding to be told again", func() bool {
		return w.up("tok-a") == 0 && cluster.stored(t, "team-a/on-a")["Accepted"].Reason == "ConnectionNotServed"
	})
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v after a signal; want nil", err)
	}
	st, err := sqlite.Open(t.Context(), w.c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if got, err := st.ServedConnections(t.Context()); err != nil || !slices.Contains(got, "team-a/slack") || slices.Contains(got, "team-b/slack") {
		t.Errorf("the record of the served connections = %v, %v; want team-a/slack kept, which still exists, and never team-b/slack", got, err)
	}
}
