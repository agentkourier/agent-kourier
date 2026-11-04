package v1alpha1_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// crdsDir is where `make crds` writes the generated CRDs, relative to this package.
const crdsDir = "../../charts/agent-kourier/files/crds"

// TestCommittedCRDsMatchGeneration fails when the CRDs committed under charts/agent-kourier/files/crds differ from
// what controller-gen produces from this package's types and markers. It runs the same generator invocation as
// `make crds` (keep the two in step), so a type or marker change that is not followed by `make crds` fails here.
func TestCommittedCRDsMatchGeneration(t *testing.T) {
	generated := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "go", "tool", "controller-gen", "crd", "paths=./api/v1alpha1", "output:crd:dir="+generated)
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("controller-gen: %v\n%s", err, out)
	}

	want := readYAMLs(t, generated)
	got := readYAMLs(t, crdsDir)
	if len(want) == 0 {
		t.Fatal("controller-gen wrote no CRDs")
	}

	names := func(m map[string][]byte) []string {
		var s []string
		for n := range m {
			s = append(s, n)
		}
		slices.Sort(s)
		return s
	}
	if !slices.Equal(names(want), names(got)) {
		t.Fatalf("CRD files differ from generation: committed %v, generated %v; run `make crds`", names(got), names(want))
	}
	for name, w := range want {
		if !bytes.Equal(w, got[name]) {
			t.Errorf("%s is out of date with api/v1alpha1; run `make crds` and commit the result", name)
		}
	}
}

// readYAMLs returns the contents of the .yaml files in dir by file name. A missing dir reads as empty.
func readYAMLs(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	files := make(map[string][]byte)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = b
	}
	return files
}
