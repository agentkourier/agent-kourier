//go:build testhooks

package main

import (
	"log/slog"

	"github.com/agentkourier/agent-kourier/internal/broker"
)

// testHooksBuild is true in a testhooks build, which is a test image and never a production one.
const testHooksBuild = true

// applyTestHooks says loudly that this is a test build, and lets it run without interactions, which the
// kind tiers deploy before WP-35h9 builds them. 35h9 deletes the option.
func applyTestHooks(cfg *broker.RunConfig, log *slog.Logger) {
	log.Warn("testhooks build: this binary carries test hooks (a fault switch, a Slack URL override) and is never for production")
	cfg.NoInteractions = true
}
