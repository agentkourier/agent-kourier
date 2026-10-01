package kube

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
)

func nsFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewInCluster_Guard(t *testing.T) {
	inCluster := func() (*rest.Config, error) { return &rest.Config{Host: "https://127.0.0.1:1"}, nil }
	outside := func() (*rest.Config, error) { return nil, rest.ErrNotInCluster }
	tests := []struct {
		name    string
		config  func() (*rest.Config, error)
		ns      string
		wantErr error // nil means the guard must pass
	}{
		{name: "not in cluster", config: outside, ns: "e2e", wantErr: ErrNotInCluster},
		{name: "wrong namespace", config: inCluster, ns: "courier\n", wantErr: ErrWrongNamespace},
		{name: "e2e namespace", config: inCluster, ns: "e2e\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := newInCluster(tc.config, nsFile(t, tc.ns))
			if tc.wantErr == nil && err != nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if (p == nil) != (tc.wantErr != nil) {
				t.Fatalf("Pods = %v, guard error = %v", p, tc.wantErr)
			}
		})
	}
}

// TestNewInCluster_RefusesOutsideCluster covers the exported constructor, which must be wired
// to rest.InClusterConfig. Clearing the service env makes that fail wherever the tests run.
func TestNewInCluster_RefusesOutsideCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := NewInCluster(); !errors.Is(err, ErrNotInCluster) {
		t.Fatalf("err = %v, want ErrNotInCluster", err)
	}
}
