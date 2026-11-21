package store

import (
	"testing"

	"github.com/agentkourier/agent-kourier/addons/cma/internal/limitsdoc"
)

// TestLimitsPage holds ListTasks's page sizes and the history it keeps by default to docs/reference/limits.md.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "cma.list-page-default", defaultPageSize)
	limitsdoc.Count(t, "cma.list-page-max", maxPageSize)
	limitsdoc.Count(t, "cma.list-history", defaultHistoryLength)
}
