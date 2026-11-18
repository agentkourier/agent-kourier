package teams

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// observed is a registry with the broker's instruments, and a context that carries them.
type observed struct {
	reg *prometheus.Registry
	ctx context.Context
}

func newObserved(t *testing.T) observed {
	t.Helper()
	reg := prometheus.NewRegistry()
	obs, err := telemetry.New(reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return observed{reg: reg, ctx: telemetry.WithObserver(t.Context(), obs)}
}

// value is the counter name's series with the label value and the test connection, or 0 if it has none.
func (o observed) value(t *testing.T, name, label string) float64 {
	t.Helper()
	mfs, err := o.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels[telemetry.ConnectionLabel] != testConn {
				continue
			}
			for k, v := range labels {
				if k != telemetry.ConnectionLabel && v == label {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// serveObserved runs rt's Receiver on a context that carries o's instruments until the test ends.
func (rt *receiverTest) serveObserved(t *testing.T, o observed) {
	t.Helper()
	ctx, cancel := context.WithCancel(o.ctx)
	done := make(chan error, 1)
	go func() { done <- rt.r.Serve(ctx, rt.sink) }()
	t.Cleanup(func() {
		if rt.sink.block != nil {
			select {
			case <-rt.sink.block:
			default:
				close(rt.sink.block)
			}
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Serve returned %v", err)
		}
	})
	testutil.Eventually(t, "the receiver takes requests", func() bool { return rt.r.live.Load() != nil })
}

// Each answer of the route is counted by its outcome, a refused token by its reason too, and an activity answered 503 as shed.
func TestReceiverCountsWhatItAnswers(t *testing.T) {
	o := newObserved(t)
	rt := newReceiverTest(t, false, nil)
	rt.sink.block = make(chan struct{})
	rt.serveObserved(t, o)

	plain := fixture(t, "message_plain")
	rt.post(t, plain, "")
	rt.post(t, plain, "")
	rt.post(t, plain, "Bearer nope")
	rt.post(t, []byte(`{"type":42}`), "")
	rt.post(t, editFixture(t, "message_plain", func(m map[string]any) {
		m["channelData"].(map[string]any)["tenant"] = map[string]any{"id": "99999999-0000-4000-8000-000000000000"}
		m["id"] = "1794960009999"
	}), "")
	next := rt.fillLane(t, "19:full@thread.tacv2", 1794960000000)
	if rec := rt.post(t, inChannel(t, "message_plain", "19:full@thread.tacv2", fmt.Sprint(next)), ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("past the lane: status %d", rec.Code)
	}

	for outcome, want := range map[string]float64{
		requestAccepted: 1 + channelBacklog, requestDuplicate: 1, requestBadToken: 1, requestMalformed: 1, requestWrongTenant: 1,
		requestShed: 1,
	} {
		if got := o.value(t, "agentkourier_teams_requests_total", outcome); got != want {
			t.Errorf("requests %s = %v, want %v", outcome, got, want)
		}
	}
	if got := o.value(t, "agentkourier_teams_tokens_refused_total", string(ReasonNoBearer)); got != 0 {
		t.Errorf("no_bearer = %v: the header had a bearer", got)
	}
	if got := o.value(t, "agentkourier_teams_tokens_refused_total", string(ReasonMalformed)); got != 1 {
		t.Errorf("tokens refused malformed = %v, want 1", got)
	}
	if got := o.value(t, "agentkourier_teams_activities_shed_total", shedChannelBacklog); got != 1 {
		t.Errorf("shed channel_backlog = %v, want 1", got)
	}
	if got := o.value(t, "agentkourier_teams_activities_lost_total", shedChannelBacklog); got != 0 {
		t.Errorf("lost channel_backlog = %v: Teams sends an activity answered 503 again, so it is not lost", got)
	}
}

// A message that the sink does not take in its tries is lost, and counted once, at the last; one refused because no Serve is live is
// shed, since Teams sends it again.
func TestReceiverCountsWhatIsLost(t *testing.T) {
	o := newObserved(t)
	rt := newReceiverTest(t, false, nil)
	failing := &failingSink{}
	ctx, cancel := context.WithCancel(o.ctx)
	done := make(chan error, 1)
	go func() { done <- rt.r.Serve(ctx, failing) }()
	testutil.Eventually(t, "the receiver takes requests", func() bool { return rt.r.live.Load() != nil })
	if rec := rt.post(t, fixture(t, "message_plain"), ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	testutil.Eventually(t, "the lost message is counted", func() bool {
		return o.value(t, "agentkourier_teams_activities_lost_total", lostNotTaken) == 1
	})
	if n := failing.tries.Load(); n != sinkAttempts {
		t.Errorf("the sink was tried %d times, want %d", n, sinkAttempts)
	}
	cancel()
	_ = <-done
	if rec := rt.post(t, fixture(t, "message_reply"), ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("after Serve: status %d", rec.Code)
	}
	if got := o.value(t, "agentkourier_teams_requests_total", requestNotRunning); got != 1 {
		t.Errorf("not_running = %v, want 1", got)
	}
	if got := o.value(t, "agentkourier_teams_activities_shed_total", shedNotRunning); got != 1 {
		t.Errorf("shed not_running = %v, want 1", got)
	}
	if got := o.value(t, "agentkourier_teams_activities_lost_total", lostNotTaken); got != 1 {
		t.Errorf("lost not_taken = %v after %d tries, want 1: the tries before the last are not counted", got, sinkAttempts)
	}
}

// What the drain at the stop cuts is lost, and counted once as stopped: the message the sink was given when the drain ran out, and the one
// that waited behind it. The cut message is not counted as not taken as well.
func TestReceiverCountsWhatTheStopDrops(t *testing.T) {
	o := newObserved(t)
	rt := newReceiverTest(t, false, nil)
	rt.sink.block = make(chan struct{})
	t.Cleanup(func() { close(rt.sink.block) })
	ctx, cancel := context.WithCancel(o.ctx)
	done := make(chan error, 1)
	go func() { done <- rt.r.Serve(ctx, rt.sink) }()
	testutil.Eventually(t, "the receiver takes requests", func() bool { return rt.r.live.Load() != nil })
	for _, id := range []string{"1794965000001", "1794965000002"} {
		if rec := rt.post(t, inChannel(t, "message_plain", testChannel, id), ""); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	testutil.Eventually(t, "the sink is given the first", func() bool { return rt.sink.callsOf("1794965000001") == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve returned %v", err)
	}
	if got := o.value(t, "agentkourier_teams_activities_lost_total", lostStopped); got != 2 {
		t.Errorf("lost stopped = %v, want 2: the message the sink held and the one behind it", got)
	}
	if got := o.value(t, "agentkourier_teams_activities_lost_total", lostNotTaken); got != 0 {
		t.Errorf("lost not_taken = %v, want 0: the message cut by the stop is counted as stopped alone", got)
	}
}

// failingSink takes nothing, and counts the tries.
type failingSink struct{ tries atomic.Int32 }

func (f *failingSink) Message(context.Context, chat.Inbound) error {
	f.tries.Add(1)
	return errors.New("not now")
}

func (*failingSink) Interaction(context.Context, chat.Interaction) error {
	return errors.New("not now")
}

// The series are the connection's: ForgetConnection deletes them (telemetry's list of the families with the label holds them).
func TestReceiverSeriesAreForgottenWithTheConnection(t *testing.T) {
	o := newObserved(t)
	rt := newReceiverTest(t, false, nil)
	rt.serveObserved(t, o)
	rt.post(t, fixture(t, "message_plain"), "")
	if n := promtest.CollectAndCount(o.reg, "agentkourier_teams_requests_total"); n != 1 {
		t.Fatalf("%d series, want 1", n)
	}
	telemetry.ForgetConnection(o.ctx, testConn)
	if n := promtest.CollectAndCount(o.reg, "agentkourier_teams_requests_total"); n != 0 {
		t.Fatalf("%d series after the connection was forgotten, want 0", n)
	}
}
