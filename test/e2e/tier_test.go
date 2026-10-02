package e2e

import "testing"

func TestCheckTier(t *testing.T) {
	for _, tier := range []string{"fast", "full"} {
		if err := CheckTier(tier); err != nil {
			t.Errorf("CheckTier(%q) = %v; want nil", tier, err)
		}
	}
	for _, tier := range []string{"", "Fast", "fast ", "prod", "none"} {
		if err := CheckTier(tier); err == nil {
			t.Errorf("CheckTier(%q) succeeded; want an error: a tier is never guessed", tier)
		}
	}
}
