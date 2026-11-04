package broker

import (
	"maps"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// A Binding that is in the cluster and not in the routing table (rejected, with no good generation to serve) has sessions that
// were made under a threadTTL this process does not know. They are not those of a Binding that was deleted, which fall to the
// Otherwise horizon, the longest threadTTL: they are skipped, with a horizon that no session is past, until the Binding is
// served again. With none unserved, as on the ConfigMap route, the horizons are what they were.
func TestRetentionSkipsTheSessionsOfAnUnservedBinding(t *testing.T) {
	grace, day := 90*24*time.Hour, 24*time.Hour
	for _, tc := range []struct {
		name     string
		unserved func() []string
		want     store.SessionHorizons
	}{
		{"none", nil, store.SessionHorizons{
			ByBinding: map[string]time.Time{"ns/long": sweepNow.Add(-30*day - grace), "ns/short": sweepNow.Add(-10*day - grace)},
			Otherwise: sweepNow.Add(-90*day - grace),
		}},
		{"one that is not served", func() []string { return []string{"ns/broken"} }, store.SessionHorizons{
			ByBinding: map[string]time.Time{"ns/long": sweepNow.Add(-30*day - grace), "ns/short": sweepNow.Add(-10*day - grace), "ns/broken": neverPast},
			Otherwise: sweepNow.Add(-90*day - grace),
		}},
		{"one that is also served keeps its own horizon", func() []string { return []string{"ns/short"} }, store.SessionHorizons{
			ByBinding: map[string]time.Time{"ns/long": sweepNow.Add(-30*day - grace), "ns/short": sweepNow.Add(-10*day - grace)},
			Otherwise: sweepNow.Add(-90*day - grace),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRetentionStore{}
			newSessionRetention(t, f, func(c *retentionConfig) { c.Unserved = tc.unserved }).purgeSessions(t.Context())
			calls := f.sessions()
			if len(calls) != 1 || !maps.EqualFunc(calls[0].horizons.ByBinding, tc.want.ByBinding, time.Time.Equal) || !calls[0].horizons.Otherwise.Equal(tc.want.Otherwise) {
				t.Errorf("PurgeExpiredSessions calls = %+v; want one with the horizons %+v", calls, tc.want)
			}
			if len(calls) == 1 && !calls[0].horizons.Valid() {
				t.Errorf("the horizons %+v are not valid for the store", calls[0].horizons)
			}
		})
	}
}

// The stale tasks of an unserved Binding are not listed as past their horizon either: the same horizons.
func TestRetentionSkipsTheStaleTasksOfAnUnservedBinding(t *testing.T) {
	g := newStaleRig(t, nil, func(c *retentionConfig) { c.Unserved = func() []string { return []string{"ns/broken"} } })
	g.r.endStaleTasks(t.Context())
	if len(g.f.staleLists) == 0 || !g.f.staleLists[0].horizons.ByBinding["ns/broken"].Equal(neverPast) {
		t.Errorf("ListStaleTasks calls = %+v; want the horizon of ns/broken to be that no session is past", g.f.staleLists)
	}
}
