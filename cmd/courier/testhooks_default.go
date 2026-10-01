//go:build !testhooks

package main

import (
	"log/slog"

	"github.com/agentkourier/agent-kourier/internal/broker"
)

// testHooksBuild is false in the production build.
const testHooksBuild = false

// applyTestHooks does nothing in the production build: it has no test options to set, and says nothing
// about test builds, whose strings it must not carry (the binary proof in binary_test.go checks).
func applyTestHooks(*broker.RunConfig, *slog.Logger) {}
