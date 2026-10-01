package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// CourierNamespace is the namespace of the one Deployment RolloutRestart may restart.
	CourierNamespace = "courier"
	// CourierDeployment is the name of that Deployment.
	CourierDeployment = "courier"

	// restartedAt is the template annotation `kubectl rollout restart` sets: a new value is a new pod
	// template, so a new rollout.
	restartedAt = "kubectl.kubernetes.io/restartedAt"
)

// ErrRestartRefused is returned, wrapped, for a rollout restart of anything but courier's own
// Deployment.
var ErrRestartRefused = errors.New("kube: refusing to restart anything but the " + CourierNamespace + "/" + CourierDeployment + " Deployment")

// RolloutRestart restarts courier through its Deployment, as `kubectl rollout restart` does, and
// returns once the rollout has finished and the new pod is ready: it patches an annotation of the pod
// template, so the Deployment's strategy: Recreate runs, and never deletes a pod ([Pods.Kill] and
// [Pods.Restart] refuse courier's namespace, because a pod delete would bypass it with the SQLite
// volume still mounted). It restarts that Deployment only, and says so before it calls the API.
// The runner's Role needs get and patch on deployments in courier's namespace, as for
// [Pods.SetFault] (hack/kind/e2e/runner.yaml).
func (p *Pods) RolloutRestart(ctx context.Context, namespace, deployment string) error {
	if namespace != CourierNamespace || deployment != CourierDeployment {
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
