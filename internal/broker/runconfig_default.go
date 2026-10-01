//go:build !testhooks

package broker

import "log/slog"

// testOptions is empty in the production build: there is no NoInteractions, so a binary without
// interactions cannot be asked to start.
type testOptions struct{}

func (testOptions) allowsNoInteractions() bool   { return false }
func (testOptions) warnTestOptions(*slog.Logger) {}
