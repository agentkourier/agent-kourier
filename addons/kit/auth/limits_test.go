package auth

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds how often an add-on reads its token file, the interval the runtime watches it at, to
// docs/reference/limits.md and the add-ons' reference pages.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "kit.token-reload", ReloadInterval)
}
