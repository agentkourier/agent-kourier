package sqlite

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// RegisterMetrics exposes both pool statistics and durable backlog gauges. connections, if not nil, lists the ChatConnections ("namespace/name") the
// broker serves, read at each scrape: the outbox gauges say 0 for each that has no unposted row, and otherwise have a series for a connection only while
// rows name it.
// Queries use a short deadline and return invalid metrics on failure, rather than false zeros.
func (s *Store) RegisterMetrics(reg prometheus.Registerer, connections func() []string) error {
	return s.a.registerMetrics(reg, connections)
}

func (s *adapter) registerMetrics(reg prometheus.Registerer, connections func() []string) error {
	for _, c := range []prometheus.Collector{collectors.NewDBStatsCollector(s.read, "read"), collectors.NewDBStatsCollector(s.write, "write"), &backlogCollector{s: s, connections: connections}} {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
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
	age := func(at int64) float64 {
		if at == 0 {
			return 0
		}
		return max(now.Sub(fromTS(at)).Seconds(), 0)
	}
	c.collectQueues(ctx, ch, age)
	c.collectOutbox(ctx, ch, age)
}

func (c *backlogCollector) collectQueues(ctx context.Context, ch chan<- prometheus.Metric, age func(int64) float64) {
	var replies, oldestReply, interactions int64
	d := Dialect{}
	undelivered, undecided := store.UndeliveredReplies("").Text(d), store.PendingInteractions("").Text(d)
	err := c.s.read.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM reply_queue WHERE `+undelivered+`),
 (SELECT COALESCE(MIN(enqueued_at),0) FROM reply_queue WHERE `+undelivered+`),
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
func (c *backlogCollector) collectOutbox(ctx context.Context, ch chan<- prometheus.Metric, age func(int64) float64) {
	ds := outboxDescriptors()
	fail := func() {
		for _, d := range ds {
			ch <- prometheus.NewInvalidMetric(d, errors.New("durable backlog query failed"))
		}
	}
	pending := store.PendingOutbox("").Text(Dialect{})
	//nolint:gosec // G202: the predicate is rendered by store.PendingOutbox for this dialect; no value is concatenated.
	rows, err := c.s.read.QueryContext(ctx, `SELECT connection, COUNT(*), MIN(created_at) FROM outbox WHERE `+pending+` GROUP BY connection`)
	if err != nil {
		fail()
		return
	}
	defer func() { _ = rows.Close() }()
	var out []prometheus.Metric // held until the whole result is read, so that a failure part way reports no partial result
	seen := map[string]bool{}
	for rows.Next() {
		var connection string
		var n, oldest int64
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
