package sqlite

import (
	"strings"
	"testing"
)

// The queries of the ack path and of the digest loop use an index of trigger_runs and never read the whole table, which
// holds a row for each alert of the last day: the open window and the mark by Binding and trigger from the small
// index of stopped, undigested rows, the counts of starts from the index of starts, and the rows due from that small
// index too. A plan that scans the table fails here, so an index dropped or a query reworded to miss it is seen.
func TestTriggerRunQueriesUseAnIndex(t *testing.T) {
	s := openTemp(t)
	for _, q := range []struct {
		name, sql string
		args      []any
		by        string // what the search must be keyed on, if it is by Binding and trigger
	}{
		{"the open window of a stopped start", openWindow, []any{"b", 0, int64(1)}, "binding=? AND trigger_index=?"},
		{"the digests due", dueWindows, []any{int64(1)}, ""},
		{"the mark of a digested window", markDigested, []any{"b", 0, int64(1)}, "binding=? AND trigger_index=?"},
		{"the starts in a window", startsSince, []any{"b", 0, int64(1)}, ""},
		{"the running investigations", runningStarts, []any{"b", 0, int64(1)}, ""},
	} {
		rows, err := s.read.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+q.sql, q.args...)
		if err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		t.Logf("%s:\n  %s", q.name, strings.Join(plan, "\n  "))
		used := false
		for _, d := range plan {
			if strings.Contains(d, "trigger_runs") && !strings.Contains(d, "INDEX") && strings.HasPrefix(d, "SCAN") {
				t.Errorf("%s reads the whole table: %q", q.name, d)
			}
			used = used || strings.Contains(d, "INDEX trigger_runs_")
		}
		if q.by != "" && !strings.Contains(strings.Join(plan, "\n"), q.by) {
			t.Errorf("%s is not searched by %q: %q", q.name, q.by, plan)
		}
		if !used {
			t.Errorf("%s uses no index of trigger_runs: %q", q.name, plan)
		}
	}
}
