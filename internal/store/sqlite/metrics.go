package sqlite

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// RegisterMetrics exposes both pool statistics and durable backlog gauges.
// Queries use a short deadline and return invalid metrics on failure, rather than false zeros.
func (s *Store) RegisterMetrics(reg prometheus.Registerer) error { return s.a.registerMetrics(reg) }

func (s *adapter) registerMetrics(reg prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{collectors.NewDBStatsCollector(s.read, "read"), collectors.NewDBStatsCollector(s.write, "write"), &backlogCollector{s: s}} {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
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
	var replies, oldestReply, outbox, oldestOutbox, interactions int64
	err := c.s.read.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM reply_queue WHERE delivered_at IS NULL),
 (SELECT COALESCE(MIN(enqueued_at),0) FROM reply_queue WHERE delivered_at IS NULL),
 (SELECT COUNT(*) FROM outbox WHERE message_ref = '' AND abandoned_at = 0),
 (SELECT COALESCE(MIN(created_at),0) FROM outbox WHERE message_ref = '' AND abandoned_at = 0),
 (SELECT COUNT(*) FROM interactions WHERE state = 'pending')`).Scan(&replies, &oldestReply, &outbox, &oldestOutbox, &interactions)
	ds := backlogDescriptors()
	if err != nil {
		for _, d := range ds {
			ch <- prometheus.NewInvalidMetric(d, errors.New("durable backlog query failed"))
		}
		return
	}
	now := time.Now()
	age := func(at int64) float64 {
		if at == 0 {
			return 0
		}
		return max(now.Sub(fromTS(at)).Seconds(), 0)
	}
	values := []float64{float64(replies), age(oldestReply), float64(outbox), age(oldestOutbox), float64(interactions)}
	for i, d := range ds {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, values[i])
	}
}
