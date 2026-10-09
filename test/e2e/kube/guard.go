// Package kube gives the E2E runner control over pods: list, crash, restart, wait for Ready.
//
// The runner is an in-cluster Job in namespace e2e. The clientset comes from
// rest.InClusterConfig and nothing else (no kubeconfig loader, so a developer's current context
// is unreachable), and construction refuses unless the service account's namespace is e2e.
//
// The runner must never carry the label app.kubernetes.io/name=agent-kourier (UNCONFIRMED(WP-35):
// Agent Kourier's real label and namespace). The full-tier NetworkPolicy
// (hack/kind/full/networkpolicy.yaml) admits any pod with that label in Agent Kourier's namespace to
// the kagent front door, so a runner wearing it would be trusted as Agent Kourier.
//
// The runner's Role needs list and delete on pods in each namespace it controls; that Role,
// including the one in Agent Kourier's namespace, is UNCONFIRMED(WP-35).
package kube

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// Namespace is the only service-account namespace the in-cluster guard accepts.
	Namespace = "e2e"
	// DefaultNamespaceFile is where Kubernetes mounts the service account's namespace.
	DefaultNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// ErrNotInCluster is returned, wrapped, when there is no in-cluster service account to use.
var ErrNotInCluster = errors.New("kube: not running in a cluster")

// ErrWrongNamespace is returned, wrapped, when the service account's namespace is not [Namespace].
var ErrWrongNamespace = errors.New("kube: service-account namespace is not " + Namespace)

// NewInCluster builds a [Pods] from rest.InClusterConfig, after checking that the service
// account's namespace, read from [DefaultNamespaceFile], is [Namespace].
func NewInCluster() (*Pods, error) {
	return newInCluster(rest.InClusterConfig, DefaultNamespaceFile)
}

func newInCluster(inClusterConfig func() (*rest.Config, error), namespaceFile string) (*Pods, error) {
	cfg, err := inClusterConfig()
	if err != nil {
		if errors.Is(err, rest.ErrNotInCluster) {
			return nil, fmt.Errorf("%w: %w", ErrNotInCluster, err)
		}
		return nil, fmt.Errorf("kube: in-cluster config: %w", err)
	}
	raw, err := os.ReadFile(namespaceFile) //nolint:gosec // the path is the mounted service-account file or a test's temp file
	if err != nil {
		return nil, fmt.Errorf("kube: read service-account namespace: %w", err)
	}
	if got := strings.TrimSpace(string(raw)); got != Namespace {
		return nil, fmt.Errorf("%w: got %q; refusing to control pods", ErrWrongNamespace, got)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: build clientset: %w", err)
	}
	return newPods(client), nil
}
