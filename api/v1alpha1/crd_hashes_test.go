package v1alpha1_test

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// schemaHashLine is the one line of a CRD file that its hash does not cover: the annotation, which `make crds` (hack/crdhashes) adds after
// controller-gen. It is written here again, not imported, so that the test holds the generator to the rule.
var schemaHashLine = regexp.MustCompile(`(?m)^    agentkourier\.dev/schema-sha256: [0-9a-f]{64}\n`)

// stripSchemaHash returns a CRD file without its hash annotation, which is what controller-gen wrote and what the hash is of.
func stripSchemaHash(file []byte) []byte { return schemaHashLine.ReplaceAll(file, nil) }

// TestSchemaHashesMatchTheCommittedCRDs: each committed CRD file carries its own SHA-256 (of the file without that one line) as the annotation
// agentkourier.dev/schema-sha256, so that whatever applies the file applies the hash; and the hash the broker carries (crd_schema_hashes.go) is
// the same value. A CRD that changes without `make crds` being run to the end fails here, and so does a CRD the constants do not know.
func TestSchemaHashesMatchTheCommittedCRDs(t *testing.T) {
	files := readYAMLs(t, crdsDir)
	var names []string
	for file, raw := range files {
		var crd struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(raw, &crd); err != nil || crd.Metadata.Name == "" {
			t.Fatalf("%s: no metadata.name (%v)", file, err)
		}
		names = append(names, crd.Metadata.Name)
		if n := len(schemaHashLine.FindAll(raw, -1)); n != 1 {
			t.Errorf("%s has %d hash annotation lines; want one: run `make crds`", file, n)
		}
		sum := sha256.Sum256(stripSchemaHash(raw))
		want := hex.EncodeToString(sum[:])
		if got := crd.Metadata.Annotations[v1alpha1.SchemaHashAnnotation]; got != want {
			t.Errorf("%s carries hash %q; want %s, the SHA-256 of the file without the annotation: run `make crds` and commit the result", file, got, want)
		}
		if got, ok := v1alpha1.SchemaSHA256(crd.Metadata.Name); !ok || got != want {
			t.Errorf("SchemaSHA256(%q) = %q, %v; want %s: run `make crds` and commit the result", crd.Metadata.Name, got, ok, want)
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
