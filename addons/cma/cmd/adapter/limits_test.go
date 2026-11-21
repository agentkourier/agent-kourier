package main

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/cma/internal/limitsdoc"
)

// TestLimitsPage holds the server's timeouts to docs/reference/limits.md: the wait for a request's headers, and for the
// requests in flight at a graceful stop.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "cma.read-header-timeout", readHeaderTimeout)
	limitsdoc.Duration(t, "cma.shutdown-grace", shutdownTimeout)
}
