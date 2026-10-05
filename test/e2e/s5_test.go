//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/testhooks"
	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// s5Answer is what test-agent says in both tiers: the stub's canned reply, and mockllm's, whatever it was asked.
const s5Answer = "The answer is 4."

// TestS5_AReplyPostedBeforeACrashIsNotPostedAgain: courier is killed between posting a reply and
// recording that it did (the testhooks fault, exit 137), and after it restarts the thread holds that one
// message, found again by the tag it carries, not a second. That the restart is done with the unfinished
// turn is shown by a positive signal, a second message of the thread being answered after it.
//
// Arming a fault is a rollout, and the fault stays armed across the container's own restarts, so the
// scenario disarms at once, with another rollout, once it has seen the crash; and a failed scenario
// disarms too. The channel's Binding is output final, because only a post through the outbox reaches the
// hook: a live reply to a person is streamed, which carries the same tag but no hook.
func TestS5_AReplyPostedBeforeACrashIsNotPostedAgain(t *testing.T) {
	s5(t, newScenario(t), ChannelS5)
}

// TestS5_AReplyPostedBeforeACrashIsNotPostedAgainOverTheA2ADialect: S5 again, through the generic a2a dialect to the stub agent in
// its plain pod (see TestS1_AReplyStreamsIntoANewThreadOverTheA2ADialect). The restarted courier finds the posted reply by its tag
// and goes on with the thread, the second message reaching the agent through the same dialect.
func TestS5_AReplyPostedBeforeACrashIsNotPostedAgainOverTheA2ADialect(t *testing.T) {
	s5(t, a2aScenario(t), ChannelA2AS5)
}

// s5 is S5's body, for the channel whose Binding is bound to test-agent with final output.
func s5(t *testing.T, s *scenario, channel string) {
	arm := func(ctx context.Context, fault string) error {
		rollout, cancel := context.WithTimeout(ctx, 3*time.Minute) // a stuck rollout fails here, with its reason
		defer cancel()
		return s.pods.SetFault(rollout, kube.CourierNamespace, kube.CourierDeployment, courierContainer, fault)
	}
	t.Cleanup(func() {
		if err := arm(context.WithoutCancel(s.ctx), ""); err != nil {
			t.Errorf("disarm at the end: %v", err)
		}
	})
	if err := arm(s.ctx, testhooks.CrashAfterPost); err != nil {
		t.Fatal(err)
	}
	s.connected()
	// The baseline is of the pod the arming rollout left, taken after it: a baseline from before would let
	// the old pod's own shutdown, which may end in 137 after its grace period, answer for a crash that
	// never happened.
	base, err := s.pods.Baseline(s.ctx, kube.CourierNamespace, courierPods, courierContainer)
	if err != nil {
		t.Fatal(err)
	}

	tok := s.stub.Gate("S5")
	m := s.mention(channel, "US5", "answer once "+tok)
	crashCtx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	crash, err := s.pods.WaitCrash(crashCtx, kube.CourierNamespace, courierPods, courierContainer, base)
	if err != nil {
		t.Fatalf("courier never crashed after posting: %v\nthe thread: %s", err, s.describeNow(channel, m.TS))
	}
	if crash.ExitCode != 137 {
		t.Fatalf("courier ended with exit code %d (%s); want 137, the fault's", crash.ExitCode, crash.Reason)
	}
	if err := arm(s.ctx, ""); err != nil { // at once: the restarted container has the fault armed too
		t.Fatal(err)
	}
	s.connected()

	// What shows that the restarted courier is done with the turn it found unfinished: a second message in
	// the thread is answered only after it, because a reply waits for the thread's active task. So when
	// that answer is there, a duplicate of the first reply, if the restart made one, is there too.
	again := s.reply(channel, m.TS, "US5", "and again "+tok)
	th := s.thread(channel, m.TS, "the answer to the second message", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) >= 2 && CompareTS(bots[len(bots)-1].TS, again.TS) > 0
	})
	checkOnce := func(th []fakeslack.MessageView) (bool, string) { return answeredOnceBeforeAndAfter(th, again) }
	if ok, got := checkOnce(th); !ok {
		t.Fatalf("want one tagged message of the bot before the second message and one after, each its own and each the agent's answer: the first reply is not posted twice\nthe thread: %s", got)
	}
	// And nothing more comes after that.
	err = Consistently(s.ctx, 3*time.Second, "the thread keeps its two tagged messages of the bot", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, channel, m.TS)
		ok, _ := checkOnce(th)
		return ok, err
	})
	if err != nil {
		t.Errorf("%v\nthe thread: %s", err, s.describeNow(channel, m.TS))
	}
}

// answeredOnceBeforeAndAfter says whether the thread holds exactly one tagged message of the bot before again, and one after it, each its
// own and each the agent's answer: the first reply was posted once, and the one the second message got is another. A message the crash
// hook reaches can be any outbox post, a notice that the turn failed included, so a scenario that checked only that something was posted
// once would pass on a courier whose agent turn never worked. The string is the thread, for a failure message.
func answeredOnceBeforeAndAfter(th []fakeslack.MessageView, again fakeslack.UserMessage) (bool, string) {
	var before, after []fakeslack.MessageView
	for _, b := range BotMessages(th) {
		if CompareTS(b.TS, again.TS) < 0 {
			before = append(before, b)
		} else {
			after = append(after, b)
		}
	}
	ok := len(before) == 1 && len(after) == 1 && OutboxID(before[0]) != "" && OutboxID(after[0]) != "" && OutboxID(before[0]) != OutboxID(after[0])
	ok = ok && strings.Contains(before[0].Text, s5Answer) && strings.Contains(after[0].Text, s5Answer)
	return ok, describe(th)
}

// describeNow is the thread at ts in channel as text, for a failure message; a thread that cannot be
// read says so.
func (s *scenario) describeNow(channel, ts string) string {
	th, err := s.slack.Thread(context.WithoutCancel(s.ctx), channel, ts)
	if err != nil {
		return "(unreadable: " + err.Error() + ")"
	}
	return describe(th)
}
