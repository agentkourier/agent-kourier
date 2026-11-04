// Package v1alpha1 defines the agentkourier.dev/v1alpha1 resources (spec section 7):
// AgentBackend, ChatConnection, and Binding.
//
// The types carry kubebuilder markers from which `make crds` generates the CustomResourceDefinitions under
// charts/agent-kourier/files/crds (D30); the object:root markers are how the generator finds the three kinds.
// No deepcopy code is generated, because nothing here is handed to a client that needs runtime.Object.
// Agent Kourier loads the resources from YAML files, which get no API-server validation, so each resource has
// Default and Validate methods. Callers run Default first, then Validate. Both look at one object only:
// reference resolution, allowedNamespaces, and the one-Binding-per-channel rule belong to the config loader
// (WP-21).
//
// +groupName=agentkourier.dev
package v1alpha1

// Group and Version identify this API; APIVersion is the value of a resource's apiVersion.
const (
	Group      = "agentkourier.dev"
	Version    = "v1alpha1"
	APIVersion = Group + "/" + Version
)
