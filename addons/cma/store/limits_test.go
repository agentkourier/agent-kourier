package store

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// TestLimitsPage holds the store's limits to docs/reference/limits.md: the server-side timeouts each Postgres connection
// starts with unless the DSN sets them, the election's lock and statement timeouts and the SQLite busy timeout, which
// the kit's store holds for the adapter, and ListTasks's page sizes and the history it keeps by default.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "cma.pg-idle-in-transaction", kitstore.PostgresIdleInTxTimeout)
	limitsdoc.Duration(t, "cma.pg-statement-timeout", kitstore.PostgresStatementTimeout)
	limitsdoc.Duration(t, "cma.lease-write-lock-timeout", kitstore.LeaseWriteLockTimeout)
	limitsdoc.Duration(t, "cma.lease-take-lock-timeout", kitstore.LeaseTakeLockTimeout)
	limitsdoc.Duration(t, "cma.lease-take-statement-timeout", kitstore.LeaseTakeStatementTimeout)
	limitsdoc.Duration(t, "cma.sqlite-busy-timeout", kitstore.SQLiteBusyTimeout)
	limitsdoc.Count(t, "cma.list-page-default", kitstore.DefaultPageSize)
	limitsdoc.Count(t, "cma.list-page-max", kitstore.MaxPageSize)
	limitsdoc.Count(t, "cma.list-history", kitstore.DefaultHistoryLength)
}
