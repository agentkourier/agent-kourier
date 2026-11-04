package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/sourcetest"
)

// The directory source is the other half of the conformance suite: the custom resource source runs the same cases.
func TestDirSourceConformance(t *testing.T) {
	sourcetest.Run(t, func(t *testing.T, c sourcetest.Corpus) config.Source {
		dir := t.TempDir()
		for i, d := range c.Docs {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.yaml", i)), []byte(d), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return config.NewDirSource(dir)
	}, sourcetest.Options{PerObject: false})
}
