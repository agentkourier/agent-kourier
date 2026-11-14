//go:build testhooks

package broker

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// The verification against the real Slack client and the fake Slack behind it: the bot a member of the Binding's channel is joined, a
// channel the fake has never heard of is not found, and an archived channel and an app without the scope are told apart on the next
// probe, each warned of with its own reason. When the channel is mended the Binding is ready again. Throughout, the probes post nothing and
// run no turn: no message lands in either channel, no turn begins, and the stub agent is sent no message.
func TestRunTellsAnArchivedChannelAndAMissingScopeApartThroughTheFakeSlack(t *testing.T) {
	t.Parallel()
	w := newSlackWorld(t, "xoxb-fake-bot")
	runsAlongside(&w.cfg)
	logs := &logBuffer{}
	w.cfg.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	w.cfg.probeEvery, w.cfg.probeTick, w.cfg.probeGap = 40*time.Millisecond, 10*time.Millisecond, time.Millisecond
	w.fake.SetMembers(verifiedChannel, "UFAKEBOT") // the fake's own bot is in the channel; the other Binding's channel is unknown to it
	r := start(t, w.cfg)

	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 1`)
	r.waitMetric(`agentkourier_binding_condition{binding="` + unverifiedBinding + `",condition="ChannelJoined",status="false"} 1`)
	r.waitMetric(`agentkourier_binding_condition{binding="` + unverifiedBinding + `",condition="BackendReachable",status="true"} 1`)

	w.fake.SetArchived(verifiedChannel, true)
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 0`)
	w.fake.SetArchived(verifiedChannel, false)
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 1`)
	w.fake.SetMissingScope("conversations.info", true)
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 0`)
	w.fake.SetMissingScope("conversations.info", false)
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 1`)

	// The probes only read: after several more of them nothing was posted to either channel, no turn began, and the agent was sent no message.
	r.waitMoreProbes("ChannelJoined", 6)
	r.waitMoreProbes("BackendReachable", 6)
	for _, channel := range []string{verifiedChannel, "G0123456789"} {
		if msgs := w.fake.Messages(channel); len(msgs) != 0 {
			t.Errorf("%d messages in %s; want none: a probe posts nothing", len(msgs), channel)
		}
	}
	w.sink.mu.Lock()
	turnsStarted := len(w.sink.outputs)
	w.sink.mu.Unlock()
	rec := httptest.NewRecorder()
	if !w.stub.ServeSendControl(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/_control/send-gate", http.NoBody)) {
		t.Fatal("the stub agent's send state route was not handled")
	}
	var state stubagent.SendGateState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if turnsStarted != 0 || len(state.Sends) != 0 {
		t.Errorf("%d turns began and the agent was sent %d messages; want none: a probe runs no turn", turnsStarted, len(state.Sends))
	}

	out := logs.String()
	for _, want := range []string{
		"binding=" + unverifiedBinding + " condition=ChannelJoined status=False reason=ChannelNotFound",
		"binding=" + verifiedBinding + " condition=ChannelJoined status=False reason=ChannelArchived",
		"binding=" + verifiedBinding + " condition=ChannelJoined status=False reason=MissingScope",
		"binding=" + verifiedBinding + " condition=ChannelJoined status=True reason=OK",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks a line with %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "xoxb-fake-bot") {
		t.Errorf("the log holds a token:\n%s", out)
	}
}
