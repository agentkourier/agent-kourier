package main

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the defaults that the binary itself fills in from an empty
// environment, which are the values the broker runs with: the chart sets AGENTKOURIER_SYNC_TIMEOUT only if its value is set.
func TestLimitsPage(t *testing.T) {
	c, err := loadEnv(env{}.get)
	if err != nil {
		t.Fatalf("loadEnv of an empty environment: %v", err)
	}
	limitsdoc.Duration(t, "syncTimeout", c.syncTimeout)
}
