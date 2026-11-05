package session

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to how long the manager waits on an agent. The broker leaves each
// of these to its default, so the fixture's Manager, which sets none, has the values.
func TestLimitsPage(t *testing.T) {
	cfg := newFixture(t).m.cfg
	limitsdoc.Duration(t, "giveUp", cfg.SendGiveUp)
	limitsdoc.Duration(t, "workingAge", cfg.MaxWorkingAge)
	limitsdoc.Duration(t, "retryBase", cfg.RetryBase)
	limitsdoc.Duration(t, "retryMax", cfg.RetryMax)
	limitsdoc.Duration(t, "busyNotice", waitNoticeAfter)
	limitsdoc.Duration(t, "cancelBound", cancelTimeout)
}
