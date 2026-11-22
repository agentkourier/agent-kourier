package store

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds the store's limits to the documentation: the server-side timeouts each Postgres connection starts
// with unless the DSN sets them, the election's lock and statement timeouts, the SQLite busy timeout, and ListTasks's
// page sizes and the history it keeps by default.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "ax.pg-idle-in-transaction", idleInTxTimeout)
	limitsdoc.Duration(t, "ax.pg-statement-timeout", statementTimeout)
	limitsdoc.Duration(t, "ax.lease-write-lock-timeout", writeLockTimeout)
	limitsdoc.Duration(t, "ax.lease-take-lock-timeout", takeLockTimeout)
	limitsdoc.Duration(t, "ax.lease-take-statement-timeout", takeStatementTimeout)
	limitsdoc.Duration(t, "ax.sqlite-busy-timeout", sqliteBusyTimeout)
	limitsdoc.Count(t, "ax.list-page-default", defaultPageSize)
	limitsdoc.Count(t, "ax.list-page-max", maxPageSize)
	limitsdoc.Count(t, "ax.list-history", defaultHistoryLength)
}
