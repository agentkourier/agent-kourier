//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// stormAlert is the Alertmanager message, firing, of the alert name in group, through S15's channel's incoming webhook.
func stormAlert(name, group string) fakeslack.WebhookPost {
	p := alertPost("FIRING", name, group)
	p.Channel, p.BotID = ChannelS15, BotS15
	return p
}

// TestS15_AStormOfAlertsIsHeldToTheLimitsAndGetsOneDigest: six distinct alerts posted at once through the incoming webhook
// to a channel whose trigger allows three starts in 10 seconds. The first three each start an investigation in their own
// thread; the other three start nothing and get no note in their own thread; and when the window ends one line in the
// channel, at the top level, says that three alerts were not investigated, once. The decisions and the limited counter say
// the same, and the line names no alert.
//
// The fast tier only, as S14: the full tier's kagent and the AX tier have no scenario-alert agent. The window slides, so a run within 10
// seconds of an earlier one on the same cluster would meet that run's starts; runs are further apart than that.
func TestS15_AStormOfAlertsIsHeldToTheLimitsAndGetsOneDigest(t *testing.T) {
	s := newScenario(t)
	if s.env.Tier != "fast" {
		t.Skipf("scenario-alert is a stub agent of the fast tier; the %s tier has no such agent", s.env.Tier)
	}
	const (
		storm   = 6
		started = 3
		binding = "courier/fast-s15"
		digest  = "3 alert messages not investigated in the last 10s (rate limit)."
	)
	decisions := func(decision string) float64 {
		return s.counter("courier_trigger_decisions_total", map[string]string{"binding": binding, "decision": decision})
	}
	limited := func() float64 {
		return s.counter("courier_trigger_limited_total", map[string]string{"binding": binding, "trigger": "0", "limit": "rate"})
	}
	startedBefore, limitedBefore, limitedCountBefore := decisions("started"), decisions("limited"), limited()

	// The channel and courier's database outlive a run, and a key holds its thread for the cooldown: the alerts are ones no
	// earlier run posted. Each has a key of its own.
	suffix := fmt.Sprintf("x%d", time.Now().UnixNano()%1_000_000_000)
	var alerts []fakeslack.UserMessage
	for i := range storm {
		name := fmt.Sprintf("AlertS15%s%d", suffix, i)
		m, err := s.slack.Webhook(s.ctx, stormAlert(name, name+" shop"))
		if err != nil {
			t.Fatal(err)
		}
		s.acked(m) // courier takes them in the order they were posted, so the first three are the ones that start
		alerts = append(alerts, m)
	}

	investigated := func(th []fakeslack.MessageView) bool {
		for _, b := range BotMessages(th) {
			if strings.Contains(b.Text, "Investigating:") && finished(b) {
				return true
			}
		}
		return false
	}
	for i, m := range alerts[:started] {
		s.thread(ChannelS15, m.TS, fmt.Sprintf("the investigation of alert %d", i), investigated)
	}
	err := Consistently(s.ctx, 5*time.Second, "the other alerts have no reply in their threads", func(ctx context.Context) (bool, error) {
		for _, m := range alerts[started:] {
			th, err := s.slack.Thread(ctx, ChannelS15, m.TS)
			if err != nil || len(th) != 1 {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		t.Errorf("%v", err)
	}

	// The digest comes when the window ends: ten seconds after the first limited alert, and within the digest loop's 15
	// seconds after that.
	isDigest := func(m fakeslack.MessageView) bool {
		return m.ThreadTS == "" && m.BotID != "" && m.BotID != BotS15 && m.TS > alerts[0].TS && strings.Contains(m.Text, "not investigated in the last")
	}
	digests := func(ctx context.Context) (n int, text string, err error) {
		msgs, err := s.slack.Messages(ctx, ChannelS15)
		for _, m := range msgs {
			if isDigest(m) {
				n, text = n+1, m.Text
			}
		}
		return n, text, err
	}
	var text string
	err = Wait(s.ctx, 90*time.Second, "the digest line in the channel", func(ctx context.Context) (bool, error) {
		n, got, err := digests(ctx)
		text = got
		return n >= 1, err
	})
	if err != nil {
		msgs, _ := s.slack.Messages(s.ctx, ChannelS15)
		t.Fatalf("%v\nthe channel: %s", err, describe(msgs))
	}
	if !strings.Contains(text, digest) {
		t.Errorf("the digest reads %q; want %q", text, digest)
	}
	if strings.Contains(text, "AlertS15") {
		t.Errorf("the digest names an alert: %q", text)
	}
	// Once: the loop passes every 15 seconds, so a second line would be there within 20.
	err = Consistently(s.ctx, 20*time.Second, "the channel holds the one digest", func(ctx context.Context) (bool, error) {
		n, _, err := digests(ctx)
		return n == 1, err
	})
	if err != nil {
		t.Errorf("%v", err)
	}

	for decision, want := range map[string]float64{"started": startedBefore + started, "limited": limitedBefore + storm - started} {
		if got := decisions(decision); got != want {
			t.Errorf("courier_trigger_decisions_total{decision=%q} is %v; want %v", decision, got, want)
		}
	}
	if got, want := limited(), limitedCountBefore+storm-started; got != want {
		t.Errorf("courier_trigger_limited_total{limit=\"rate\"} is %v; want %v", got, want)
	}
}
