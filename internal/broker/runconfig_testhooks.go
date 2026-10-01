//go:build testhooks

package broker

import "log/slog"

// testOptions holds the option that exists only in a testhooks build, which is a test image and never a
// production one. It lets courier run before the interactions (WP-33) are wired in, which is what the
// fast tier deploys until WP-35h9; 35h9 deletes it. A config sets it as c.NoInteractions.
type testOptions struct {
	// NoInteractions runs with no handler for the presses of buttons.
	NoInteractions bool
}

func (o testOptions) allowsNoInteractions() bool { return o.NoInteractions }

// warnTestOptions says, loudly, that this is a testhooks build running without what a real one needs.
func (o testOptions) warnTestOptions(log *slog.Logger) {
	if o.NoInteractions {
		log.Warn("testhooks build: running with no interactions: button presses are dropped")
	}
}
