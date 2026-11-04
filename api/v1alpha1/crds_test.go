package v1alpha1_test

import (
	"bytes"
	"maps"
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
	// go test caches a passing result keyed on the files the test itself opens, and it never sees the ones that the
	// generator subprocess reads. Without these Stats an edit to a type or marker would replay "ok (cached)" and
	// pass without a regeneration. Stat records each file's size and modification time, so an in-place edit
	// invalidates the cache, and an unchanged tree still caches. go.mod is here because it pins the generator.
	goFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append(goFiles, "../../go.mod") {
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
	}

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

	if !slices.Equal(slices.Sorted(maps.Keys(want)), slices.Sorted(maps.Keys(got))) {
		t.Fatalf("CRD files differ from generation: committed %v, generated %v; run `make crds`",
			slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
	for name, w := range want {
		if !bytes.Equal(w, stripSchemaHash(got[name])) { // the committed file also carries the hash annotation, which make crds adds after controller-gen
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
