package store

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/cma/internal/limitsdoc"
)

// TestLimitsPage holds the store's limits to docs/reference/limits.md: the election's lock and statement timeouts, and
// ListTasks's page sizes and the history it keeps by default.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "cma.lease-write-lock-timeout", writeLockTimeout)
	limitsdoc.Duration(t, "cma.lease-take-lock-timeout", takeLockTimeout)
	limitsdoc.Duration(t, "cma.lease-take-statement-timeout", takeStatementTimeout)
	limitsdoc.Count(t, "cma.list-page-default", defaultPageSize)
	limitsdoc.Count(t, "cma.list-page-max", maxPageSize)
	limitsdoc.Count(t, "cma.list-history", defaultHistoryLength)
}
