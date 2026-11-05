package postgres

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the limits of the Postgres store. The broker sets none of the
// options that change them, so the defaults are the values.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "pgConns", int(defaultMaxConns))
	limitsdoc.Duration(t, "pgStatement", defaultStatementTimeout)
	limitsdoc.Duration(t, "pgIdleInTx", defaultIdleInTxTimeout)
}
