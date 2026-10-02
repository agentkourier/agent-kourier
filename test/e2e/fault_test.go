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

// Courier's container and the label of its pod (hack/kind/fast/courier/deployment.yaml); its namespace
// and Deployment are kube.CourierNamespace and kube.CourierDeployment.
const (
	courierContainer = "courier"
	courierPods      = "app.kubernetes.io/name=courier"
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

// onePod is courier's only pod, which a Recreate rollout leaves once it has finished.
func onePod(ctx context.Context, t *testing.T, pods *kube.Pods) corev1.Pod {
	t.Helper()
	list, err := pods.List(ctx, kube.CourierNamespace, courierPods)
	if err != nil || len(list) != 1 {
		t.Fatalf("courier's pods = %d, %v; want exactly one after a rollout", len(list), err)
	}
	return list[0]
}

// TestFaultArmsAndDisarmsOnTheRealDeployment arms COURIER_FAULT on courier's Deployment through the runner's
// own Role, with SetFault, and disarms it: each is a rollout, which ends with one new pod that is ready,
// that carries the variable (or not) and that is connected to the fake Slack. The fault armed is the one the
// scenarios use, and it only acts at a post, so courier stays up while it is armed.
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
		return onePod(ctx, t, pods)
	}
	before := onePod(ctx, t, pods)
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
