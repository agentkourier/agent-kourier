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

// TestA2A_APlainPodThatRestartedAnswersAThreadFromAFreshContext: what a thread shows when the agent no longer knows its
// contextId, as after the restart of an agent pod that has no volume (docs/spikes/demo1-a2a-plain-pod.md). Two users take turns in a
// thread, the stub agent's pod is deleted and replaced between the second turn and the third (it keeps its sessions in memory, as an
// agent without a volume does), and the next reply shows what the agent has: scenario-memory says which turn its context has had.
//
// This is one of the two ways an agent can react, measured with the stub: it accepts the unknown contextId, keeping the id, and starts
// a context without error (the stub's own behavior, UNCONFIRMED(WP-02) against kagent). The other, an agent that answers the send with
// an error, is not reachable with the stub and is read from the code in the note. The assertions are what was measured: the third
// reply is turn 1 of an empty context, so the agent lost its memory, and Agent Kourier says nothing about it, since the id it sent is the id
// it got back (the notice of a session opened under another id, NoticeSessionGone, needs the agent to answer with another one).
//
// The fast tier only: the stub agent is not in the others.
func TestA2A_APlainPodThatRestartedAnswersAThreadFromAFreshContext(t *testing.T) {
	s := a2aScenario(t)
	tok := s.stub.Gate("A2ACONTEXT") // unique to this run, and plain text
	one, two, three := "alpha-"+tok, "bravo-"+tok, "charlie-"+tok

	m := s.mention(ChannelA2AMemory, "UCTXA", one)
	s.thread(ChannelA2AMemory, m.TS, "the first reply", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 1 && finished(bots[0]) && strings.Contains(bots[0].Text, "Message 1. Earlier: none")
	})
	s.reply(ChannelA2AMemory, m.TS, "UCTXB", two)
	th := s.thread(ChannelA2AMemory, m.TS, "the second reply, which quotes the first", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 2 && finished(bots[1]) && strings.Contains(bots[1].Text, "Message 2.")
	})
	// Without this the later "Message 1" would not mean a context lost: it would be what the dialect always does.
	if got := BotMessages(th)[1].Text; !strings.Contains(got, one) {
		t.Fatalf("the second reply is %q; want it to quote the first turn, %q: the dialect must keep the context before the agent can lose it", got, one)
	}

	restart, cancel := context.WithTimeout(s.ctx, 3*time.Minute) // a stuck replacement fails here, with its reason
	defer cancel()
	fresh, err := s.pods.Restart(restart, kube.Namespace, "app=stubagent")
	if err != nil {
		t.Fatal(err)
	}
	// Restart returns when the replacement is Ready, but the pod it replaced may still be terminating with its endpoint routed, and a
	// turn sent then could meet a refused or reset connection, which is Agent Kourier's retry and not what is measured. So the third turn waits
	// until the replacement is the only pod there.
	err = Wait(restart, time.Minute, "the replaced stub agent pod to be gone", func(ctx context.Context) (bool, error) {
		pods, err := s.pods.List(ctx, kube.Namespace, "app=stubagent")
		return len(pods) == 1 && pods[0].UID == fresh.UID && pods[0].DeletionTimestamp == nil, err
	})
	if err != nil {
		t.Fatal(err)
	}

	s.reply(ChannelA2AMemory, m.TS, "UCTXA", three)
	th = s.thread(ChannelA2AMemory, m.TS, "the reply after the agent's restart", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) >= 3 && finished(bots[len(bots)-1])
	})
	t.Logf("the thread after the agent's restart:%s", describe(th))
	bots := BotMessages(th)
	got := bots[len(bots)-1].Text
	if !strings.Contains(got, "Message 1. Earlier: none") || strings.Contains(got, one) || strings.Contains(got, two) {
		t.Errorf("the reply after the restart is %q; want turn 1 of an empty context, which the agent opened for the contextId it did not know", got)
	}
	// Agent Kourier tells the thread nothing about it: no notice, and no second reply. If this fails, the note's account of what the thread
	// shows is out of date.
	err = Consistently(s.ctx, 3*time.Second, "the thread holds the three replies of the bot and no notice", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelA2AMemory, m.TS)
		return len(BotMessages(th)) == 3, err
	})
	if err != nil {
		t.Errorf("%v\nthe thread:%s", err, describe(th))
	}
}
