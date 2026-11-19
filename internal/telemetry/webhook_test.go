package telemetry

import (
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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

// The recorder's series are the connection's: ForgetConnection deletes them with its other series.
func TestForgetConnectionDeletesTheWebhookSeries(t *testing.T) {
	ctx, _, reg := traced(t)
	WebhookRecorder(ctx, "ns/kept").Count("accepted", "none")
	WebhookRecorder(ctx, "ns/gone").Init("accepted", "none")
	ForgetConnection(ctx, "ns/gone")
	if got := seriesOf(t, reg, "agentkourier_webhook_requests_total"); !slices.Equal(got, []string{"ns/kept"}) {
		t.Errorf("the series after the removal are of %v, want only ns/kept", got)
	}
}

// Without the broker's instruments in its context, the recorder records nothing, and does not fail.
func TestAWebhookRecorderWithoutAnObserverRecordsNothing(t *testing.T) {
	rec := WebhookRecorder(t.Context(), "ns/hooks")
	rec.Init("accepted", "none")
	rec.Count("accepted", "none")
	var zero WebhookRequests
	zero.Init("accepted", "none")
	zero.Count("accepted", "none")
}
