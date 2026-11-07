package broker

import (
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
)

// serveGroups makes the source serve the Bindings at the generations given, each asking for tool approvals from the groups given.
func (r *statusRig) serveGroups(generations map[string]int64, groups map[string][]string) {
	r.mu.Lock()
	r.report.Snapshot = snapshotWith(r.t, generations, nil, groups)
	r.mu.Unlock()
}

// edit makes the cluster hold the Binding key at a newer generation, as an edit of its spec does.
func (c *statusCluster) edit(t *testing.T, key string, generation int64) {
	t.Helper()
	ns, name, _ := strings.Cut(key, "/")
	obj, err := c.dyn.Tracker().Get(crBindings, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	u := obj.(*unstructured.Unstructured).DeepCopy()
	u.SetGeneration(generation)
	if err := c.update(u, ns); err != nil {
		t.Fatal(err)
	}
}

var statusGroups = map[string][]string{sreAlerts: {"S0AAAAAAAAA"}}

// groupsProbe is probe(generation) with the ApproversReadable the Prober holds for a Binding that names groups: the status of it, and a
// Ready that the Prober derived with it.
func groupsProbe(generation int64, status bindingcheck.Status, reason bindingcheck.Reason, message string) []bindingcheck.Condition {
	conds := probe(generation)
	ap := bindingcheck.Condition{Type: bindingcheck.ApproversReadable, Status: status, Reason: reason, Message: message, ObservedGeneration: generation, LastTransitionTime: statusT0.Add(time.Minute)}
	if status != bindingcheck.True {
		conds[len(conds)-1].Status = status
		conds[len(conds)-1].Reason = bindingcheck.Reason(string(bindingcheck.ApproversReadable) + ":" + string(reason))
		conds[len(conds)-1].Message = message
	}
	return append([]bindingcheck.Condition{ap}, conds...)
}

// A Binding that asks for approvals from groups has ApproversReadable in its status, as the Prober found it, and it counts toward the
// Ready the Prober derived.
func TestABindingWithApproverGroupsHasApproversReadableInItsStatus(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, nil)
	r.serveGroups(map[string]int64{sreAlerts: 1}, statusGroups)
	r.set(sreAlerts, groupsProbe(1, bindingcheck.False, bindingcheck.ReasonGroupNotFound, "approver group spec.interactions.approverGroups[0] (S0AAAAAAAAA) does not exist")...)
	r.reconcile()

	got := c.last(t).conds()
	ap := got["ApproversReadable"]
	if ap["status"] != "False" || ap["reason"] != "GroupNotFound" || ap["observedGeneration"] != int64(1) || ap["message"] == "" {
		t.Errorf("ApproversReadable = %v; want False/GroupNotFound at generation 1, with the Prober's message", ap)
	}
	if got["Ready"]["status"] != "False" || got["Ready"]["reason"] != "ApproversReadable:GroupNotFound" {
		t.Errorf("Ready = %v; want False/ApproversReadable:GroupNotFound", got["Ready"])
	}
}

// Until the Prober has listed the Binding, a Binding that names groups has ApproversReadable Pending, like its other probed conditions,
// and not missing; one that names none has no such condition.
func TestApproversReadableIsPendingBeforeTheProberHasListedTheBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		groups map[string][]string
		want   []string
	}{
		{"with groups", statusGroups, []string{"Accepted", "ApproversReadable", "BackendReachable", "ChannelJoined", "Ready"}},
		{"without", nil, []string{"Accepted", "BackendReachable", "ChannelJoined", "Ready"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newStatusCluster(t, storedBinding(sreAlerts, 1))
			r := newStatusRig(t, c, nil)
			r.serveGroups(map[string]int64{sreAlerts: 1}, tc.groups)
			r.reconcile() // the Prober has found nothing yet
			got := c.last(t).conds()
			var types []string
			for typ := range got {
				types = append(types, typ)
			}
			slices.Sort(types)
			if !slices.Equal(types, tc.want) {
				t.Fatalf("condition types = %v, want %v", types, tc.want)
			}
			for _, typ := range tc.want[1:4] {
				if got[typ]["status"] != "Unknown" || got[typ]["reason"] != "Pending" {
					t.Errorf("%s = %v; want Unknown/Pending", typ, got[typ])
				}
			}
		})
	}
}

// The Prober picks up an edit at its next Tick, and the writer sees it first: a Binding that has gained groups is not Ready on the word
// of probes that did not ask about them. ApproversReadable is Pending and Ready follows it, unless Ready is already False.
func TestABindingThatJustGainedGroupsIsPendingOnThemUntilTheProberHasThem(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 2))
	r := newStatusRig(t, c, nil)
	r.serveGroups(map[string]int64{sreAlerts: 2}, statusGroups)
	r.set(sreAlerts, probe(2)...) // the Prober's, with no ApproversReadable: it has not yet seen the edit
	r.reconcile()
	got := c.last(t).conds()
	if ap := got["ApproversReadable"]; ap["status"] != "Unknown" || ap["reason"] != "Pending" || ap["observedGeneration"] != int64(2) {
		t.Errorf("ApproversReadable = %v; want Unknown/Pending at generation 2", ap)
	}
	if got["Ready"]["status"] != "Unknown" || got["Ready"]["reason"] != "ApproversReadable:Pending" {
		t.Errorf("Ready = %v; want Unknown/ApproversReadable:Pending, since a probe that did not ask about the groups cannot call the Binding ready", got["Ready"])
	}

	// A Ready that is False for another reason stays so: the Binding is known not to be ready.
	failing := probe(2)
	failing[0].Status, failing[0].Reason = bindingcheck.False, bindingcheck.ReasonAgentNotFound
	failing[2].Status, failing[2].Reason = bindingcheck.False, "BackendReachable:AgentNotFound"
	r.set(sreAlerts, failing...)
	r.advance(11 * time.Second)
	r.reconcile()
	if got := c.last(t).conds(); got["Ready"]["status"] != "False" || got["Ready"]["reason"] != "BackendReachable:AgentNotFound" {
		t.Errorf("Ready = %v; want False/BackendReachable:AgentNotFound", got["Ready"])
	}
}

// A Binding that names no groups has no ApproversReadable in its status, though the Prober may still hold one for the instant between
// the writer seeing the edit and the Prober seeing it; and the edit that takes the groups away writes a status without it, which Server-Side
// Apply takes to remove the condition the writer wrote.
func TestABindingThatLosesItsGroupsLosesTheCondition(t *testing.T) {
	c := newStatusCluster(t, storedBinding(sreAlerts, 1))
	r := newStatusRig(t, c, nil)
	r.serveGroups(map[string]int64{sreAlerts: 1}, statusGroups)
	r.set(sreAlerts, groupsProbe(1, bindingcheck.True, bindingcheck.ReasonOK, "")...)
	r.reconcile()
	if _, has := c.last(t).conds()["ApproversReadable"]; !has {
		t.Fatalf("no ApproversReadable written while the Binding names groups: %v", c.last(t).conds())
	}
	if _, has := c.stored(t, sreAlerts)["ApproversReadable"]; !has {
		t.Fatal("the cluster holds no ApproversReadable")
	}

	c.edit(t, sreAlerts, 2) // the edit that takes the groups away
	r.serveGroups(map[string]int64{sreAlerts: 2}, nil)
	r.set(sreAlerts, groupsProbe(1, bindingcheck.True, bindingcheck.ReasonOK, "")...) // the Prober still holds it
	r.advance(11 * time.Second)
	r.reconcile()
	applied := c.last(t).conds()
	if _, has := applied["ApproversReadable"]; has {
		t.Errorf("the status written after the groups went still says %v", applied["ApproversReadable"])
	}
	if _, has := c.stored(t, sreAlerts)["ApproversReadable"]; has {
		t.Error("the cluster still holds ApproversReadable after the write")
	}
}
