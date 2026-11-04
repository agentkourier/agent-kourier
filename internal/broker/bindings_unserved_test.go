package broker

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// reporting is a Snapshots that also reports what its source refused, as a Reloader does, and counts what is asked of it.
type reporting struct {
	snaps
	report  config.Report
	reports atomic.Int32
	reads   atomic.Int32 // calls of Current
}

func (r *reporting) Report() config.Report { r.reports.Add(1); return r.report }

func (r *reporting) Current() *config.Snapshot { r.reads.Add(1); return r.snaps.Current() }

// ThreadTTLsAndUnserved is the Bindings that are served with their threadTTL and, from the same snapshot, those that exist
// and are not routed, which the retention skips. A Snapshots that cannot report, a Reloader of files in effect, has none.
func TestThreadTTLsAndUnservedComeFromOneReport(t *testing.T) {
	f := newFixture(t)
	f.snaps.p.Store(snapshot(t, f.url, "kagent-v1", 48*time.Hour))
	ttls, unserved, ok := f.b.ThreadTTLsAndUnserved()
	if !ok || len(ttls) != 1 || len(unserved) != 0 {
		t.Errorf("Snapshots that cannot report: %v, %v, %v; want one Binding and none unserved", ttls, unserved, ok)
	}

	r := &reporting{report: config.Report{Snapshot: snapshot(t, f.url, "kagent-v1", 48*time.Hour), Rejections: []config.Rejection{
		{Namespace: "sre", Name: "left-out", Reason: config.ReasonBackendNotFound},
		{Namespace: "sre", Name: "flagged", Reason: config.ReasonInvalidSpec, Served: 3},
	}}}
	b, err := NewBindings(r, f.reader, f.audit, Options{OnRejected: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	ttls, unserved, ok = b.ThreadTTLsAndUnserved()
	if !ok || !slices.Equal(unserved, []string{"sre/left-out"}) || ttls["sre/oncall"] != 48*time.Hour {
		t.Errorf("ThreadTTLsAndUnserved = %v, %v, %v; want sre/oncall at 48h and only the Binding that is not served", ttls, unserved, ok)
	}
	if r.reports.Load() != 1 || r.reads.Load() != 0 {
		t.Errorf("%d reports and %d reads of Current; want the one report and nothing else: two reads could straddle a reload", r.reports.Load(), r.reads.Load())
	}

	empty := &reporting{} // before the first load
	b2, err := NewBindings(empty, f.reader, f.audit, Options{OnRejected: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b2.Close)
	if _, _, ok := b2.ThreadTTLsAndUnserved(); ok {
		t.Error("ThreadTTLsAndUnserved is ready before the first load")
	}
}
