package e2e

import "fmt"

// CheckTier returns an error unless tier is "fast" or "full", the two tiers a runner is in, from Env.Tier. A
// tier is never guessed: the empty value is an error too. Both tiers have courier, so a test that needs it
// fails, and does not skip, when its pod is missing, which is what a Deployment that was rejected looks like.
func CheckTier(tier string) error {
	switch tier {
	case "fast", "full":
		return nil
	}
	return fmt.Errorf("E2E_TIER = %q; want fast or full (hack/kind/e2e-run.sh sets it)", tier)
}
