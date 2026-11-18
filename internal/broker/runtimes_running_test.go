package broker

import (
	"slices"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// A connection is running while its runtime is in the table, and after the runtime is taken out until its run has returned, since a run
// that is stopping may still write (a Microsoft Teams receiver drains what it answered). One taken out that never ran is not running.
func TestARuntimeTakenOutIsRunningUntilItsRunReturns(t *testing.T) {
	table := newRuntimeTable()
	if table.running(teamConn) {
		t.Fatal("a connection with no runtime is running")
	}
	rt := &connRuntime{name: teamConn, done: make(chan struct{})}
	rt.ran.Store(true)
	table.add(rt)
	if !table.running(teamConn) {
		t.Error("a connection whose runtime is in the table is not running")
	}
	table.remove(rt)
	if !table.running(teamConn) {
		t.Error("a runtime taken out whose run has not returned is not running")
	}
	close(rt.done)
	if table.running(teamConn) {
		t.Error("a runtime whose run has returned is still running")
	}

	never := &connRuntime{name: teamConn, done: make(chan struct{})}
	table.add(never)
	table.remove(never)
	if table.running(teamConn) {
		t.Error("a runtime taken out that never ran is running")
	}

	// Restarts do not pile up the runtimes taken out: one whose run has returned is forgotten at the next remove.
	for range 3 {
		old := &connRuntime{name: teamConn, done: make(chan struct{})}
		old.ran.Store(true)
		table.add(old)
		table.remove(old)
		close(old.done)
	}
	table.mu.Lock()
	kept := len(table.ending[teamConn])
	table.mu.Unlock()
	if kept > 1 {
		t.Errorf("%d runtimes whose run has returned are kept; want at most the last", kept)
	}
}

// The leader's record asks the runtime table: a deleted connection whose runtime the supervisor has not stopped, or whose run is still
// draining, is not pruned, and it is once the run has returned.
func TestTheLeadersRecordHoldsADeletedConnectionWhileItsRunGoes(t *testing.T) {
	table := newRuntimeTable()
	rt := &connRuntime{name: teamConn, done: make(chan struct{})}
	rt.ran.Store(true)
	table.add(rt)
	record := &recordedServed{set: []string{ownConn, teamConn}}
	served := leaderServedRecord(record, namedBindings{ownConn: {sreAlerts}}.of, table)
	rep := config.Report{Snapshot: servedSnapshot(t, ownServed())}
	for _, step := range []struct {
		name string
		do   func()
		want []string
	}{
		{"in the table", func() {}, []string{ownConn, teamConn}},
		{"taken out, its run draining", func() { table.remove(rt) }, []string{ownConn, teamConn}},
		{"its run returned", func() { close(rt.done) }, []string{ownConn}},
	} {
		step.do()
		if _, err := served.update(t.Context(), rep); err != nil {
			t.Fatalf("%s: update: %v", step.name, err)
		}
		if got := record.recorded(); !slices.Equal(got, step.want) {
			t.Errorf("%s: the record = %v; want %v", step.name, got, step.want)
		}
	}
	if pruned, n := record.prunedOf(); !slices.Equal(pruned, []string{teamConn}) || n != 1 {
		t.Errorf("deleted %v in %d calls; want the deleted connection's, once its run returned", pruned, n)
	}
}
