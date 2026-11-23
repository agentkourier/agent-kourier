package store

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/limitsdoc"
	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
)

// TestLimitsPage holds the store's limits to the documentation: the server-side timeouts each Postgres connection
// starts with unless the DSN sets them, the election's lock and statement timeouts and the SQLite busy timeout, which
// the kit's store holds for the agent, and ListTasks's page sizes and the history it keeps by default.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "ax.pg-idle-in-transaction", kitstore.PostgresIdleInTxTimeout)
	limitsdoc.Duration(t, "ax.pg-statement-timeout", kitstore.PostgresStatementTimeout)
	limitsdoc.Duration(t, "ax.lease-write-lock-timeout", kitstore.LeaseWriteLockTimeout)
	limitsdoc.Duration(t, "ax.lease-take-lock-timeout", kitstore.LeaseTakeLockTimeout)
	limitsdoc.Duration(t, "ax.lease-take-statement-timeout", kitstore.LeaseTakeStatementTimeout)
	limitsdoc.Duration(t, "ax.sqlite-busy-timeout", kitstore.SQLiteBusyTimeout)
	limitsdoc.Count(t, "ax.list-page-default", kitstore.DefaultPageSize)
	limitsdoc.Count(t, "ax.list-page-max", kitstore.MaxPageSize)
	limitsdoc.Count(t, "ax.list-history", kitstore.DefaultHistoryLength)
}
