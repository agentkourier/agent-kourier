package agent

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/auth/oidc"
	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestCallerCheckLimits holds how long an OIDC discovery or key fetch may take, which only this add-on's caller check
// makes, to the documentation. The limits every add-on shares are the kit's, which its tests hold.
func TestCallerCheckLimits(t *testing.T) {
	limitsdoc.Duration(t, "ax.oidc-timeout", oidc.Timeout)
}
