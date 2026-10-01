package e2e

import "testing"

func TestCourierExpected(t *testing.T) {
	for tier, want := range map[string]bool{"fast": true, "full": false} {
		if got, err := CourierExpected(tier); err != nil || got != want {
			t.Errorf("CourierExpected(%q) = %t, %v; want %t", tier, got, err, want)
		}
	}
	for _, tier := range []string{"", "Fast", "fast ", "prod", "none"} {
		if _, err := CourierExpected(tier); err == nil {
			t.Errorf("CourierExpected(%q) succeeded; want an error: a tier is never guessed", tier)
		}
	}
}
