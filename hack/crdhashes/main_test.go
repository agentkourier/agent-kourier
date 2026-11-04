package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Where the repository's own CRDs and Go file are, from this package.
const (
	repoCRDs   = "../../charts/agent-kourier/files/crds"
	repoGoFile = "../../api/v1alpha1/crd_schema_hashes.go"
)

// TestRunIsIdempotentAndWritesTheCommittedFiles: run on a copy of the committed CRDs changes nothing and writes the committed Go file, and run on the
// same files without their annotation (as controller-gen writes them) puts back the very same bytes. `make crds` is therefore deterministic.
func TestRunIsIdempotentAndWritesTheCommittedFiles(t *testing.T) {
	entries, err := os.ReadDir(repoCRDs)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no CRDs in %s: %v", repoCRDs, err)
	}
	dir := t.TempDir()
	committed := map[string][]byte{}
	bare := map[string][]byte{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(repoCRDs, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		committed[e.Name()] = b
		bare[e.Name()] = stamp.ReplaceAll(b, nil)
		if bytes.Equal(b, bare[e.Name()]) {
			t.Fatalf("%s carries no hash annotation: run `make crds`", e.Name())
		}
	}
	wantGo, err := os.ReadFile(repoGoFile)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "hashes.go")

	for name, files := range map[string]map[string][]byte{"stamped": committed, "as controller-gen writes them": bare} {
		for n, b := range files {
			if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for pass := 1; pass <= 2; pass++ { // the second pass changes nothing either
			if err := run(dir, out); err != nil {
				t.Fatalf("%s, pass %d: %v", name, pass, err)
			}
			for n, want := range committed {
				got, err := os.ReadFile(filepath.Join(dir, n))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("%s, pass %d: %s differs from the committed file", name, pass, n)
				}
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, wantGo) {
				t.Errorf("%s, pass %d: the Go file differs from the committed %s", name, pass, repoGoFile)
			}
		}
	}
}

// TestRunRefusesWhatItCannotStamp: no CRDs, and a file that does not open its metadata with annotations as controller-gen writes it, are errors,
// not a silent skip.
func TestRunRefusesWhatItCannotStamp(t *testing.T) {
	out := filepath.Join(t.TempDir(), "hashes.go")
	if err := run(t.TempDir(), out); err == nil {
		t.Error("run on an empty directory succeeded")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.yaml"), []byte("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: x.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(dir, out); err == nil {
		t.Error("run on a CRD with no annotations succeeded")
	}
}
