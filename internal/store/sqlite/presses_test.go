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
	// The purge of delivery keys reads by when the press was recorded, and the look-up of a key is the primary key's.
	for query, index := range map[string]string{
		`EXPLAIN QUERY PLAN DELETE FROM press_deliveries WHERE pressed_at <= 0`:                           "press_deliveries_pressed",
		`EXPLAIN QUERY PLAN SELECT 1 FROM press_deliveries WHERE connection = 'c' AND delivery_key = 'k'`: "sqlite_autoindex_press_deliveries_1",
	} {
		if err := s.a.read.QueryRowContext(ctx, query).Scan(new(int), new(int), new(int), &plan); err != nil || !strings.Contains(plan, index) {
			t.Errorf("%s: query plan = %q, %v; want it to use %s", query, plan, err, index)
		}
	}
}
