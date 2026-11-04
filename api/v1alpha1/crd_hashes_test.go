package v1alpha1_test

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// TestSchemaHashesMatchTheCommittedCRDs: the hash the broker carries (crd_schema_hashes.go, written by `make crds`) is the SHA-256 of each
// generated CRD file, which is what the chart stamps on the CRD it renders (test/chart holds the chart to the same value). A CRD that
// changes without `make crds` being run to the end fails here, and so does a CRD the constants do not know.
func TestSchemaHashesMatchTheCommittedCRDs(t *testing.T) {
	files := readYAMLs(t, crdsDir)
	var names []string
	for file, raw := range files {
		var crd struct {
			Metadata struct{ Name string } `json:"metadata"`
		}
		if err := yaml.Unmarshal(raw, &crd); err != nil || crd.Metadata.Name == "" {
			t.Fatalf("%s: no metadata.name (%v)", file, err)
		}
		names = append(names, crd.Metadata.Name)
		sum := sha256.Sum256(raw)
		got, ok := v1alpha1.SchemaSHA256(crd.Metadata.Name)
		if !ok || got != hex.EncodeToString(sum[:]) {
			t.Errorf("SchemaSHA256(%q) = %q, %v; want %x from %s: run `make crds` and commit the result", crd.Metadata.Name, got, ok, sum, file)
		}
	}
	slices.Sort(names)
	if want := v1alpha1.CRDNames(); !slices.Equal(names, want) {
		t.Errorf("the CRD files are %v; CRDNames is %v", names, want)
	}
	if got, ok := v1alpha1.SchemaSHA256("secrets.example.test"); ok || got != "" {
		t.Errorf("SchemaSHA256 of a name that is not ours = %q, %v; want none", got, ok)
	}
}
