//go:build testhooks

package main

import "log/slog"

// applyTestHooks says loudly that this is a test build, which carries the fault switch and the Slack URL
// override of internal/testhooks and is never for production.
func applyTestHooks(log *slog.Logger) {
	log.Warn("testhooks build: this binary carries test hooks (a fault switch, a Slack URL override) and is never for production")
}
