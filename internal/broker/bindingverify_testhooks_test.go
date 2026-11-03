//go:build testhooks

package broker

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// The verification against the real Slack client and the fake Slack behind it: the bot a member of the Binding's channel is joined, a
// channel the fake has never heard of is not found, and an archived channel and an app without the scope are told apart on the next
// probe, each warned of with its own reason. When the channel is mended the Binding is ready again.
func TestRunTellsAnArchivedChannelAndAMissingScopeApartThroughTheFakeSlack(t *testing.T) {
	w := newSlackWorld(t, "xoxb-fake-bot")
	logs := &logBuffer{}
	w.cfg.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	w.cfg.probeEvery, w.cfg.probeTick = 40*time.Millisecond, 10*time.Millisecond
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
