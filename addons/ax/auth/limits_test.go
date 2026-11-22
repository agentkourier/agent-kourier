package auth

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/limitsdoc"
)

// TestLimitsPage holds how often the token file is read, and how long an OIDC discovery or key fetch may take, to the
// documentation.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "ax.token-reload", ReloadInterval)
	limitsdoc.Duration(t, "ax.oidc-timeout", oidcTimeout)
}
