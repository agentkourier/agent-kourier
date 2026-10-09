//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// followUpHint is the line that ends the final message of a turn in a Binding with chat.threadReplies: mention (spec 8.2).
const followUpHint = "Mention me in this thread to ask a follow-up."

// TestS18_OnlyAMentionIsATurnInAThreadAndItCarriesTheRepliesSinceTheLastTurn: in a Binding with threadReplies mention, a reply in a
// thread Agent Kourier owns that does not mention Agent Kourier is ignored: no message, no reaction, and the agent is not woken. The next
// mention carries the replies since the previous turn, in a block of their own after its text, which the scenario agent
// reports ("Turn N. Replies: K. Block: <tag>."): in a thread a mention started (case 1), and in one an alert started, where
// Agent Kourier's own note ("Fired again") is not a reply (case 2). A mention sent while the agent is held is queued, and an unmentioned
// reply between the two is in the second one's window only: none lost, none sent twice (case 3). The ignored replies and the
// contexts are counted (case 4). The final message of each turn ends with the follow-up hint, and nothing else does.
//
// The fast tier only: the full tier's kagent and the AX tier have no scenario-context agent.
func TestS18_OnlyAMentionIsATurnInAThreadAndItCarriesTheRepliesSinceTheLastTurn(t *testing.T) {
	s := newScenario(t)
	if s.env.Tier != "fast" {
		t.Skipf("scenario-context is a stub agent of the fast tier; the %s tier has no such agent", s.env.Tier)
	}
	const binding = "agent-kourier/fast-s18"
	unmentioned := func() float64 {
		return s.counter("agentkourier_thread_unmentioned_total", map[string]string{"binding": binding})
	}
	contexts := func(outcome string) float64 {
		return s.counter("agentkourier_thread_context_total", map[string]string{"binding": binding, "outcome": outcome})
	}
	unmentionedBefore := unmentioned()
	contextsBefore := map[string]float64{}
	for _, outcome := range []string{"read", "empty", "cut", "failed", "unsupported"} {
		contextsBefore[outcome] = contexts(outcome)
	}

	// answers are Agent Kourier's messages of a thread that hold an answer of the scenario agent, oldest first, once they end with
	// the follow-up hint: a stream that has stopped is not yet the final message, whose footer comes with it (S17 waits for its
	// buttons for the same reason). The bot's alert and a note of Agent Kourier's are none.
	isAnswer := func(m fakeslack.MessageView) bool {
		return m.BotID != BotS18 && strings.Contains(m.Text, "Turn ") && finished(m) && strings.Contains(string(m.Blocks), followUpHint)
	}
	answers := func(th []fakeslack.MessageView) (out []fakeslack.MessageView) {
		for _, m := range BotMessages(th) {
			if isAnswer(m) {
				out = append(out, m)
			}
		}
		return out
	}
	// says waits for the thread's n-th answer to match want (a regular expression), and returns the thread.
	says := func(channel, ts string, n int, want string) []fakeslack.MessageView {
		t.Helper()
		re := regexp.MustCompile(want)
		return s.thread(channel, ts, fmt.Sprintf("answer %d to be finished, with the follow-up hint, and say %s", n, want), func(th []fakeslack.MessageView) bool {
			got := answers(th)
			return len(got) >= n && re.MatchString(got[n-1].Text)
		})
	}
	// quiet checks, for 5 seconds, that the unmentioned replies woke nothing: the thread holds no more of Agent Kourier's messages than
	// it did, and none of the replies has a reaction (the hourglass of a queued reply, or any other).
	quiet := func(channel, ts string, replies ...fakeslack.UserMessage) {
		t.Helper()
		before, err := s.slack.Thread(s.ctx, channel, ts)
		if err != nil {
			t.Fatal(err)
		}
		err = Consistently(s.ctx, 5*time.Second, "the unmentioned replies are ignored", func(ctx context.Context) (bool, error) {
			th, err := s.slack.Thread(ctx, channel, ts)
			if err != nil || len(BotMessages(th)) != len(BotMessages(before)) {
				return false, err
			}
			for _, r := range replies {
				rs, err := s.slack.Reactions(ctx, channel, r.TS)
				if err != nil || len(rs) != 0 {
					return false, err
				}
			}
			return true, nil
		})
		if err != nil {
			th, _ := s.slack.Thread(s.ctx, channel, ts)
			t.Fatalf("%v\nthe thread: %s", err, describe(th))
		}
	}
	// hinted checks that the follow-up hint ends each answer once, and is on no other message of Agent Kourier's.
	hinted := func(th []fakeslack.MessageView) {
		t.Helper()
		for _, m := range BotMessages(th) {
			if n := strings.Count(string(m.Blocks), followUpHint) + strings.Count(m.Text, followUpHint); isAnswer(m) && n != 1 || !isAnswer(m) && n != 0 {
				t.Errorf("the message %q has the follow-up hint %d times (blocks %s); want once on an answer and never elsewhere", m.Text, n, m.Blocks)
			}
		}
	}

	// Case 1: a mention starts the thread, two replies without one are ignored, and the next mention carries them.
	const ada, bob, cy = "US18ADA", "US18BOB", "US18CY"
	m := s.mention(ChannelS18, ada, "what is going on")
	th := says(ChannelS18, m.TS, 1, `Turn 1\. Replies: 0\. Block: none\.`)
	hinted(th)
	r1 := s.reply(ChannelS18, m.TS, bob, "I think it is the cache")
	r2 := s.reply(ChannelS18, m.TS, cy, "no, the node is full")
	quiet(ChannelS18, m.TS, r1, r2)
	s.reply(ChannelS18, m.TS, ada, botMention+"what do you make of that")
	th = says(ChannelS18, m.TS, 2, `Turn 2\. Replies: 2\. Block: thread-replies-[0-9a-f]{12}\.`)
	hinted(th)
	if n := len(answers(th)); n != 2 {
		t.Errorf("the thread holds %d answers; want the two turns' and none for the ignored replies", n)
	}

	// Case 2: an alert starts a thread through the bot's trigger. A reply without a mention is ignored, a fire again is Agent Kourier's
	// note, and the next mention carries the one reply and not the note.
	// The channel, the stub and Agent Kourier's database outlive a run, and a key holds its thread for the cooldown, so the alert is
	// one no earlier run posted.
	name := fmt.Sprintf("AlertS18x%d", time.Now().UnixNano()%1_000_000_000)
	post := func(state string) fakeslack.UserMessage {
		t.Helper()
		p := alertPost(state, name, name+" shop")
		p.Channel, p.BotID = ChannelS18, BotS18
		a, err := s.slack.Webhook(s.ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		s.acked(a)
		return a
	}
	alert := post("FIRING")
	th = says(ChannelS18, alert.TS, 1, `Turn 1\. Replies: 0\. Block: none\.`)
	hinted(th)
	r3 := s.reply(ChannelS18, alert.TS, bob, "the pod restarted twice before this")
	post("FIRING") // Agent Kourier's note in the alert's thread, "Fired again at", is its own post and no reply of a person
	s.thread(ChannelS18, alert.TS, "the note of the repeat", func(th []fakeslack.MessageView) bool {
		for _, b := range BotMessages(th) {
			if strings.Contains(b.Text, "Fired again at") && finished(b) {
				return true
			}
		}
		return false
	})
	quiet(ChannelS18, alert.TS, r3)
	s.reply(ChannelS18, alert.TS, ada, botMention+"anything to add")
	th = says(ChannelS18, alert.TS, 2, `Turn 2\. Replies: 1\. Block: thread-replies-[0-9a-f]{12}\.`)
	hinted(th)

	// Case 3: a mention while the agent is held on its turn is queued, with the hourglass. A reply without a mention between
	// the two lands in the second one's window, once: not the first's, which is held when the reply is written, and not in the
	// window of a third mention, which starts after the second.
	gate := s.stub.Gate("S18")
	first := s.mention(ChannelS18, ada, "hold gate:"+gate)
	s.thread(ChannelS18, first.TS, "the first turn, held", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 1 && bots[0].Streaming && strings.Contains(bots[0].Text, "Turn 1. Replies: 0. Block: none.")
	})
	between := s.reply(ChannelS18, first.TS, bob, "the disk is at 99 percent")
	queued := s.reply(ChannelS18, first.TS, ada, botMention+"and now")
	hourglass := func(m fakeslack.UserMessage, want bool) {
		t.Helper()
		err := Wait(s.ctx, stepTimeout, fmt.Sprintf("the hourglass on %s to be %t", m.TS, want), func(ctx context.Context) (bool, error) {
			rs, err := s.slack.Reactions(ctx, ChannelS18, m.TS)
			has := false
			for _, r := range rs {
				has = has || r.Name == queuedReaction
			}
			return has == want, err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	hourglass(queued, true)
	err := Consistently(s.ctx, 2*time.Second, "the queued mention is not answered while the first turn is held", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelS18, first.TS)
		return len(BotMessages(th)) == 1, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.stub.Release(s.ctx, gate); err != nil {
		t.Fatal(err)
	}
	says(ChannelS18, first.TS, 1, `Turn 1\. Replies: 0\. Block: none\. Released\.`)
	th = says(ChannelS18, first.TS, 2, `Turn 2\. Replies: 1\. Block: thread-replies-[0-9a-f]{12}\.`)
	hourglass(queued, false)
	hinted(th)
	// The reply came before the second mention and is sent with it: a third mention, after both, carries none of it again, and
	// has no block (the window holds Agent Kourier's own answer alone).
	s.reply(ChannelS18, first.TS, cy, botMention+"anything new")
	th = says(ChannelS18, first.TS, 3, `Turn 3\. Replies: 0\. Block: none\.`)
	hinted(th)
	if rs, err := s.slack.Reactions(s.ctx, ChannelS18, between.TS); err != nil || len(rs) != 0 {
		t.Errorf("the unmentioned reply has reactions %+v (%v); want none", rs, err)
	}

	// Case 4: the counters.
	for _, want := range []struct {
		name string
		got  float64
		diff float64
	}{
		{"agentkourier_thread_context_total{outcome=cut}", contexts("cut") - contextsBefore["cut"], 0},
		{"agentkourier_thread_context_total{outcome=failed}", contexts("failed") - contextsBefore["failed"], 0},
		{"agentkourier_thread_context_total{outcome=unsupported}", contexts("unsupported") - contextsBefore["unsupported"], 0},
		{"agentkourier_thread_unmentioned_total", unmentioned() - unmentionedBefore, 4},
		{"agentkourier_thread_context_total{outcome=read}", contexts("read") - contextsBefore["read"], 3},
		{"agentkourier_thread_context_total{outcome=empty}", contexts("empty") - contextsBefore["empty"], 1},
	} {
		if want.got != want.diff {
			t.Errorf("%s rose by %v; want %v", want.name, want.got, want.diff)
		}
	}
}
