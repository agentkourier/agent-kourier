//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// feedbackValue finds, in the blocks of a message, the value of the Useful button of an investigation.
var feedbackValue = regexp.MustCompile(`fb1\|investigation\|useful\|[^"]+`)

// TestS17_PeopleRateAnInvestigationAndChangeTheirVote: an alert starts an investigation in its own thread, whose final message
// carries the Useful and Not useful buttons (spec 8.7), though the Binding does not set feedback.enabled. A member who presses
// Useful is counted and told so in an ephemeral, alone; pressing Not useful instead changes their vote, which is one more vote
// under its new verdict, and pressing it again changes nothing and counts nothing. A second member's vote is their own. The
// press of a bot, and of a person who is not in the channel, count for nothing and are told nothing, and no vote is a message
// in the channel. A press after Agent Kourier has been restarted still records, on the same message: the buttons carry what they rate.
//
// The fast tier only, as S14: the full tier's kagent and the AX tier have no scenario-alert agent.
func TestS17_PeopleRateAnInvestigationAndChangeTheirVote(t *testing.T) {
	s := newScenario(t)
	if s.env.Tier != "fast" {
		t.Skipf("scenario-alert is a stub agent of the fast tier; the %s tier has no such agent", s.env.Tier)
	}
	const (
		ada, bob, bot, outsider = "US17ADA", "US17BOB", "UBOT17", "US17OUT"
		binding                 = "agent-kourier/fast-s17"
		agent                   = "kagent/scenario-alert"
	)
	if err := s.slack.AddUser(s.ctx, fakeslack.User{ID: bot, Name: "helper17", RealName: "Helper bot", IsBot: true}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{ada, bob, outsider} {
		s.know(u)
	}
	if err := s.slack.SetMembers(s.ctx, ChannelS17, ada, bob, bot); err != nil { // the outsider is not in the channel
		t.Fatal(err)
	}

	// The channel, the stub and Agent Kourier's database outlive a run, and a key holds its thread for the cooldown, so the alert is
	// one no earlier run posted.
	name := fmt.Sprintf("AlertS17x%d", time.Now().UnixNano()%1_000_000_000)
	p := alertPost("FIRING", name, name+" shop")
	p.Channel, p.BotID = ChannelS17, BotS17
	alert, err := s.slack.Webhook(s.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	s.acked(alert)

	// The final message of the investigation, with its buttons.
	isFinal := func(m fakeslack.MessageView) bool {
		return m.BotID != BotS17 && strings.Contains(m.Text, "Investigating:") && finished(m) && feedbackValue.Match(m.Blocks)
	}
	th := s.thread(ChannelS17, alert.TS, "the investigation's final message with its buttons", func(th []fakeslack.MessageView) bool {
		for _, m := range th {
			if isFinal(m) {
				return true
			}
		}
		return false
	})
	var reply fakeslack.MessageView
	for _, m := range th {
		if isFinal(m) {
			reply = m
		}
	}
	useful := feedbackValue.FindString(string(reply.Blocks))
	notUseful := strings.Replace(useful, "|useful|", "|not_useful|", 1)
	if strings.Count(string(reply.Blocks), "Was this useful?") != 1 || !strings.Contains(string(reply.Blocks), notUseful) {
		t.Fatalf("the final message has blocks %s; want the question once and both buttons", reply.Blocks)
	}
	messages := len(BotMessages(th))

	label := func(verdict string) map[string]string {
		return map[string]string{"binding": binding, "agent": agent, "alertname": name, "verdict": verdict}
	}
	counted := func(verdict string) float64 {
		return s.counter("agentkourier_investigation_feedback_total", label(verdict))
	}
	click := func(user, action, value string) {
		t.Helper()
		id, err := s.slack.Click(s.ctx, user, reply.TS, action, value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.slack.AckWait(s.ctx, 15*time.Second, id); err != nil {
			t.Fatalf("Agent Kourier never acked the press of %s: %v", user, err)
		}
	}
	waitCount := func(verdict string, want float64) {
		t.Helper()
		err := Wait(s.ctx, stepTimeout, fmt.Sprintf("%s{verdict=%s} to be %v", "agentkourier_investigation_feedback_total", verdict, want), func(context.Context) (bool, error) {
			return counted(verdict) == want, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// told is what a person was told in this thread, which only they can see.
	told := func(user string) (texts []string) {
		es, err := s.slack.Ephemerals(s.ctx, ChannelS17)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Recipient == user && e.ThreadTS == alert.TS {
				texts = append(texts, e.Text)
			}
		}
		return texts
	}
	waitTold := func(user string, n int) []string {
		t.Helper()
		var texts []string
		err := Wait(s.ctx, stepTimeout, fmt.Sprintf("%s to be told %d times", user, n), func(context.Context) (bool, error) {
			texts = told(user)
			return len(texts) >= n, nil
		})
		if err != nil {
			t.Fatalf("%v; told: %q", err, texts)
		}
		return texts
	}

	// A member votes, and is told so alone.
	click(ada, "feedback-useful", useful)
	waitCount("useful", 1)
	if texts := waitTold(ada, 1); !strings.Contains(texts[0], "recorded as useful") {
		t.Errorf("told %q; want the vote recorded as useful", texts)
	}
	// Changing it is one more vote, under its new verdict.
	click(ada, "feedback-not-useful", notUseful)
	waitCount("not_useful", 1)
	if texts := waitTold(ada, 2); !strings.Contains(texts[1], "recorded as not useful") {
		t.Errorf("told %q; want the changed vote recorded as not useful", texts)
	}
	// The same verdict again changes nothing: it is told so, and counted no more.
	click(ada, "feedback-not-useful", notUseful)
	if texts := waitTold(ada, 3); !strings.Contains(texts[2], "already recorded") {
		t.Errorf("told %q; want the repeated press answered that the vote is recorded already", texts)
	}
	// Another member's vote is their own.
	click(bob, "feedback-useful", useful)
	waitCount("useful", 2)

	// A bot's press and a press of a person outside the channel count for nothing, and are told nothing.
	click(bot, "feedback-useful", useful)
	click(outsider, "feedback-useful", useful)
	err = Consistently(s.ctx, 5*time.Second, "the bot's and the outsider's presses count for nothing", func(context.Context) (bool, error) {
		return counted("useful") == 2 && counted("not_useful") == 1 && len(told(bot)) == 0 && len(told(outsider)) == 0, nil
	})
	if err != nil {
		t.Errorf("%v (useful %v, not useful %v, told the bot %q and the outsider %q)", err, counted("useful"), counted("not_useful"), told(bot), told(outsider))
	}
	if got, err := s.slack.Thread(s.ctx, ChannelS17, alert.TS); err != nil || len(BotMessages(got)) != messages {
		t.Errorf("the thread holds %d bot messages after the votes (%v); want the %d it had: a vote is no message in the channel", len(BotMessages(got)), err, messages)
	}

	// A press after a restart records, on the same message.
	restart, cancel := context.WithTimeout(s.ctx, 3*time.Minute) // a stuck rollout fails here, with its reason
	defer cancel()
	if err := s.pods.RolloutRestart(restart, kube.AgentKourierNamespace, kube.AgentKourierDeployment); err != nil {
		t.Fatal(err)
	}
	s.connected()
	click(bob, "feedback-not-useful", notUseful)
	waitCount("not_useful", 1) // the restarted process counts from nothing: Bob's changed vote is its first
	if texts := waitTold(bob, 2); !strings.Contains(texts[len(texts)-1], "recorded as not useful") {
		t.Errorf("told %q; want Bob's vote after the restart recorded as not useful", texts)
	}
}
