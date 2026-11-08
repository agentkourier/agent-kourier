package broker

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// The Bindings of a connection that the broker holds in its Snapshot and does not run, because it is a second connection of a Slack app, are
// told so: Accepted is False with the reason ConnectionNotServed, and Ready follows it, while the Bindings of the connection that is served are
// as they were. The overlay of the report is all the writer is given for it; when the refusal lifts the Binding is accepted again.
func TestTheBindingsOfARefusedConnectionAreToldItIsNotServed(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1, servedStatus(1)...), storedBinding(teamBind, 1, servedStatus(1)...))
	refused := &refusals{}
	rig := newStatusRigWith(t, c, nil, func(cfg *statusConfig) {
		raw := cfg.Report
		cfg.Report = func() config.Report { return refused.overlay(raw()) }
	})
	refused.changed = rig.w.notify
	sr := &servedRig{statusRig: rig, record: &recordedServed{}}
	sr.serving(t, bothServed())
	sr.reconcile()
	if a := c.stored(t, teamBind)["Accepted"]; a.Status != metav1.ConditionTrue {
		t.Fatalf("before the refusal the team Binding says Accepted %+v; want True", a)
	}

	refused.set(map[string]string{teamConn: refusedMessage})
	sr.pass()

	a, ready := c.stored(t, teamBind)["Accepted"], c.stored(t, teamBind)["Ready"]
	if a.Status != metav1.ConditionFalse || a.Reason != "ConnectionNotServed" || !strings.Contains(a.Message, "team-a/slack is not served by this broker") ||
		!strings.Contains(a.Message, "an app is served once") {
		t.Errorf("the refused connection's Binding says Accepted %+v; want False, ConnectionNotServed and why", a)
	}
	if ready.Status != metav1.ConditionFalse || ready.Reason != "Accepted:ConnectionNotServed" {
		t.Errorf("Ready = %+v; want False with the reason of Accepted", ready)
	}
	if strings.Contains(a.Message, "sre/slack") {
		t.Errorf("the message names the other connection: %q", a.Message)
	}
	if own := c.stored(t, sreAlerts)["Accepted"]; own.Status != metav1.ConditionTrue {
		t.Errorf("the served connection's Binding says Accepted %+v; want True", own)
	}

	// The Binding is not served here, so it has none of the probes of a served one, and what it says is written once: the broker that
	// does serve the connection has the last word, and is not fought.
	var told statusApply
	for _, ap := range c.snapshotApplies() {
		if ap.Name == "on-a" {
			told = ap
		}
	}
	for _, probed := range []string{"BackendReachable", "ChannelJoined"} {
		if _, has := told.conds()[probed]; has {
			t.Errorf("the status of a refused connection's Binding has %s, which only a served Binding has: %v", probed, told.conds())
		}
	}
	writes := appliesTo(c, teamBind)
	if err := c.update(storedBinding(teamBind, 1, servedStatus(1)...), "team-a"); err != nil { // another broker writes its own words
		t.Fatal(err)
	}
	for range 5 {
		sr.advance(time.Minute)
		sr.reconcile()
	}
	if appliesTo(c, teamBind) != writes || c.stored(t, teamBind)["Ready"].Status != metav1.ConditionTrue {
		t.Errorf("writes %d (was %d), Ready %+v; want the other broker's words left alone: once, and not put back", appliesTo(c, teamBind), writes, c.stored(t, teamBind)["Ready"])
	}

	refused.set(nil)
	sr.pass()
	if a := c.stored(t, teamBind)["Accepted"]; a.Status != metav1.ConditionTrue {
		t.Errorf("after the refusal lifted the Binding says Accepted %+v; want True", a)
	}
}

// The overlay changes nothing with no refusal, and does not change the report it is given.
func TestTheOverlayLeavesTheReportAsItIs(t *testing.T) {
	rep := config.Report{Snapshot: servedSnapshot(t, bothServed())}
	refused := &refusals{}
	if got := refused.overlay(rep); len(got.Rejections) != 0 || got.Snapshot != rep.Snapshot {
		t.Errorf("overlay with nothing refused = %+v; want the report", got)
	}
	refused.set(map[string]string{teamConn: refusedMessage})
	got := refused.overlay(rep)
	if len(got.Rejections) != 1 || got.Rejections[0].Namespace != "team-a" || got.Rejections[0].Name != "on-a" || got.Rejections[0].Served != 1 {
		t.Errorf("overlay = %+v; want one rejection of the team Binding, served at its generation", got.Rejections)
	}
	if len(rep.Rejections) != 0 {
		t.Error("overlay changed the report it was given")
	}
}
