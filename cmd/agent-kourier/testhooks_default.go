//go:build !testhooks

package main

import "log/slog"

// applyTestHooks does nothing in the production build, and says nothing about test builds, whose strings it
// must not carry (the binary proof in binary_test.go checks).
func applyTestHooks(*slog.Logger) {}
