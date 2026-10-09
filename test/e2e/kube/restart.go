package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// AgentKourierNamespace is the namespace of the one Deployment RolloutRestart may restart.
	AgentKourierNamespace = "agent-kourier"
	// AgentKourierDeployment is the name of that Deployment.
	AgentKourierDeployment = "agent-kourier"

	// restartedAt is the template annotation `kubectl rollout restart` sets: a new value is a new pod
	// template, so a new rollout.
	restartedAt = "kubectl.kubernetes.io/restartedAt"
)

// ErrRestartRefused is returned, wrapped, for a rollout restart of anything but Agent Kourier's own
// Deployment.
var ErrRestartRefused = errors.New("kube: refusing to restart anything but the " + AgentKourierNamespace + "/" + AgentKourierDeployment + " Deployment")

// RolloutRestart restarts Agent Kourier through its Deployment, as `kubectl rollout restart` does, and
// returns once the rollout has finished and the new pod is ready: it patches an annotation of the pod
// template, so the Deployment's strategy: Recreate runs, and never deletes a pod ([Pods.Kill] and
// [Pods.Restart] refuse Agent Kourier's namespace, because a pod delete would bypass it with the SQLite
// volume still mounted). It restarts that Deployment only, and says so before it calls the API.
// The runner's Role needs get and patch on deployments in Agent Kourier's namespace, as for
// [Pods.SetFault] (hack/kind/e2e/runner.yaml).
func (p *Pods) RolloutRestart(ctx context.Context, namespace, deployment string) error {
	if namespace != AgentKourierNamespace || deployment != AgentKourierDeployment {
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
