package sqlite

import (
	"strings"
	"testing"
)

// TestUnattributedRowsCountsTheOutboxByTheThreadIndex: the count of outbox rows with no connection searches outbox_thread by its
// first column, and never reads the whole table, which holds a row for every post until its session is deleted.
func TestUnattributedRowsCountsTheOutboxByTheThreadIndex(t *testing.T) {
	s := openTemp(t)
	plan := strings.Join(queryPlan(t, s, unattributedOutbox, nil), "\n")
	if !strings.Contains(plan, "USING INDEX outbox_thread") && !strings.Contains(plan, "USING COVERING INDEX outbox_thread") {
		t.Errorf("the count of outbox rows with no connection does not use outbox_thread: %q", plan)
	}
}
