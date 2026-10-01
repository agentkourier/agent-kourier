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

// TestS5_AReplyPostedBeforeACrashIsNotPostedAgain: courier is killed between posting a reply and
// recording that it did (the testhooks fault, exit 137), and after it restarts the thread holds that one
// message, found again by the tag it carries, not a second.
//
// Arming a fault is a rollout, and the fault stays armed across the container's own restarts, so the
// scenario disarms at once, with another rollout, once it has seen the crash; and a failed scenario
// disarms too. The channel's Binding is output final, because only a post through the outbox reaches the
// hook: a live reply to a person is streamed, which carries the same tag but no hook.
func TestS5_AReplyPostedBeforeACrashIsNotPostedAgain(t *testing.T) {
	s := newScenario(t)
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
	base, err := s.pods.Baseline(s.ctx, kube.CourierNamespace, courierPods, courierContainer)
	if err != nil {
		t.Fatal(err)
	}
	if err := arm(s.ctx, testhooks.CrashAfterPost); err != nil {
		t.Fatal(err)
	}
	s.connected()

	m := s.mention(ChannelS5, "US5", "answer once "+s.stub.Gate("S5"))
	crashCtx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	crash, err := s.pods.WaitCrash(crashCtx, kube.CourierNamespace, courierPods, courierContainer, base)
	if err != nil {
		t.Fatalf("courier never crashed after posting: %v\nthe thread: %s", err, s.describeNow(m.TS))
	}
	if crash.ExitCode != 137 {
		t.Fatalf("courier ended with exit code %d (%s); want 137, the fault's", crash.ExitCode, crash.Reason)
	}
	if err := arm(s.ctx, ""); err != nil { // at once: the restarted container has the fault armed too
		t.Fatal(err)
	}
	s.connected()

	// The reply was posted before the crash, and is there.
	s.thread(ChannelS5, m.TS, "the reply that was posted before the crash", func(th []fakeslack.MessageView) bool {
		return len(BotMessages(th)) >= 1
	})
	// And after the restart courier finds it by its tag, and does not post it again. That nothing more
	// comes is a thing to watch for: courier says nothing when it finds the message.
	err = Consistently(s.ctx, 10*time.Second, "the thread keeps exactly one message of the bot, with the outbox tag", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelS5, m.TS)
		bots := BotMessages(th)
		return len(bots) == 1 && OutboxID(bots[0]) != "" && strings.TrimSpace(bots[0].Text) != "", err
	})
	if err != nil {
		t.Errorf("%v\nthe thread: %s", err, s.describeNow(m.TS))
	}
}

// describeNow is the thread at ts in S5's channel as text, for a failure message; a thread that cannot be
// read says so.
func (s *scenario) describeNow(ts string) string {
	th, err := s.slack.Thread(context.WithoutCancel(s.ctx), ChannelS5, ts)
	if err != nil {
		return "(unreadable: " + err.Error() + ")"
	}
	return describe(th)
}
