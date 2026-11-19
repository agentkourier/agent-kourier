package telemetry

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// webhookSeries returns the series of agentkourier_webhook_requests_total in reg, as "outcome reason connection" and its count.
func webhookSeries(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "agentkourier_webhook_requests_total" {
			continue
		}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			out[labels["outcome"]+" "+labels["reason"]+" "+labels[ConnectionLabel]] = m.GetCounter().GetValue()
		}
	}
	return out
}

// A webhook connection's recorder is bound to its connection when it is made, and counts into the broker's registry with no context
// of the request or of a Run: Init makes a series exist at zero, and Count adds one to it.
func TestTheWebhookRecorderCountsByOutcomeReasonAndConnection(t *testing.T) {
	ctx, _, reg := traced(t)
	rec := WebhookRecorder(ctx, "ns/hooks")
	rec.Init("accepted", "none")
	rec.Init("unverified", "stale")
	if got, want := webhookSeries(t, reg), map[string]float64{"accepted none ns/hooks": 0, "unverified stale ns/hooks": 0}; len(got) != len(want) ||
		got["accepted none ns/hooks"] != 0 || got["unverified stale ns/hooks"] != 0 {
		t.Fatalf("after Init the series are %v, want %v", got, want)
	}
	rec.Count("accepted", "none")
	rec.Count("accepted", "none")
	rec.Init("accepted", "none") // a second Init leaves the count as it is
	rec.Count("unmapped", "no_thread")
	got := webhookSeries(t, reg)
	for series, want := range map[string]float64{"accepted none ns/hooks": 2, "unverified stale ns/hooks": 0, "unmapped no_thread ns/hooks": 1} {
		if got[series] != want {
			t.Errorf("%s = %v, want %v (all: %v)", series, got[series], want, got)
		}
	}
}

// The help texts of the webhook connection's histogram and gauge, as /metrics writes them.
const (
	webhookAckHelp = "# HELP agentkourier_webhook_ack_duration_seconds " + webhookAckDurationHelp + "\n" +
		"# TYPE agentkourier_webhook_ack_duration_seconds histogram\n"
	webhookLastHelp = "# HELP agentkourier_webhook_last_accepted_timestamp_seconds " + webhookLastAcceptedHelp + "\n" +
		"# TYPE agentkourier_webhook_last_accepted_timestamp_seconds gauge\n"
)

// histogramText is the exposition of one series of agentkourier_webhook_ack_duration_seconds under labels, with the shared
// duration buckets, for samples (in seconds).
func histogramText(labels string, samples ...float64) string {
	var b strings.Builder
	sum := 0.0
	for _, s := range samples {
		sum += s
	}
	for _, le := range append(durationBuckets(), math.Inf(1)) {
		n := 0
		for _, s := range samples {
			if s <= le {
				n++
			}
		}
		fmt.Fprintf(&b, "agentkourier_webhook_ack_duration_seconds_bucket{%s,le=%q} %d\n", labels, strconv.FormatFloat(le, 'g', -1, 64), n)
	}
	fmt.Fprintf(&b, "agentkourier_webhook_ack_duration_seconds_sum{%s} %v\n", labels, sum)
	fmt.Fprintf(&b, "agentkourier_webhook_ack_duration_seconds_count{%s} %d\n", labels, len(samples))
	return b.String()
}

// The recorder observes each answer's duration on agentkourier_webhook_ack_duration_seconds, a histogram by outcome and connection on
// the shared duration buckets (which put the 2 s wait for the broker past the 1 s bound and within the 2.5 s one), whose series exist
// once an outcome is first observed: Init makes none.
func TestTheWebhookRecorderObservesTheAckDurationByOutcome(t *testing.T) {
	ctx, _, reg := traced(t)
	rec := WebhookRecorder(ctx, "ns/hooks")
	rec.Init("accepted", "none")
	if n, err := testutil.GatherAndCount(reg, "agentkourier_webhook_ack_duration_seconds"); err != nil || n != 0 {
		t.Fatalf("after Init the histogram has %d series (%v), want none before an answer", n, err)
	}
	rec.Observe("accepted", 25*time.Millisecond)
	rec.Observe("accepted", 500*time.Millisecond)
	rec.Observe("timeout", 2*time.Second)
	want := webhookAckHelp + histogramText(`connection="ns/hooks",outcome="accepted"`, 0.025, 0.5) +
		histogramText(`connection="ns/hooks",outcome="timeout"`, 2)
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "agentkourier_webhook_ack_duration_seconds"); err != nil {
		t.Error(err)
	}
}

// The recorder sets agentkourier_webhook_last_accepted_timestamp_seconds, a gauge by connection, to the Unix time of the last
// accepted request; the series does not exist before the first, and neither Init nor Count makes it.
func TestTheWebhookRecorderSetsTheLastAcceptedTime(t *testing.T) {
	ctx, _, reg := traced(t)
	rec := WebhookRecorder(ctx, "ns/hooks")
	for _, s := range [][2]string{{"accepted", "none"}, {"self", "none"}, {"unmapped", "no_thread"}} {
		rec.Init(s[0], s[1])
		rec.Count(s[0], s[1])
	}
	if n, err := testutil.GatherAndCount(reg, "agentkourier_webhook_last_accepted_timestamp_seconds"); err != nil || n != 0 {
		t.Fatalf("the gauge has %d series (%v) before an accepted request, want none", n, err)
	}
	rec.Accepted(time.Unix(1_800_000_000, 500_000_000))
	rec.Accepted(time.Unix(1_800_000_060, 250_000_000))
	want := webhookLastHelp + `agentkourier_webhook_last_accepted_timestamp_seconds{connection="ns/hooks"} 1.80000006025e+09` + "\n"
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "agentkourier_webhook_last_accepted_timestamp_seconds"); err != nil {
		t.Error(err)
	}
}

// The recorder's series are the connection's: ForgetConnection deletes them with its other series.
func TestForgetConnectionDeletesTheWebhookSeries(t *testing.T) {
	ctx, _, reg := traced(t)
	for _, conn := range []string{"ns/kept", "ns/gone"} {
		rec := WebhookRecorder(ctx, conn)
		rec.Count("accepted", "none")
		rec.Observe("accepted", time.Millisecond)
		rec.Accepted(time.Unix(1_800_000_000, 0))
	}
	ForgetConnection(ctx, "ns/gone")
	for _, name := range []string{"agentkourier_webhook_requests_total", "agentkourier_webhook_ack_duration_seconds",
		"agentkourier_webhook_last_accepted_timestamp_seconds"} {
		if got := seriesOf(t, reg, name); !slices.Equal(got, []string{"ns/kept"}) {
			t.Errorf("the series of %s after the removal are of %v, want only ns/kept", name, got)
		}
	}
}

// Without the broker's instruments in its context, the recorder records nothing, and does not fail.
func TestAWebhookRecorderWithoutAnObserverRecordsNothing(t *testing.T) {
	rec := WebhookRecorder(t.Context(), "ns/hooks")
	rec.Init("accepted", "none")
	rec.Count("accepted", "none")
	rec.Observe("accepted", time.Millisecond)
	rec.Accepted(time.Unix(1_800_000_000, 0))
	var zero WebhookRequests
	zero.Init("accepted", "none")
	zero.Count("accepted", "none")
	zero.Observe("accepted", time.Millisecond)
	zero.Accepted(time.Unix(1_800_000_000, 0))
}
