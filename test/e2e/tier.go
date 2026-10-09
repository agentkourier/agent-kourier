package e2e

import "fmt"

// CheckTier validates the fast, full (kagent), and AX runner tiers from Env.Tier. A
// tier is never guessed: the empty value is an error too. Every tier has Agent Kourier, so a test that needs it
// fails, and does not skip, when its pod is missing, which is what a Deployment that was rejected looks like.
func CheckTier(tier string) error {
	switch tier {
	case "fast", "full", "ax":
		return nil
	}
	return fmt.Errorf("E2E_TIER = %q; want fast, full or ax (hack/kind/e2e-run.sh sets it)", tier)
}
