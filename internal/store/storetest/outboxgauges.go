package storetest

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// MetricsStore is a store that exports its backlog gauges, as the SQLite and Postgres stores do.
type MetricsStore interface {
	store.Store
	RegisterMetrics(prometheus.Registerer) error
}

// OutboxGaugesByConnection holds a store's agentkourier_outbox_pending and agentkourier_outbox_oldest_age_seconds to what they say: one
// series for each connection that has an unposted, nonabandoned row, with the count and the age of the oldest of that connection's rows, and
// none for a connection that has none. A row that records no connection is the series whose connection label is empty. s is a fresh store.
func OutboxGaugesByConnection(t *testing.T, s MetricsStore) {
	t.Helper()
	ctx := t.Context()
	reg := prometheus.NewRegistry()
	if err := s.RegisterMetrics(reg); err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}
	now := time.Now()
	for _, o := range []struct {
		id, connection string
		age            time.Duration
	}{
		{"a-old", "ns/a", 2 * time.Minute}, {"a-new", "ns/a", time.Minute}, {"a-posted", "ns/a", 10 * time.Minute}, {"a-abandoned", "ns/a", 10 * time.Minute},
		{"b-only", "ns/b", 30 * time.Second},
		{"c-posted", "ns/c", 5 * time.Minute},
		{"unattributed", "", 10 * time.Second},
	} {
		_, err := s.CreateOutbox(ctx, store.OutboxEntry{ID: o.id, EventID: o.id, Role: store.OutboxReply, Connection: o.connection, Channel: "C1", Thread: "1.1", CreatedAt: now.Add(-o.age), Payload: "{}"})
		noErr(t, err, "CreateOutbox "+o.id)
	}
	noErr(t, s.RecordMessageRef(ctx, "a-posted", "9.1"), "RecordMessageRef a-posted")
	noErr(t, s.RecordMessageRef(ctx, "c-posted", "9.2"), "RecordMessageRef c-posted")
	_, err := s.AbandonOutbox(ctx, "a-abandoned", now)
	noErr(t, err, "AbandonOutbox")

	// check reads both gauges: each connection with unposted rows has the count and age that want says (the age within a margin for the time the
	// test takes), and no other has a series.
	check := func(step string, want map[string]gaugeWant) {
		t.Helper()
		pending, age := gaugeByConnection(t, reg, "agentkourier_outbox_pending"), gaugeByConnection(t, reg, "agentkourier_outbox_oldest_age_seconds")
		if len(pending) != len(want) || len(age) != len(want) {
			t.Errorf("%s: outbox gauges have series for %v (pending) and %v (age); want exactly %d connections", step, pending, age, len(want))
		}
		for conn, w := range want {
			if got, ok := pending[conn]; !ok || got != w.pending {
				t.Errorf("%s: agentkourier_outbox_pending{connection=%q} = %v, present %v; want %v", step, conn, got, ok, w.pending)
			}
			if got, ok := age[conn]; !ok || got < w.age.Seconds() || got > (w.age+time.Minute).Seconds() {
				t.Errorf("%s: agentkourier_outbox_oldest_age_seconds{connection=%q} = %v, present %v; want %v and no more than a minute over", step, conn, got, ok, w.age.Seconds())
			}
		}
	}
	check("two connections and a row with none", map[string]gaugeWant{"ns/a": {2, 2 * time.Minute}, "ns/b": {1, 30 * time.Second}, "": {1, 10 * time.Second}})

	// A row that is posted leaves its connection's count, and a connection that has none left has no series.
	noErr(t, s.RecordMessageRef(ctx, "b-only", "9.3"), "RecordMessageRef b-only")
	noErr(t, s.RecordMessageRef(ctx, "a-old", "9.4"), "RecordMessageRef a-old")
	check("after b's row and a's oldest were posted", map[string]gaugeWant{"ns/a": {1, time.Minute}, "": {1, 10 * time.Second}})
}

// gaugeWant is what the outbox gauges of one connection should say: the unposted rows, and the age of the oldest at least.
type gaugeWant struct {
	pending float64
	age     time.Duration
}

// gaugeByConnection is the value of each series of the gauge name in reg, by its connection label ("" for a series with none).
func gaugeByConnection(t *testing.T, reg *prometheus.Registry, name string) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]float64{}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.Metric {
			conn := ""
			for _, l := range m.GetLabel() {
				if l.GetName() == "connection" {
					conn = l.GetValue()
				}
			}
			got[conn] = m.GetGauge().GetValue()
		}
	}
	return got
}
