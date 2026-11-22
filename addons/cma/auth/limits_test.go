package auth

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/cma/internal/limitsdoc"
)

// TestLimitsPage holds how often the token file is read to docs/reference/limits.md.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "cma.token-reload", ReloadInterval)
}
