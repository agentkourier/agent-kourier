package serve

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestCallerCheckLimits holds how often the agent reads its token file, which it watches at the kit's interval, and how
// long an OIDC discovery or key fetch may take, to the documentation.
func TestCallerCheckLimits(t *testing.T) {
	limitsdoc.Duration(t, "ax.token-reload", auth.ReloadInterval)
	limitsdoc.Duration(t, "ax.oidc-timeout", auth.OIDCTimeout)
}
