// Package v1alpha1 defines the agentcourier.dev/v1alpha1 resources (spec section 7):
// AgentBackend, ChatConnection, and Binding.
//
// The types carry kubebuilder markers so CRDs and deepcopy code can be generated later
// without changing them (D30). Until then courier loads the resources from YAML files, which
// get no API-server validation, so each resource has Default and Validate methods. Callers
// run Default first, then Validate. Both look at one object only: reference resolution,
// allowedNamespaces, and the one-Binding-per-channel rule belong to the config loader (WP-21).
//
// +kubebuilder:object:generate=true
// +groupName=agentcourier.dev
package v1alpha1

// Group and Version identify this API; APIVersion is the value of a resource's apiVersion.
const (
	Group      = "agentcourier.dev"
	Version    = "v1alpha1"
	APIVersion = Group + "/" + Version
)
