package config

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the limits of the config loader. The broker leaves the
// Reloader's poll to its default.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Bytes(t, "configFile", maxFileSize)
	limitsdoc.Duration(t, "reloadPoll", NewReloader(ReloaderConfig{}).poll)
}
