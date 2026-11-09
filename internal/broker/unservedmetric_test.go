package broker

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// notServed is the series of agentkourier_config_binding_rejections that counts the Bindings whose status says ConnectionNotServed.
const notServed = "{reason=ConnectionNotServed}"

// notServedLine returns the value of the ConnectionNotServed series as the line "{reason=ConnectionNotServed} N".
func notServedLine(t *testing.T, m *Metrics) string {
	t.Helper()
	for _, g := range gauges(t, m, "agentkourier_config_binding_rejections") {
		if strings.HasPrefix(g, notServed) {
			return g
		}
	}
	t.Fatalf("no ConnectionNotServed series among %v", gauges(t, m, "agentkourier_config_binding_rejections"))
	return ""
}

// wantCount fails unless the series says n (0, 1 or 2).
func wantCount(t *testing.T, m *Metrics, when string, n string) {
	t.Helper()
	if got, want := notServedLine(t, m), notServed+" "+n; got != want {
		t.Errorf("%s: %s; want %s", when, got, want)
	}
}

// The status writer composes ConnectionNotServed itself, so the series counts what the writer says and not what the source's report holds: the
// Bindings of a connection past the cap, and then of one taken off the list, are counted, a Binding the connection does not admit (the writer
// tells it nothing) is not, and the count returns to 0 when the connection is served.
func TestTheBindingsOfAnUnservedConnectionAreCounted(t *testing.T) {
	over := "teams/on-c16"
	const outsider = "other/outsider"
	m := mustMetrics(t, nil)
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(over, 3), storedBinding(outsider, 1))
	r := newStatusRigWith(t, c, map[string]int64{sreAlerts: 1}, func(cfg *statusConfig) { cfg.OnUnserved = m.UnservedBindings })
	r.set(sreAlerts, probe(1)...)
	wantCount(t, m, "before any pass", "0")
	r.reconcile()
	wantCount(t, m, "nothing is left out", "0")

	r.mu.Lock()
	r.report.UnservedConnections = []config.UnservedConnection{{Namespace: "teams", Name: "c16", Reason: config.UnservedCap,
		Message: "the broker serves at most 16 chat connections", Bindings: []string{over, outsider}, Admitted: []string{over}}}
	r.mu.Unlock()
	r.advance(11 * time.Second)
	r.reconcile()
	wantCount(t, m, "a connection past the cap, with one Binding it admits and one it does not", "1")

	r.mu.Lock()
	r.report.UnservedConnections = nil
	r.mu.Unlock()
	r.advance(11 * time.Second)
	r.reconcile()
	wantCount(t, m, "the connection is served", "0")
}

// A connection taken off the admission list is found by the record of the store, not by the report, and its Bindings are counted all the same;
// served again, they are not. The other series of the metric are left as the report set them.
func TestTheBindingsOfAConnectionTakenOffTheListAreCounted(t *testing.T) {
	m := mustMetrics(t, nil)
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1))
	record := &recordedServed{set: []string{ownConn, teamConn}} // what the broker before it recorded
	r := newServedRig(t, c, ownServed(), record, bothExist())
	r.w.cfg.OnUnserved = m.UnservedBindings
	m.ConfigReport(config.Report{Rejections: []config.Rejection{{Reason: config.ReasonChannelConflict}}})

	r.pass()
	wantCount(t, m, "the team connection is off the list", "1")
	if got := gauges(t, m, "agentkourier_config_binding_rejections"); !slices.Contains(got, "{reason=ChannelConflict} 1") {
		t.Errorf("rejections = %v; want ChannelConflict left at what the report said", got)
	}
	m.ConfigReport(config.Report{})
	wantCount(t, m, "a report of the source comes", "1")

	r.serving(t, bothServed())
	r.pass()
	wantCount(t, m, "the namespace is on the list again", "0")
}

// The second connection of one Slack app is in the Snapshot, so its Bindings are served as far as the source knows; the overlay of the
// refusals makes them ConnectionNotServed for the writer, and the count follows it.
func TestTheBindingsOfARefusedSecondConnectionAreCounted(t *testing.T) {
	m := mustMetrics(t, nil)
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1))
	refused := &refusals{}
	rig := newStatusRigWith(t, c, nil, func(cfg *statusConfig) {
		raw := cfg.Report
		cfg.Report = func() config.Report { return refused.overlay(raw()) }
		cfg.OnUnserved = m.UnservedBindings
	})
	refused.changed = rig.w.notify
	sr := &servedRig{statusRig: rig, record: &recordedServed{}}
	sr.serving(t, bothServed())
	sr.reconcile()
	wantCount(t, m, "both connections are served", "0")

	refused.set(map[string]string{teamConn: refusedMessage})
	sr.pass()
	wantCount(t, m, "the team connection is a second connection of an app", "1")

	refused.set(nil)
	sr.pass()
	wantCount(t, m, "the refusal lifted", "0")
}

// A Binding that a more specific rejection explains says that reason and not ConnectionNotServed, and is counted under the first only, as its status is.
func TestABindingWithAMoreSpecificRejectionIsNotCountedAsUnserved(t *testing.T) {
	m := mustMetrics(t, nil)
	c := newStatusCluster(t, storedBinding(sreAlerts, 1), storedBinding(teamBind, 1))
	record := &recordedServed{set: []string{ownConn, teamConn}}
	r := newServedRig(t, c, ownServed(), record, bothExist())
	r.w.cfg.OnUnserved = m.UnservedBindings
	r.reject(config.Rejection{Namespace: "team-a", Name: "on-a", Reason: config.ReasonConnectionRejected, Generation: 1, Message: "spec.chat.connectionRef: ChatConnection team-a/slack generation 1 is invalid"})
	r.reconcile()
	wantCount(t, m, "the Binding says ConnectionRejected", "0")
}

// The series is there from the start with the other reasons', is not set by a report of a source (a report holds no such reason), and on the
// files route, which has no status writer, never moves from 0.
func TestTheNotServedSeriesStartsAtZeroAndIsNotTheReportsToSet(t *testing.T) {
	m := mustMetrics(t, nil)
	wantCount(t, m, "at start", "0")
	m.UnservedBindings(2)
	m.ConfigReport(config.Report{})
	wantCount(t, m, "after a report", "2")
	m.UnservedBindings(0)
	wantCount(t, m, "when nothing is left out", "0")
}
