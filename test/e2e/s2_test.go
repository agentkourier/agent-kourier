//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// TestS2_AThreadKeepsItsContextAcrossACourierRestart: two users take turns in one thread, courier is
// restarted through its Deployment between turns, and the next reply shows that the agent still has the
// earlier ones, which only the same context ID gives it. The agent, scenario-memory, says which turn it
// has had and quotes the earlier ones; "Message 1" would mean the context was lost.
//
// UNCONFIRMED(WP-02): what text of a mention courier sends the agent (it may keep or drop the mention). The
// assertions look for each earlier turn's own words, not for the agent's exact quoting.
func TestS2_AThreadKeepsItsContextAcrossACourierRestart(t *testing.T) {
	s := newScenario(t)
	tok := s.stub.Gate("S2") // unique to this run, and plain text
	one, two, three := "alpha-"+tok, "bravo-"+tok, "charlie-"+tok

	m := s.mention(ChannelS2, "US2A", one)
	th := s.thread(ChannelS2, m.TS, "the first reply", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 1 && finished(bots[0]) && strings.Contains(bots[0].Text, "Message 1.")
	})
	if got := BotMessages(th)[0].Text; !strings.Contains(got, "Earlier: none") {
		t.Errorf("the first reply is %q; want it to say there was nothing earlier", got)
	}
	s.reply(ChannelS2, m.TS, "US2B", two)
	th = s.thread(ChannelS2, m.TS, "the second user's reply", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 2 && finished(bots[1]) && strings.Contains(bots[1].Text, "Message 2.")
	})
	if got := BotMessages(th)[1].Text; !strings.Contains(got, one) {
		t.Errorf("the second reply is %q; want it to quote the first turn, %q", got, one)
	}

	restart, cancel := context.WithTimeout(s.ctx, 3*time.Minute) // a stuck rollout fails here, with its reason
	defer cancel()
	if err := s.pods.RolloutRestart(restart, kube.CourierNamespace, kube.CourierDeployment); err != nil {
		t.Fatal(err)
	}
	s.connected()

	s.reply(ChannelS2, m.TS, "US2A", three)
	th = s.thread(ChannelS2, m.TS, "the reply after the restart", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 3 && finished(bots[2])
	})
	got := BotMessages(th)[2].Text
	if !strings.Contains(got, "Message 3.") || !strings.Contains(got, one) || !strings.Contains(got, two) || strings.Contains(got, "Message 1") {
		t.Errorf("the reply after the restart is %q; want turn 3, quoting %q and %q: the thread kept its context", got, one, two)
	}
}
