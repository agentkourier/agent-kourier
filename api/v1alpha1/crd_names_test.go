package v1alpha1_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
)

// The names an operator types are part of the CRDs: the documentation tells them to run `kubectl get akbinding`, and
// `kubectl get agentkourier` for all three. Regenerating from markers that lost one would be consistent with the committed
// files, so TestCommittedCRDsMatchGeneration cannot see it go.
func TestTheCRDsServeTheNamesThatTheDocumentationTells(t *testing.T) {
	for _, tc := range []struct {
		file, kind, plural, short string
	}{
		{"agentkourier.dev_bindings.yaml", "Binding", "bindings", "akbinding"},
		{"agentkourier.dev_agentbackends.yaml", "AgentBackend", "agentbackends", "akbackend"},
		{"agentkourier.dev_chatconnections.yaml", "ChatConnection", "chatconnections", "akconnection"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(crdsDir, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			var crd struct {
				Spec struct {
					Group string `json:"group"`
					Scope string `json:"scope"`
					Names struct {
						Kind       string   `json:"kind"`
						Plural     string   `json:"plural"`
						ShortNames []string `json:"shortNames"`
						Categories []string `json:"categories"`
					} `json:"names"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal(data, &crd); err != nil {
				t.Fatal(err)
			}
			n := crd.Spec.Names
			if crd.Spec.Group != "agentkourier.dev" || crd.Spec.Scope != "Namespaced" || n.Kind != tc.kind || n.Plural != tc.plural {
				t.Errorf("group %q scope %q kind %q plural %q; want agentkourier.dev, Namespaced, %s, %s", crd.Spec.Group, crd.Spec.Scope, n.Kind, n.Plural, tc.kind, tc.plural)
			}
			if !slices.Equal(n.ShortNames, []string{tc.short}) {
				t.Errorf("shortNames = %v; want [%s], which `kubectl get %s` needs", n.ShortNames, tc.short, tc.short)
			}
			if !slices.Equal(n.Categories, []string{"agentkourier"}) {
				t.Errorf("categories = %v; want [agentkourier], which `kubectl get agentkourier` lists all three by", n.Categories)
			}
		})
	}
}
