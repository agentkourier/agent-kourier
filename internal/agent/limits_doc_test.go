package agent

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to what the broker waits for from an agent. The broker never sets
// these in Config, so the defaults are the values.
func TestLimitsPage(t *testing.T) {
	cfg := Config{}.withDefaults()
	limitsdoc.Bytes(t, "replyBytes", cfg.MaxReplyBytes)
	limitsdoc.Bytes(t, "replyBytes", DefaultMaxReplyBytes)
	limitsdoc.Duration(t, "inactivity", cfg.InactivityTimeout)
	limitsdoc.Count(t, "reconnects", cfg.MaxReconnects)
	limitsdoc.Duration(t, "unaryTimeout", cfg.UnaryTimeout)
	limitsdoc.Duration(t, "connectTimeout", cfg.ConnectTimeout)
	limitsdoc.Count(t, "listPageStart", defaultListPageSize)
	limitsdoc.Count(t, "listPageMin", minListPageSize)
	limitsdoc.Count(t, "listPages", maxListPages)
	limitsdoc.Duration(t, "lookupTimeout", cfg.LookupTimeout)
	limitsdoc.Duration(t, "lookupTimeout", DefaultLookupTimeout)
}
