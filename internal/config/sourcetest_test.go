package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/sourcetest"
)

// The directory source is the other half of the conformance suite: the custom resource source runs the same cases.
func TestDirSourceConformance(t *testing.T) {
	sourcetest.Run(t, func(t *testing.T, c sourcetest.Corpus) config.Source {
		return config.NewDirSource(writeDocs(t, t.TempDir(), c.Docs))
	}, sourcetest.Options{PerObject: false, Edit: func(t *testing.T, c sourcetest.Corpus) (config.Source, func([]string)) {
		dir := writeDocs(t, t.TempDir(), c.Docs)
		src := config.NewDirSource(dir)
		return src, func(docs []string) {
			before := src.Fingerprint(t.Context())
			writeDocs(t, dir, docs)
			if src.Fingerprint(t.Context()) == before {
				t.Fatal("rewriting the files did not move the fingerprint")
			}
		}
	}})
}

// writeDocs writes each document to dir as a file of its own, the ith as i.yaml, and returns dir. A file that is written
// again gets a modification time a second later than the one before, so that its fingerprint moves even when its size does not.
func writeDocs(t *testing.T, dir string, docs []string) string {
	t.Helper()
	for i, d := range docs {
		name := filepath.Join(dir, fmt.Sprintf("%d.yaml", i))
		old, statErr := os.Stat(name)
		if err := os.WriteFile(name, []byte(d), 0o600); err != nil {
			t.Fatal(err)
		}
		if statErr == nil {
			later := old.ModTime().Add(time.Second)
			if err := os.Chtimes(name, later, later); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}
