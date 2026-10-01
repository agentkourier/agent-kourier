package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var deployments = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

// courierDeployment is a Deployment whose spec is already at generation 2, with the controller still
// at generation 1 and its one replica up to date and ready for that generation: a rollout that has
// been asked for and not begun, which must not look finished.
func courierDeployment() *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "courier", Name: "courier", Generation: 2},
		Spec: appsv1.DeploymentSpec{Replicas: &one, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "courier", Env: []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}},
			{Name: "sidecar"},
		}}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1},
	}
}

// rolloutAfter walks the controller through a Recreate rollout, one step per Get of the Deployment:
// from the third before the end it has seen the new generation but the old pod is gone and the new
// one is not up (nothing updated, present or ready); one step later the new pod exists but is not
// ready; and on the n-th it is ready. It returns a counter of the Gets.
func rolloutAfter(cs *fake.Clientset, n int) *int {
	gets := new(int)
	cs.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		*gets++
		if *gets < n-2 {
			return false, nil, nil
		}
		cur, _ := cs.Tracker().Get(deployments, "courier", "courier")
		d := cur.(*appsv1.Deployment).DeepCopy()
		switch *gets {
		case n - 2:
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: 2}
		case n - 1:
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1}
		default: // the n-th Get and every one after it
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1}
		}
		_ = cs.Tracker().Update(deployments, d, "courier")
		return false, nil, nil
	})
	return gets
}

func TestSetFault_PatchesTheEnvAndWaitsForTheRollout(t *testing.T) {
	cs := fake.NewClientset(courierDeployment())
	gets := rolloutAfter(cs, 5)
	p := newPods(cs)
	p.poll = time.Millisecond

	if err := p.SetFault(t.Context(), "courier", "courier", "courier", "crash-after-post"); err != nil {
		t.Fatal(err)
	}
	if *gets < 5 {
		t.Fatalf("SetFault returned after %d reads of the Deployment, before the controller caught up", *gets)
	}
	d, _ := cs.AppsV1().Deployments("courier").Get(t.Context(), "courier", metav1.GetOptions{})
	got := envOf(d, "courier")
	if got["COURIER_FAULT"] != "crash-after-post" || got["LOG_LEVEL"] != "debug" || len(d.Spec.Template.Spec.Containers) != 2 {
		t.Fatalf("env after the patch = %v in %d containers; want COURIER_FAULT added, LOG_LEVEL and the sidecar kept", got, len(d.Spec.Template.Spec.Containers))
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "pods" {
			t.Fatalf("SetFault touched pods (%s): a fault is set through the Deployment, never by deleting a pod", a.GetVerb())
		}
	}

	if err := p.SetFault(t.Context(), "courier", "courier", "courier", ""); err != nil {
		t.Fatal(err)
	}
	d, _ = cs.AppsV1().Deployments("courier").Get(t.Context(), "courier", metav1.GetOptions{})
	if got := envOf(d, "courier"); got["COURIER_FAULT"] != "" || len(got) != 2 {
		t.Fatalf("env after disarming = %v; want COURIER_FAULT present and empty", got)
	}
}

// envOf returns the env of the named container as a map.
func envOf(d *appsv1.Deployment, container string) map[string]string {
	out := map[string]string{}
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if c.Name == container {
				out[e.Name] = e.Value
			}
		}
	}
	return out
}

func TestSetFault_FailsWhenTheRolloutNeverCompletes(t *testing.T) {
	p := newPods(fake.NewClientset(courierDeployment()))
	p.poll = time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := p.SetFault(ctx, "courier", "courier", "courier", "crash-after-post")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SetFault = %v, want a deadline error naming the rollout", err)
	}
}

func TestSetFault_RefusesBadInputAndMissingDeployments(t *testing.T) {
	p := newPods(fake.NewClientset())
	for name, call := range map[string]func() error{
		"no container":       func() error { return p.SetFault(t.Context(), "courier", "courier", "", "x") },
		"no such Deployment": func() error { return p.SetFault(t.Context(), "courier", "courier", "courier", "x") },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: SetFault succeeded", name)
		}
	}
}

func TestPodDeletesAreRefusedOutsideE2E(t *testing.T) {
	// Courier runs under strategy: Recreate on one SQLite volume; a pod delete would bypass that.
	cp := sink("courier-0", "u1", true)
	cp.Namespace = "courier"
	cs := fake.NewClientset(cp)
	p := newPods(cs)
	p.poll = time.Millisecond
	if err := p.Kill(t.Context(), "courier", "courier-0"); !errors.Is(err, ErrDeleteRefused) {
		t.Errorf("Kill = %v, want ErrDeleteRefused", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second) // a Restart that is not refused would wait for a pod forever
	defer cancel()
	if _, err := p.Restart(ctx, "courier", sinkSel); !errors.Is(err, ErrDeleteRefused) {
		t.Errorf("Restart = %v, want ErrDeleteRefused", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "delete" {
			t.Errorf("a delete reached the API: %v", a)
		}
	}
}
