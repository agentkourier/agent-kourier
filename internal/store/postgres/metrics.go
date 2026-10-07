package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// RegisterMetrics exposes the pool's statistics and the durable backlog gauges, under the names the
// SQLite store gives the gauges, so one dashboard reads either, and courier_store_deposed_total, which only this store has. The queries use a short deadline and
// report invalid metrics on failure, rather than false zeros.
func (s *Store) RegisterMetrics(reg prometheus.Registerer) error { return s.a.registerMetrics(reg) }

func (s *adapter) registerMetrics(reg prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{&poolCollector{s: s}, &backlogCollector{s: s}, s.deposed} {
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
		{d("courier_store_pool_acquired_conns", "Connections of the store's pool in use."), prometheus.GaugeValue,
			func(st *pgxpool.Stat) float64 { return float64(st.AcquiredConns()) }},
		{d("courier_store_pool_idle_conns", "Idle connections of the store's pool."), prometheus.GaugeValue,
			func(st *pgxpool.Stat) float64 { return float64(st.IdleConns()) }},
		{d("courier_store_pool_conns", "Connections of the store's pool, in use, idle or being made."), prometheus.GaugeValue,
			func(st *pgxpool.Stat) float64 { return float64(st.TotalConns()) }},
		{d("courier_store_pool_max_conns", "Largest size of the store's pool."), prometheus.GaugeValue,
			func(st *pgxpool.Stat) float64 { return float64(st.MaxConns()) }},
		{d("courier_store_pool_acquires_total", "Connections taken from the store's pool."), prometheus.CounterValue,
			func(st *pgxpool.Stat) float64 { return float64(st.AcquireCount()) }},
		{d("courier_store_pool_empty_acquires_total", "Takes that had to wait for a connection."), prometheus.CounterValue,
			func(st *pgxpool.Stat) float64 { return float64(st.EmptyAcquireCount()) }},
		{d("courier_store_pool_canceled_acquires_total", "Takes that a context ended before they got a connection."), prometheus.CounterValue,
			func(st *pgxpool.Stat) float64 { return float64(st.CanceledAcquireCount()) }},
		{d("courier_store_pool_acquire_seconds_total", "Time spent taking connections from the store's pool."), prometheus.CounterValue,
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

type backlogCollector struct{ s *adapter }

func backlogDescriptors() []*prometheus.Desc {
	return []*prometheus.Desc{
		prometheus.NewDesc("courier_reply_queue_depth", "Undelivered replies in the durable queue.", nil, nil),
		prometheus.NewDesc("courier_reply_queue_oldest_age_seconds", "Age of the oldest undelivered reply.", nil, nil),
		prometheus.NewDesc("courier_outbox_pending", "Unposted, nonabandoned durable outbound rows, including streams.", nil, nil),
		prometheus.NewDesc("courier_outbox_oldest_age_seconds", "Age of the oldest unposted, nonabandoned outbound row.", nil, nil),
		prometheus.NewDesc("courier_interactions_pending", "Durable interactions awaiting a decision.", nil, nil),
	}
}

func (c *backlogCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range backlogDescriptors() {
		ch <- d
	}
}

func (c *backlogCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var replies, outbox, interactions int64
	var oldestReply, oldestOutbox *time.Time
	d := Dialect{}
	undelivered, pending, undecided := store.UndeliveredReplies("").Text(d), store.PendingOutbox("").Text(d), store.PendingInteractions("").Text(d)
	err := c.s.pool.QueryRow(ctx, `SELECT
 (SELECT COUNT(*) FROM reply_queue WHERE `+undelivered+`),
 (SELECT MIN(enqueued_at) FROM reply_queue WHERE `+undelivered+`),
 (SELECT COUNT(*) FROM outbox WHERE `+pending+`),
 (SELECT MIN(created_at) FROM outbox WHERE `+pending+`),
 (SELECT COUNT(*) FROM interactions WHERE `+undecided+`)`).Scan(&replies, &oldestReply, &outbox, &oldestOutbox, &interactions)
	ds := backlogDescriptors()
	if err != nil {
		for _, d := range ds {
			ch <- prometheus.NewInvalidMetric(d, errors.New("durable backlog query failed"))
		}
		return
	}
	now := time.Now()
	age := func(at *time.Time) float64 {
		if at == nil {
			return 0
		}
		return max(now.Sub(*at).Seconds(), 0)
	}
	values := []float64{float64(replies), age(oldestReply), float64(outbox), age(oldestOutbox), float64(interactions)}
	for i, d := range ds {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, values[i])
	}
}
