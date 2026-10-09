package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// SetFault sets AGENTKOURIER_FAULT to fault, on the named container of the Deployment, and returns once
// the rollout has finished (an empty fault disarms). A test image reads the variable at startup, so
// arming a fault is a rollout, never a pod delete: Agent Kourier's Deployment uses strategy: Recreate on
// one SQLite volume, and Kill and Restart refuse Agent Kourier's namespace. Other env vars and containers
// are left alone (a strategic merge patch). The container, and Agent Kourier's Deployment name and
// namespace, are UNCONFIRMED(WP-35g). The runner's Role needs get and patch on deployments there
// (hack/kind/e2e/runner.yaml).
//
// A fault stays armed until another rollout disarms it: the container, restarted by the kubelet
// after the crash, reads the same variable and crashes at its next post too. A scenario that arms
// crash-after-post (S5) must call SetFault with an empty fault straight after it sees the crash.
func (p *Pods) SetFault(ctx context.Context, namespace, deployment, container, fault string) error {
	if container == "" {
		return errors.New("set fault: a container name is required")
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []map[string]any{{"name": container, "env": []map[string]string{{"name": testhooks.FaultEnv, "value": fault}}}},
	}}}})
	if err != nil {
		return fmt.Errorf("set fault: %w", err)
	}
	return p.patchAndWait(ctx, namespace, deployment, patch, "set fault on")
}

// patchAndWait strategic-merge-patches the Deployment and waits for the rollout the patch asks for
// to finish: the controller has seen the new generation and every replica is new and ready. what
// begins the error of a patch that fails.
func (p *Pods) patchAndWait(ctx context.Context, namespace, deployment string, patch []byte, what string) error {
	client := p.client.AppsV1().Deployments(namespace)
	patched, err := client.Patch(ctx, deployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("%s deployment %s/%s: %w", what, namespace, deployment, err)
	}
	reason := "not yet read"
	err = wait.PollUntilContextCancel(ctx, p.poll, true, func(ctx context.Context) (bool, error) {
		d, err := client.Get(ctx, deployment, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		reason = rolloutPending(d, patched.Generation)
		return reason == "", nil
	})
	if err != nil {
		return fmt.Errorf("wait for the rollout of %s/%s: %s: %w", namespace, deployment, reason, err)
	}
	return nil
}

// rolloutPending says what a Deployment still lacks of a finished rollout of generation gen, or ""
// if it has finished: the controller has seen gen, and every replica is new, present and ready.
func rolloutPending(d *appsv1.Deployment, gen int64) string {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	switch {
	case s.ObservedGeneration < gen:
		return fmt.Sprintf("the controller has seen generation %d of %d", s.ObservedGeneration, gen)
	case s.UpdatedReplicas != want || s.Replicas != want || s.ReadyReplicas != want:
		return fmt.Sprintf("%d updated, %d total and %d ready of %d replicas", s.UpdatedReplicas, s.Replicas, s.ReadyReplicas, want)
	}
	return ""
}
