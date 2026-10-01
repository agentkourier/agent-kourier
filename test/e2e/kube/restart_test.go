package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// RolloutRestart asks for a new rollout through the pod template, as `kubectl rollout restart`
// does, and returns once it has finished. Nothing else about the Deployment changes, and no pod is
// deleted: courier runs under strategy: Recreate on one SQLite volume.
func TestRolloutRestart_AnnotatesTheTemplateAndWaitsForTheRollout(t *testing.T) {
	cs := fake.NewClientset(courierDeployment())
	gets := rolloutAfter(cs, 5)
	p := newPods(cs)
	p.poll = time.Millisecond
	tick := 0
	p.now = func() time.Time { tick++; return time.Unix(1_700_000_000, int64(tick)).UTC() }

	if err := p.RolloutRestart(t.Context(), "courier", "courier"); err != nil {
		t.Fatal(err)
	}
	if *gets < 5 {
		t.Fatalf("RolloutRestart returned after %d reads of the Deployment, before the controller caught up", *gets)
	}
	d, _ := cs.AppsV1().Deployments("courier").Get(t.Context(), "courier", metav1.GetOptions{})
	first := d.Spec.Template.Annotations[restartedAt]
	if first == "" || len(d.Spec.Template.Spec.Containers) != 2 || envOf(d, "courier")["LOG_LEVEL"] != "debug" {
		t.Fatalf("template = %+v; want %s set, the containers and their env kept", d.Spec.Template, restartedAt)
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "pods" {
			t.Fatalf("RolloutRestart touched pods (%s): a restart is a rollout, never a pod delete", a.GetVerb())
		}
	}
	// Another restart is another value, or it would patch nothing and roll nothing.
	rolloutAfter(cs, 8)
	if err := p.RolloutRestart(t.Context(), "courier", "courier"); err != nil {
		t.Fatal(err)
	}
	d, _ = cs.AppsV1().Deployments("courier").Get(t.Context(), "courier", metav1.GetOptions{})
	if second := d.Spec.Template.Annotations[restartedAt]; second == "" || second == first {
		t.Fatalf("second restart annotation %q after %q; want a different value", second, first)
	}
}

// It restarts courier's own Deployment and nothing else, and says so before it calls the API.
func TestRolloutRestart_RefusesAnythingButCourier(t *testing.T) {
	for name, tc := range map[string]struct{ namespace, deployment string }{
		"another namespace":  {Namespace, "courier"},
		"another Deployment": {"courier", "sink"},
		"kube-system":        {"kube-system", "coredns"},
		"nothing":            {"", ""},
	} {
		cs := fake.NewClientset(courierDeployment())
		p := newPods(cs)
		if err := p.RolloutRestart(t.Context(), tc.namespace, tc.deployment); !errors.Is(err, ErrRestartRefused) {
			t.Errorf("%s: RolloutRestart = %v; want ErrRestartRefused", name, err)
		}
		if n := len(cs.Actions()); n != 0 {
			t.Errorf("%s: %d API calls were made; want none before the refusal", name, n)
		}
	}
}

func TestRolloutRestart_FailsWhenTheRolloutNeverCompletesOrTheDeploymentIsMissing(t *testing.T) {
	p := newPods(fake.NewClientset(courierDeployment()))
	p.poll = time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := p.RolloutRestart(ctx, "courier", "courier"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a rollout that never completes: %v; want a deadline error", err)
	}
	if err := newPods(fake.NewClientset()).RolloutRestart(t.Context(), "courier", "courier"); err == nil {
		t.Error("a missing Deployment: RolloutRestart succeeded")
	}
}
