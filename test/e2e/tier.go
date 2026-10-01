package e2e

import "fmt"

// CourierExpected says whether the tier has courier running, from Env.Tier: the fast tier does, and a
// test that needs it must fail, not skip, when its pod is missing, which is what a Deployment that was
// rejected looks like. The full tier gets courier with WP-35i and is skipped until then. Any other value,
// the empty one included, is an error: a tier is never guessed.
func CourierExpected(tier string) (bool, error) {
	switch tier {
	case "fast":
		return true, nil
	case "full":
		return false, nil // TODO(WP-35i): courier in the full tier
	}
	return false, fmt.Errorf("E2E_TIER = %q; want fast or full (hack/kind/e2e-run.sh sets it)", tier)
}
