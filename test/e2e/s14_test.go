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

// alertLink is the link of the kind receiver's alerts, which the Binding's trigger narrows to (anchored at its end).
const alertLink = "http://alertmanager.kind.local/#/alerts?receiver=slack-critical-kind"

// alertPost is what Alertmanager's Slack receiver posts through the channel's incoming webhook: the flattened
// attachment, with the state and the alert's name in its title, the group's label values in its fallback, and a
// line of detail. name is the alertname and group the label values that make the alert's key.
func alertPost(state, name, group string) fakeslack.WebhookPost {
	title, fallback := "[kind] FIRING (1) - "+name, "[FIRING:1] "+group+" (critical) | "+alertLink
	if state == "RESOLVED" {
		title, fallback = "[kind] RESOLVED - "+name, "[RESOLVED] "+group+" (critical) | "+alertLink
	}
	return fakeslack.WebhookPost{Channel: ChannelS14, BotID: BotS14, Attachments: []fakeslack.Attachment{{
		Title: title, TitleLink: alertLink, Fallback: fallback, Text: "*" + name + "* - critical\nPod is crash looping.\n",
	}}}
}

// TestS14_AnAlertStartsOneInvestigationInItsOwnThread: an Alertmanager alert posted through the channel's incoming
// webhook (a bot message, the way the alert channel gets them) starts exactly one investigation, in the alert's own
// thread. The agent's first turn is the trigger's prompt with the whole alert in the hashed untrusted block, which the
// scenario agent says it saw. The alert firing again is a note in that thread and starts no second turn, and the
// resolve is a note, "Resolved at". The decisions are counted.
//
// The fast tier only: the full tier's kagent and the AX tier have no scenario-alert agent.
func TestS14_AnAlertStartsOneInvestigationInItsOwnThread(t *testing.T) {
	s := newScenario(t)
	if s.env.Tier != "fast" {
		t.Skipf("scenario-alert is a stub agent of the fast tier; the %s tier has no such agent", s.env.Tier)
	}
	// The channel, the stub and Agent Kourier's database outlive a run, and a key holds its thread for the cooldown, so the
	// alert is one no earlier run posted.
	name := fmt.Sprintf("AlertS14x%d", time.Now().UnixNano()%1_000_000_000)
	group := name + " shop"
	decisions := func(decision string) float64 {
		return s.counter("agentkourier_trigger_decisions_total", map[string]string{"binding": "agent-kourier/fast-s14", "decision": decision})
	}
	startedBefore, repeatBefore, closedBefore := decisions("started"), decisions("repeat"), decisions("closed")

	post := func(p fakeslack.WebhookPost) fakeslack.UserMessage {
		t.Helper()
		m, err := s.slack.Webhook(s.ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		s.acked(m)
		return m
	}
	investigations := func(th []fakeslack.MessageView) (n int) {
		for _, b := range BotMessages(th) {
			if strings.Contains(b.Text, "Investigating:") {
				n++
			}
		}
		return n
	}
	has := func(text string) func([]fakeslack.MessageView) bool {
		return func(th []fakeslack.MessageView) bool {
			for _, b := range BotMessages(th) {
				if strings.Contains(b.Text, text) && finished(b) {
					return true
				}
			}
			return false
		}
	}

	alert := post(alertPost("FIRING", name, group))
	th := s.thread(ChannelS14, alert.TS, "the investigation", has("Investigating:"))
	if th[0].TS != alert.TS || th[0].BotID != BotS14 {
		t.Errorf("the thread starts with %+v; want the alert itself, posted by %s", th[0], BotS14)
	}
	var reply fakeslack.MessageView
	for _, b := range BotMessages(th) {
		if b.BotID != BotS14 && strings.Contains(b.Text, "Investigating:") {
			reply = b
		}
	}
	// The scenario agent says the first line of the prompt it was given, which is the trigger's template output over the
	// alert, and the tag of the untrusted block Agent Kourier appended.
	wantFirst := fmt.Sprintf("Investigating: An alert is FIRING in channel %s: %s. Block: untrusted-data-", ChannelS14, name)
	if reply.ThreadTS != alert.TS || !strings.Contains(reply.Text, wantFirst) {
		t.Errorf("the investigation is %q in thread %q; want it in the alert's thread, saying %q and a block", reply.Text, reply.ThreadTS, wantFirst)
	}

	// The turn ran on the update path (an alert has no one to stream to) in the default progress mode, so the thread was shown as
	// busy when the turn began and the mark was cleared when it ended. The fake keeps the calls in order.
	err := Wait(s.ctx, 30*time.Second, "the alert thread to be marked busy and then cleared", func(ctx context.Context) (bool, error) {
		all, err := s.slack.ThreadStatuses(ctx, ChannelS14)
		if err != nil {
			return false, err
		}
		var mine []fakeslack.ThreadStatus
		for _, st := range all {
			if st.ThreadTS == alert.TS {
				mine = append(mine, st)
			}
		}
		return len(mine) >= 2 && mine[0].Status == "is working…" && mine[len(mine)-1].Status == "", nil
	})
	if err != nil {
		t.Errorf("%v", err)
	}

	// A fire again is a note in the same thread, and no second turn.
	again := post(alertPost("FIRING", name, group))
	s.thread(ChannelS14, alert.TS, "the note of the repeat", has("Fired again at"))
	// And the resolve.
	post(alertPost("RESOLVED", name, group))
	th = s.thread(ChannelS14, alert.TS, "the note of the resolve", has("Resolved at"))

	err = Consistently(s.ctx, 5*time.Second, "the thread holds the one investigation and the two notes", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelS14, alert.TS)
		return investigations(th) == 1 && len(BotMessages(th)) == 4, err // the alert is a bot's message too: the alert, the reply, two notes
	})
	if err != nil {
		t.Errorf("%v\nthe thread: %s", err, describe(th))
	}
	if own, err := s.slack.Thread(s.ctx, ChannelS14, again.TS); err != nil || len(own) != 1 {
		t.Errorf("the repeat has a thread of its own: %+v, %v; want its note in the first alert's", own, err)
	}

	for decision, before := range map[string]float64{"started": startedBefore, "repeat": repeatBefore, "closed": closedBefore} {
		if got := decisions(decision); got != before+1 {
			t.Errorf("agentkourier_trigger_decisions_total{decision=%q} went from %v to %v; want one more", decision, before, got)
		}
	}
}
