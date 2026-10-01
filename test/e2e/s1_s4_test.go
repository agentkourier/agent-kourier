//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// TestS1_AReplyStreamsIntoANewThread: a user mentions courier; the reply appears in a new thread under the
// mention while the agent is still working, as one message that grows, and ends as one message. The agent,
// scenario-gated, holds its task on a gate the test releases, so what the thread shows while it is held
// does not depend on timing.
//
// UNCONFIRMED(WP-04): that the fake's streaming calls show what Slack's would. The assertions are about
// the fake's own record of the stream: Streaming is true from startStream to stopStream, and the text is
// what was appended. The streamed text is compared by its words, not whole, since the renderer converts the
// agent's markdown.
func TestS1_AReplyStreamsIntoANewThread(t *testing.T) {
	s := newScenario(t)
	gate := s.stub.Gate("S1")
	m := s.mention(ChannelS1, "US1", "stream it gate:"+gate)

	held := s.thread(ChannelS1, m.TS, "the first chunk, while the task is held", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 1 && bots[0].Streaming && strings.Contains(bots[0].Text, "Holding on gate "+gate)
	})
	if held[0].TS != m.TS || held[0].User != "US1" {
		t.Fatalf("the thread starts with %+v; want the user's mention, which it is a reply to", held[0])
	}
	if got := BotMessages(held)[0]; strings.Contains(got.Text, "Released") {
		t.Fatalf("the message already says %q while the gate is closed", got.Text)
	}

	if err := s.stub.Release(s.ctx, gate); err != nil {
		t.Fatal(err)
	}
	final := s.thread(ChannelS1, m.TS, "the end of the reply", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 1 && finished(bots[0]) && strings.Contains(bots[0].Text, "Released.")
	})
	if got := BotMessages(final)[0]; !strings.Contains(got.Text, "Holding on gate "+gate) || got.ThreadTS != m.TS {
		t.Errorf("the reply is %q in thread %q; want the whole text, in the mention's thread %q", got.Text, got.ThreadTS, m.TS)
	}
	// One post: the stream's end is not a second message, and nothing else is said.
	err := Consistently(s.ctx, 3*time.Second, "the thread holds the one message of the bot", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelS1, m.TS)
		return len(BotMessages(th)) == 1, err
	})
	if err != nil {
		t.Error(err)
	}
}

// TestS4_AReplySentWhileATaskRunsIsQueuedAndAnsweredAfterIt: a reply to a thread whose task is running is
// not answered at once. It is marked as waiting, with a reaction on it, and is answered after the task
// ends, once, in order. The agent holds each task on a gate, so neither depends on timing.
func TestS4_AReplySentWhileATaskRunsIsQueuedAndAnsweredAfterIt(t *testing.T) {
	s := newScenario(t)
	first, second := s.stub.Gate("S4a"), s.stub.Gate("S4b")
	m := s.mention(ChannelS4, "US4A", "one gate:"+first)
	s.thread(ChannelS4, m.TS, "the first task, held", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 1 && bots[0].Streaming && strings.Contains(bots[0].Text, "Holding on gate "+first)
	})

	r := s.reply(ChannelS4, m.TS, "US4B", "two gate:"+second)
	err := Wait(s.ctx, stepTimeout, "the reaction that marks the reply as waiting", func(ctx context.Context) (bool, error) {
		rs, err := s.slack.Reactions(ctx, ChannelS4, r.TS)
		for _, x := range rs {
			if x.Name == queuedReaction {
				return true, err
			}
		}
		return false, err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = Consistently(s.ctx, 2*time.Second, "the queued reply is not answered while the first task is held", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelS4, m.TS)
		return len(BotMessages(th)) == 1, err
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.stub.Release(s.ctx, first); err != nil {
		t.Fatal(err)
	}
	th := s.thread(ChannelS4, m.TS, "the second task to start after the first has ended", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 2 && finished(bots[0]) && strings.Contains(bots[0].Text, "Released.") && strings.Contains(bots[1].Text, "Holding on gate "+second)
	})
	if bots := BotMessages(th); !strings.Contains(bots[0].Text, "Holding on gate "+first) || CompareTS(bots[1].TS, r.TS) <= 0 {
		t.Errorf("replies %q and %q; want the first task's text, then the second's, which began after the reply it answers (%s)", bots[0].Text, bots[1].Text, r.TS)
	}
	if err := s.stub.Release(s.ctx, second); err != nil {
		t.Fatal(err)
	}
	s.thread(ChannelS4, m.TS, "the second reply to end", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 2 && finished(bots[1]) && strings.Contains(bots[1].Text, "Released.")
	})
}
