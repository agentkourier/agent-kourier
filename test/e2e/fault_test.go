//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/agentkourier/agent-kourier/internal/testhooks"
	"github.com/agentkourier/agent-kourier/test/e2e/kube"
)

// faultOf is the COURIER_FAULT the pod's courier container runs with ("" if unset).
func faultOf(t *testing.T, pod corev1.Pod) string {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name != courierContainer {
			continue
		}
		for _, e := range c.Env {
			if e.Name == testhooks.FaultEnv {
				return e.Value
			}
		}
		return ""
	}
	t.Fatalf("pod %s has no %s container", pod.Name, courierContainer)
	return ""
}

// leaderPod is the pod of courier's leader, which a rollout leaves once it has finished: the one pod that says it leads
// (courier_leader) and is connected to Slack. It does not count pods: with Postgres a rollout runs a surge, and the old
// pod, which hands its Lease over as it stops, may still be listed, and with two replicas one of the two is the standby.
func leaderPod(ctx context.Context, t *testing.T, pods *kube.Pods) corev1.Pod {
	t.Helper()
	leader, err := NewReplicas(pods).WaitLeader(ctx, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	list, err := pods.List(ctx, kube.CourierNamespace, courierPods)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.Name == leader.Name {
			return p
		}
	}
	t.Fatalf("the leader %s is in none of courier's pods", leader.Name)
	return corev1.Pod{}
}

// TestFaultArmsAndDisarmsOnTheRealDeployment arms COURIER_FAULT on courier's Deployment through the runner's
// own Role, with SetFault, and disarms it: each is a rollout, which ends with new pods that are ready, with a leader
// that is a new pod, that carries the variable (or not) and that is connected to the fake Slack (with two replicas
// both pods are new, and the leader is the one looked at). The fault armed is the one the scenarios use, and it only
// acts at a post, so courier stays up while it is armed.
func TestFaultArmsAndDisarmsOnTheRealDeployment(t *testing.T) {
	env, ctx := NewEnv(os.Getenv), t.Context()
	pods, err := kube.NewInCluster()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckTier(env.Tier); err != nil {
		t.Fatal(err)
	}
	if list, err := pods.List(ctx, kube.CourierNamespace, courierPods); err != nil {
		t.Fatal(err)
	} else if len(list) == 0 {
		t.Fatal("the tier has no courier pod: its Deployment is missing or was rejected")
	}
	// A failure part way must not leave courier armed for the tests after it.
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if err := pods.SetFault(cleanup, kube.CourierNamespace, kube.CourierDeployment, courierContainer, ""); err != nil {
			t.Errorf("disarm at the end: %v", err)
		}
	})
	set := func(fault string) corev1.Pod {
		t.Helper()
		rollout, cancel := context.WithTimeout(ctx, 3*time.Minute) // a stuck rollout fails here, with its reason
		defer cancel()
		if err := pods.SetFault(rollout, kube.CourierNamespace, kube.CourierDeployment, courierContainer, fault); err != nil {
			t.Fatal(err)
		}
		return leaderPod(ctx, t, pods)
	}
	before := leaderPod(ctx, t, pods)
	armed := set(testhooks.CrashAfterPost)
	if got := faultOf(t, armed); got != testhooks.CrashAfterPost || armed.Name == before.Name {
		t.Errorf("armed pod %s (was %s) has %s=%q; want a new pod with %s", armed.Name, before.Name, testhooks.FaultEnv, got, testhooks.CrashAfterPost)
	}
	disarmed := set("")
	if got := faultOf(t, disarmed); got != "" || disarmed.Name == armed.Name {
		t.Errorf("disarmed pod %s (was %s) has %s=%q; want a new pod without it", disarmed.Name, armed.Name, testhooks.FaultEnv, got)
	}
	// The new courier is up and has connected to the fake, which is how a scenario knows it can go on.
	sl := NewSlack(env.SlackURL)
	err = Wait(ctx, 30*time.Second, "courier to connect to the fake Slack", func(ctx context.Context) (bool, error) {
		n, err := sl.Connections(ctx)
		return n >= 1, err
	})
	if err != nil {
		t.Error(err)
	}
}
