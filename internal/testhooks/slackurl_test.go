//go:build testhooks

package testhooks

import "testing"

// A unit test's override wins over the environment, and the environment, which the test image sets, is what is read without
// one.
func TestSlackAPIURLPrefersTheOverrideToTheEnvironment(t *testing.T) {
	t.Setenv(SlackAPIURLEnv, "http://from-the-env.test/")
	for name, tc := range map[string]struct {
		o    Overrides
		want string
	}{
		"no override": {Overrides{}, "http://from-the-env.test/"},
		"an override": {Overrides{SlackAPIURL: "http://from-the-test.test/"}, "http://from-the-test.test/"},
	} {
		if got := SlackAPIURL(tc.o); got != tc.want {
			t.Errorf("%s: SlackAPIURL = %q, want %q", name, got, tc.want)
		}
	}
	t.Setenv(SlackAPIURLEnv, "")
	if got := SlackAPIURL(Overrides{}); got != "" {
		t.Errorf("SlackAPIURL = %q with neither set, want empty: Slack's own", got)
	}
}
