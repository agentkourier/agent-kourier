//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
)

// The scenarios of a Binding's status, which the leader writes (the status writer): what `kubectl get akbinding` shows in its READY and REASON columns,
// and what .status.conditions holds. A status is written after a probe pass, which a change of config calls for at most every 30 seconds, and by a
// queue that waits at least 10 between writes of one Binding, so they wait minutes, not seconds.
const statusWithin = 4 * time.Minute

// condition returns the status condition of that type, or false.
func condition(obj *unstructured.Unstructured, typ string) (map[string]any, bool) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == typ {
			return m, true
		}
	}
	return nil, false
}

// waitRow waits until the row of the Binding in `kubectl get akbinding -n e2e-team` has every cell in want, and returns the row.
func (c *crScenario) waitRow(name string, want map[string]string) map[string]string {
	c.t.Helper()
	var last map[string]string
	err := Wait(c.ctx, statusWithin, fmt.Sprintf("the row of %s to show %v", name, want), func(ctx context.Context) (bool, error) {
		tab, err := c.crs.Table(ctx, kube.BindingGVR, kube.TeamNamespace)
		if err != nil {
			return false, err
		}
		row, ok := tab.Row(name)
		last = row
		if !ok {
			return false, nil
		}
		for col, v := range want {
			if row[col] != v {
				return false, fmt.Errorf("row %v", row)
			}
		}
		return true, nil
	})
	if err != nil {
		c.t.Fatalf("%v\nthe last row: %v", err, last)
	}
	return last
}

// TestCR_StatusShowsInKubectlGet: a Binding the broker can serve shows READY True and REASON OK in the table `kubectl get akbinding` prints (the printer
// columns of the CRD, read here as the API server's table), a Binding whose channel the bot has not joined shows READY False with the condition that
// holds it back, and one that loses its channel to an older Binding shows READY False for that. .status.conditions has the four conditions of a ready
// Binding, each at the generation the broker has seen, and an edit that changes no condition moves observedGeneration: the generation is not stuck.
func TestCR_StatusShowsInKubectlGet(t *testing.T) {
	c := newCRScenario(t)
	for channel, members := range map[string][]string{ChannelCRStatusJoined: {fakeBot}, ChannelCRStatus: {"UCROTHER"}} {
		if err := c.slack.SetMembers(c.ctx, channel, members...); err != nil {
			t.Fatal(err)
		}
	}
	tm := c.newTeam("status")
	tm.addBinding("status-ready", ChannelCRStatusJoined, crAgentAnswer)
	tm.addBinding("status-notjoined", ChannelCRStatus, crAgentAnswer)

	row := c.waitRow("status-ready", map[string]string{"Ready": "True", "Reason": "OK"})
	if row["Channel"] != ChannelCRStatusJoined {
		t.Errorf("the Channel column of status-ready is %q, want %q", row["Channel"], ChannelCRStatusJoined)
	}
	c.waitRow("status-notjoined", map[string]string{"Ready": "False", "Reason": "ChannelJoined:NotInChannel"})

	ready, err := c.crs.Get(c.ctx, kube.BindingGVR, kube.TeamNamespace, "status-ready")
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"Ready", "Accepted", "BackendReachable", "ChannelJoined"} {
		cond, ok := condition(ready, typ)
		switch {
		case !ok:
			t.Errorf("status-ready has no %s condition; its conditions: %v", typ, ready.Object["status"])
		case cond["status"] != "True":
			t.Errorf("status-ready %s is %v (%v), want True", typ, cond["status"], cond["reason"])
		case cond["observedGeneration"] != ready.GetGeneration():
			t.Errorf("status-ready %s observed generation %v, want %d", typ, cond["observedGeneration"], ready.GetGeneration())
		}
	}

	// An edit that moves no condition: the output of the Binding. Its generation moves, and so must observedGeneration.
	spec := tm.bindingSpec(ChannelCRStatusJoined, crAgentAnswer)
	spec["chat"].(map[string]any)["output"] = "final"
	edited := tm.mustRewrite("status-ready", spec)
	err = Wait(c.ctx, statusWithin, fmt.Sprintf("status-ready's conditions to observe generation %d", edited.GetGeneration()), func(ctx context.Context) (bool, error) {
		obj, err := c.crs.Get(ctx, kube.BindingGVR, kube.TeamNamespace, "status-ready")
		if err != nil {
			return false, err
		}
		cond, ok := condition(obj, "Ready")
		return ok && cond["observedGeneration"] == edited.GetGeneration() && cond["status"] == "True", nil
	})
	if err != nil {
		t.Error(err)
	}

	// A younger Binding on the channel of status-ready is not served, and says so.
	time.Sleep(3 * time.Second) // creationTimestamp has a resolution of a second: the younger must be younger by it, or the names decide
	tm.addBinding("status-conflict", ChannelCRStatusJoined, crAgentInvest)
	c.waitRow("status-conflict", map[string]string{"Ready": "False", "Reason": "Accepted:ChannelConflict"})
	loser, err := c.crs.Get(c.ctx, kube.BindingGVR, kube.TeamNamespace, "status-conflict")
	if err != nil {
		t.Fatal(err)
	}
	if cond, ok := condition(loser, "Accepted"); !ok || cond["status"] != "False" || cond["reason"] != "ChannelConflict" || !strings.Contains(fmt.Sprint(cond["message"]), "status-ready") {
		t.Errorf("status-conflict's Accepted condition is %v, want False, ChannelConflict, and a message that names the Binding that holds the channel", cond)
	}
}

// TestCR_FailoverDoesNotBlankStatus: a Binding that is Ready keeps its status through a failover. A new leader starts every condition Pending, since it
// has not probed yet, and writing that would blank a good status at every failover; it writes nothing for a Binding it has not probed, so Ready stays
// True, with the lastTransitionTime it had, through the takeover and after the new leader's first probe pass.
//
// The fast tier on Postgres with two replicas, on custom resources only (E2E_STORE=postgres E2E_REPLICAS=2 E2E_CONFIG_SOURCE=crds make e2e-fast-up).
func TestCR_FailoverDoesNotBlankStatus(t *testing.T) {
	if env := NewEnv(os.Getenv); env.ConfigSource != "crds" {
		t.Skip("Agent Kourier reads its config from the ConfigMap here; no status is written. Bring the tier up with: E2E_CONFIG_SOURCE=crds E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up")
	}
	f := newFailover(t)
	crs, err := kube.NewCRsInCluster()
	if err != nil {
		t.Fatal(err)
	}
	c := &crScenario{scenario: f.scenario, crs: crs}
	if err := c.slack.SetMembers(c.ctx, ChannelCRFailover, fakeBot); err != nil {
		t.Fatal(err)
	}
	tm := c.newTeam("failover")
	tm.addBinding("failover", ChannelCRFailover, crAgentAnswer)
	c.waitRow("failover", map[string]string{"Ready": "True", "Reason": "OK"})

	readyOf := func(ctx context.Context) (map[string]any, error) {
		obj, err := c.crs.Get(ctx, kube.BindingGVR, kube.TeamNamespace, "failover")
		if err != nil {
			return nil, err
		}
		cond, ok := condition(obj, "Ready")
		if !ok {
			return nil, errors.New("the Binding has no Ready condition: its status was blanked")
		}
		return cond, nil
	}
	was, err := readyOf(c.ctx)
	if err != nil {
		t.Fatal(err)
	}

	old := f.leader()
	if err := f.sig.Kill(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	next := f.takeover(old)
	// The new leader has probed the Binding once it reports it ready: its first pass ends within seconds of taking over, and is what a writer that
	// did not skip Pending would have written over the status with.
	err = Wait(c.ctx, 3*time.Minute, "the new leader to have probed the Binding", func(ctx context.Context) (bool, error) {
		v, err := f.reps.Counter(ctx, next, "agentkourier_binding_ready", map[string]string{"binding": kube.TeamNamespace + "/failover"})
		if errors.Is(err, ErrNoSample) {
			return false, nil
		}
		return v == 1, err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = Consistently(c.ctx, 30*time.Second, "the Binding stays Ready, with the lastTransitionTime it had", func(ctx context.Context) (bool, error) {
		now, err := readyOf(ctx)
		if err != nil {
			return false, err
		}
		if now["status"] != "True" || now["lastTransitionTime"] != was["lastTransitionTime"] {
			return false, fmt.Errorf("Ready is %v since %v; it was True since %v", now["status"], now["lastTransitionTime"], was["lastTransitionTime"])
		}
		return true, nil
	})
	if err != nil {
		t.Error(err)
	}

	// Not blanked is not the same as written: a new leader that never wrote status again would pass the above. An edit that moves no condition
	// (the output of the Binding) must show in the status as the new generation, written by the new leader.
	spec := tm.bindingSpec(ChannelCRFailover, crAgentAnswer)
	spec["chat"].(map[string]any)["output"] = "final"
	edited := tm.mustRewrite("failover", spec)
	err = Wait(c.ctx, statusWithin, fmt.Sprintf("the new leader to write observedGeneration %d", edited.GetGeneration()), func(ctx context.Context) (bool, error) {
		cond, err := readyOf(ctx)
		if err != nil {
			return false, err
		}
		return cond["observedGeneration"] == edited.GetGeneration() && cond["status"] == "True", nil
	})
	if err != nil {
		t.Error(err)
	}
	f.settle()
}
