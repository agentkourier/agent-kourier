package store

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/cma/internal/limitsdoc"
)

// TestLimitsPage holds the store's limits to docs/reference/limits.md: the server-side timeouts each Postgres connection
// starts with unless the DSN sets them, the election's lock and statement timeouts, the SQLite busy timeout, and
// ListTasks's page sizes and the history it keeps by default.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "cma.pg-idle-in-transaction", idleInTxTimeout)
	limitsdoc.Duration(t, "cma.pg-statement-timeout", statementTimeout)
	limitsdoc.Duration(t, "cma.lease-write-lock-timeout", writeLockTimeout)
	limitsdoc.Duration(t, "cma.lease-take-lock-timeout", takeLockTimeout)
	limitsdoc.Duration(t, "cma.lease-take-statement-timeout", takeStatementTimeout)
	limitsdoc.Duration(t, "cma.sqlite-busy-timeout", sqliteBusyTimeout)
	limitsdoc.Count(t, "cma.list-page-default", defaultPageSize)
	limitsdoc.Count(t, "cma.list-page-max", maxPageSize)
	limitsdoc.Count(t, "cma.list-history", defaultHistoryLength)
}
