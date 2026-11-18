package broker

import (
	"slices"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// A connection is running while its runtime is in the table, and after the runtime is taken out until its run has returned, since a run
// that is stopping may still write (a Microsoft Teams receiver drains what it answered). One taken out that never ran, or whose run had
// returned already, is not running, and the table keeps nothing of it.
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
	close(rt.done) // as the run's goroutine does, and then tells the table
	table.ended(rt)
	if table.running(teamConn) || endingOf(table, teamConn) != 0 {
		t.Errorf("a runtime whose run has returned is running, or kept (%d)", endingOf(table, teamConn))
	}

	never := &connRuntime{name: teamConn, done: make(chan struct{})}
	table.add(never)
	table.remove(never)
	if table.running(teamConn) || endingOf(table, teamConn) != 0 {
		t.Error("a runtime taken out that never ran is running, or kept")
	}

	// A run that ended by itself (the table's fail took it out) and is taken out later is not kept: nothing would tell the table again.
	over := &connRuntime{name: teamConn, done: make(chan struct{})}
	over.ran.Store(true)
	table.add(over)
	close(over.done)
	table.ended(over)
	table.remove(over)
	if table.running(teamConn) || endingOf(table, teamConn) != 0 {
		t.Errorf("a runtime taken out after its run returned is running, or kept (%d)", endingOf(table, teamConn))
	}
}

// A connection that the supervisor stops and that still exists, whose deletion would never be asked about, leaves nothing in the table once
// its run has returned: a runtime taken out holds its credentials and its platform.
func TestAStoppedRuntimeLeavesNothingBehindOnceItsRunReturns(t *testing.T) {
	r := newHostRig(t, false)
	rt := r.start("stopped", chattest.NewFake(), false)
	r.host.stopRuntime(t.Context(), rt)
	testutil.Eventually(t, "the table forgets the stopped runtime", func() bool { return endingOf(r.host.table, rt.name) == 0 })
}

// endingOf is the number of runtimes taken out of table whose run it still counts as going.
func endingOf(table *runtimeTable, conn string) int {
	table.mu.Lock()
	defer table.mu.Unlock()
	return len(table.ending[conn])
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
		{"its run returned", func() { close(rt.done); table.ended(rt) }, []string{ownConn}},
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
