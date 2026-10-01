//go:build !testhooks

package broker

import "log/slog"

// testOptions is empty in the production build: there is no NoRenderer or NoInteractions, so a
// binary without a renderer cannot be asked to start.
type testOptions struct{}

func (testOptions) allowsNoRenderer() bool     { return false }
func (testOptions) allowsNoInteractions() bool { return false }
func (testOptions) warnTestOptions(*slog.Logger) {
}
