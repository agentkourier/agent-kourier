package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// AgentKourierNamespace holds the broker and AX Deployments.
	AgentKourierNamespace = "agent-kourier"
	// AgentKourierDeployment is the name of the broker Deployment.
	AgentKourierDeployment = "agent-kourier"
	// AXDeployment is the name of the AX launcher Deployment.
	AXDeployment = "agent-kourier-ax"

	// restartedAt is the template annotation `kubectl rollout restart` sets: a new value is a new pod
	// template, so a new rollout.
	restartedAt = "kubectl.kubernetes.io/restartedAt"
)

// ErrRestartRefused is returned, wrapped, when a restart targets a different namespace or Deployment.
var ErrRestartRefused = errors.New("kube: rollout restart is limited to the broker and AX Deployments")

// RolloutRestart restarts the broker or AX launcher through its Deployment, as `kubectl rollout restart` does, and
// returns once the rollout has finished and the new pod is ready: it patches an annotation of the pod
// template, so the Deployment's strategy: Recreate runs, and never deletes a pod ([Pods.Kill] and
// [Pods.Restart] refuse Agent Kourier's namespace, because a pod delete would bypass it with the SQLite
// volume still mounted). It restarts that Deployment only, and says so before it calls the API.
// The runner's Role needs get and patch on deployments in Agent Kourier's namespace, as for
// [Pods.SetFault] (hack/kind/e2e/runner.yaml).
func (p *Pods) RolloutRestart(ctx context.Context, namespace, deployment string) error {
	if namespace != AgentKourierNamespace || (deployment != AgentKourierDeployment && deployment != AXDeployment) {
		return fmt.Errorf("%w: %s/%s", ErrRestartRefused, namespace, deployment)
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{restartedAt: p.now().UTC().Format(time.RFC3339Nano)},
	}}}})
	if err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	return p.patchAndWait(ctx, namespace, deployment, patch, "restart")
}
