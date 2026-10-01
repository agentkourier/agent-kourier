//go:build testhooks

package broker

import "log/slog"

// testOptions holds the two options that exist only in a testhooks build, which is a test image
// and never a production one. They let courier run before the renderer (WP-32e) and the
// interactions (WP-33) are wired in, which is what the fast tier deploys until WP-35h; 35h deletes
// them. A config sets them as c.NoRenderer and c.NoInteractions.
type testOptions struct {
	// NoRenderer runs with no Sink: the agent's output is dropped.
	NoRenderer bool
	// NoInteractions runs with no handler for the presses of buttons.
	NoInteractions bool
}

func (o testOptions) allowsNoRenderer() bool     { return o.NoRenderer }
func (o testOptions) allowsNoInteractions() bool { return o.NoInteractions }

// warnTestOptions says, loudly, that this is a testhooks build running without what a real one needs.
func (o testOptions) warnTestOptions(log *slog.Logger) {
	if o.NoRenderer {
		log.Warn("testhooks build: running with no renderer: no agent output is posted")
	}
	if o.NoInteractions {
		log.Warn("testhooks build: running with no interactions: button presses are dropped")
	}
}
