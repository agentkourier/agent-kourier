package sqlite

import (
	"database/sql"
	"strings"
	"testing"
)

// Deleted content is overwritten: secure_delete is on for every connection, and the lookup the
// sweep makes by interaction has its index.
func TestSecureDeleteAndThePendingIndex(t *testing.T) {
	ctx := t.Context()
	s := openTemp(t)
	for name, db := range map[string]*sql.DB{"write": s.a.write, "read": s.a.read} {
		var on int
		if err := db.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&on); err != nil || on != 1 {
			t.Errorf("%s pool: secure_delete = %d, %v; want 1", name, on, err)
		}
	}
	var plan string
	if err := s.a.read.QueryRowContext(ctx, `EXPLAIN QUERY PLAN SELECT 1 FROM interaction_presses WHERE interaction_id = ? AND pressed_at <= ?`, "x", 0).Scan(new(int), new(int), new(int), &plan); err != nil || !strings.Contains(plan, "interaction_presses_interaction") {
		t.Errorf("query plan = %q, %v; want it to use interaction_presses_interaction", plan, err)
	}
}
