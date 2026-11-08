package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// RegisterMetrics exposes the pool's statistics and the durable backlog gauges, under the names the
// SQLite store gives the gauges, so one dashboard reads either, and agentkourier_store_deposed_total, which only this store has. The queries use a short deadline and
// report invalid metrics on failure, rather than false zeros.
func (s *Store) RegisterMetrics(reg prometheus.Registerer, connections func() []string) error {
	return s.a.registerMetrics(reg, connections)
}

func (s *adapter) registerMetrics(reg prometheus.Registerer, connections func() []string) error {
	for _, c := range []prometheus.Collector{&poolCollector{s: s}, &backlogCollector{s: s, connections: connections}, s.deposed} {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}

type poolCollector struct{ s *adapter }

// poolMetric is one series of the pool's statistics.
type poolMetric struct {
	desc  *prometheus.Desc
	kind  prometheus.ValueType
	value func(*pgxpool.Stat) float64
}

func poolMetrics() []poolMetric {
	d := func(name, help string) *prometheus.Desc { return prometheus.NewDesc(name, help, nil, nil) }
	return []poolMetric{
		{d("agentkourier_store_pool_acquired_conns", "Connections of the store's pool in use."), prometheus.GaugeValue,
			func(st *pgxpool.Stat) float64 { return float64(st.AcquiredConns()) }},
		{d("agentkourier_store_pool_idle_conns", "Idle connections of the store's pool."), prometheus.GaugeValue,
			func(st *pgxpool.Stat) float64 { return float64(st.IdleConns()) }},
		{d("agentkourier_store_pool_conns", "Connections of the store's pool, in use, idle or being made."), prometheus.GaugeValue,
			func(st *pgxpool.Stat) float64 { return float64(st.TotalConns()) }},
		{d("agentkourier_store_pool_max_conns", "Largest size of the store's pool."), prometheus.GaugeValue,
			func(st *pgxpool.Stat) float64 { return float64(st.MaxConns()) }},
		{d("agentkourier_store_pool_acquires_total", "Connections taken from the store's pool."), prometheus.CounterValue,
			func(st *pgxpool.Stat) float64 { return float64(st.AcquireCount()) }},
		{d("agentkourier_store_pool_empty_acquires_total", "Takes that had to wait for a connection."), prometheus.CounterValue,
			func(st *pgxpool.Stat) float64 { return float64(st.EmptyAcquireCount()) }},
		{d("agentkourier_store_pool_canceled_acquires_total", "Takes that a context ended before they got a connection."), prometheus.CounterValue,
			func(st *pgxpool.Stat) float64 { return float64(st.CanceledAcquireCount()) }},
		{d("agentkourier_store_pool_acquire_seconds_total", "Time spent taking connections from the store's pool."), prometheus.CounterValue,
			func(st *pgxpool.Stat) float64 { return st.AcquireDuration().Seconds() }},
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range poolMetrics() {
		ch <- m.desc
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	st := c.s.pool.Stat()
	for _, m := range poolMetrics() {
		ch <- prometheus.MustNewConstMetric(m.desc, m.kind, m.value(st))
	}
}

type backlogCollector struct {
	s *adapter
	// connections are the ChatConnections the broker serves (RegisterMetrics); the outbox gauges say 0 for each that has no unposted row. Nil is none.
	connections func() []string
}

// queueDescriptors are the backlogs that no connection splits: the reply queue and the interactions.
func queueDescriptors() []*prometheus.Desc {
	return []*prometheus.Desc{
		prometheus.NewDesc("agentkourier_reply_queue_depth", "Undelivered replies in the durable queue.", nil, nil),
		prometheus.NewDesc("agentkourier_reply_queue_oldest_age_seconds", "Age of the oldest undelivered reply.", nil, nil),
		prometheus.NewDesc("agentkourier_interactions_pending", "Durable interactions awaiting a decision.", nil, nil),
	}
}

// outboxDescriptors are the outbox's backlog gauges, one series for each connection that has an unposted row (store.OutboxEntry.Connection;
// empty for a row written before the broker recorded connections, which the leader attributes to its own connection when it starts).
func outboxDescriptors() []*prometheus.Desc {
	return []*prometheus.Desc{
		prometheus.NewDesc("agentkourier_outbox_pending", "Unposted, nonabandoned durable outbound rows, including streams, by connection.", []string{telemetry.ConnectionLabel}, nil),
		prometheus.NewDesc("agentkourier_outbox_oldest_age_seconds", "Age of the oldest unposted, nonabandoned outbound row, by connection.", []string{telemetry.ConnectionLabel}, nil),
	}
}

func (c *backlogCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range append(queueDescriptors(), outboxDescriptors()...) {
		ch <- d
	}
}

func (c *backlogCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	now := time.Now()
	age := func(at *time.Time) float64 {
		if at == nil {
			return 0
		}
		return max(now.Sub(*at).Seconds(), 0)
	}
	c.collectQueues(ctx, ch, age)
	c.collectOutbox(ctx, ch, age)
}

func (c *backlogCollector) collectQueues(ctx context.Context, ch chan<- prometheus.Metric, age func(*time.Time) float64) {
	var replies, interactions int64
	var oldestReply *time.Time
	d := Dialect{}
	undelivered, undecided := store.UndeliveredReplies("").Text(d), store.PendingInteractions("").Text(d)
	err := c.s.pool.QueryRow(ctx, `SELECT
 (SELECT COUNT(*) FROM reply_queue WHERE `+undelivered+`),
 (SELECT MIN(enqueued_at) FROM reply_queue WHERE `+undelivered+`),
 (SELECT COUNT(*) FROM interactions WHERE `+undecided+`)`).Scan(&replies, &oldestReply, &interactions)
	ds := queueDescriptors()
	if err != nil {
		for _, d := range ds {
			ch <- prometheus.NewInvalidMetric(d, errors.New("durable backlog query failed"))
		}
		return
	}
	for i, v := range []float64{float64(replies), age(oldestReply), float64(interactions)} {
		ch <- prometheus.MustNewConstMetric(ds[i], prometheus.GaugeValue, v)
	}
}

// collectOutbox reports the unposted rows of each connection, from one grouped query. A connection that the broker serves (c.connections) and that has
// none says 0, so that it always has a series; one it does not serve has a series only while rows name it, and none once they are posted or abandoned.
func (c *backlogCollector) collectOutbox(ctx context.Context, ch chan<- prometheus.Metric, age func(*time.Time) float64) {
	ds := outboxDescriptors()
	fail := func() {
		for _, d := range ds {
			ch <- prometheus.NewInvalidMetric(d, errors.New("durable backlog query failed"))
		}
	}
	pending := store.PendingOutbox("").Text(Dialect{})
	rows, err := c.s.pool.Query(ctx, `SELECT connection, COUNT(*), MIN(created_at) FROM outbox WHERE `+pending+` GROUP BY connection`)
	if err != nil {
		fail()
		return
	}
	defer rows.Close()
	var out []prometheus.Metric // held until the whole result is read, so that a failure part way reports no partial result
	seen := map[string]bool{}
	for rows.Next() {
		var connection string
		var n int64
		var oldest *time.Time
		if err := rows.Scan(&connection, &n, &oldest); err != nil {
			fail()
			return
		}
		seen[connection] = true
		out = append(out, prometheus.MustNewConstMetric(ds[0], prometheus.GaugeValue, float64(n), connection),
			prometheus.MustNewConstMetric(ds[1], prometheus.GaugeValue, age(oldest), connection))
	}
	if rows.Err() != nil {
		fail()
		return
	}
	if c.connections != nil {
		for _, connection := range c.connections() {
			if !seen[connection] {
				seen[connection] = true
				out = append(out, prometheus.MustNewConstMetric(ds[0], prometheus.GaugeValue, 0, connection),
					prometheus.MustNewConstMetric(ds[1], prometheus.GaugeValue, 0, connection))
			}
		}
	}
	for _, m := range out {
		ch <- m
	}
}
