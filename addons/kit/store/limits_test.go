package store

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
)

// TestLimitsPage holds the store's limits, which every add-on's store has, to docs/reference/limits.md and the
// add-ons' reference pages: the server-side timeouts each Postgres connection starts with unless the DSN sets them, the
// election's lock and statement timeouts, the SQLite busy timeout, and ListTasks's page sizes and the history it keeps
// by default.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "kit.pg-idle-in-transaction", PostgresIdleInTxTimeout)
	limitsdoc.Duration(t, "kit.pg-statement-timeout", PostgresStatementTimeout)
	limitsdoc.Duration(t, "kit.lease-write-lock-timeout", LeaseWriteLockTimeout)
	limitsdoc.Duration(t, "kit.lease-take-lock-timeout", LeaseTakeLockTimeout)
	limitsdoc.Duration(t, "kit.lease-take-statement-timeout", LeaseTakeStatementTimeout)
	limitsdoc.Duration(t, "kit.sqlite-busy-timeout", SQLiteBusyTimeout)
	limitsdoc.Count(t, "kit.list-page-default", DefaultPageSize)
	limitsdoc.Count(t, "kit.list-page-max", MaxPageSize)
	limitsdoc.Count(t, "kit.list-history", DefaultHistoryLength)
}
