package broker

import (
	"slices"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// reporting is a Snapshots that also reports what its source refused, as a Reloader does.
type reporting struct {
	snaps
	report config.Report
}

func (r *reporting) Report() config.Report { return r.report }

// Unserved is the Bindings that exist and are not routed, which the retention skips. A Snapshots that cannot report, a
// Reloader of files in effect, has none.
func TestUnservedAreTheBindingsTheReportLeavesOut(t *testing.T) {
	f := newFixture(t)
	if got := f.b.Unserved(); len(got) != 0 {
		t.Errorf("Unserved of Snapshots that cannot report = %v; want none", got)
	}

	r := &reporting{report: config.Report{Rejections: []config.Rejection{
		{Namespace: "sre", Name: "left-out", Reason: config.ReasonBackendNotFound},
		{Namespace: "sre", Name: "flagged", Reason: config.ReasonInvalidSpec, Served: 3},
	}}}
	b, err := NewBindings(r, f.reader, f.audit, Options{OnRejected: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	if got := b.Unserved(); !slices.Equal(got, []string{"sre/left-out"}) {
		t.Errorf("Unserved = %v; want only the Binding that is not served", got)
	}
}
