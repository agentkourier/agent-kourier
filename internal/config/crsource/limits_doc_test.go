package crsource

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to how long a deleted custom resource is still served, and how many ChatConnections the source serves.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "settleFloor", settleFloor)
	limitsdoc.Duration(t, "goneGrace", goneGrace)
	limitsdoc.Count(t, "servedConnections", maxServedConnections)
}
