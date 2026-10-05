//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// TestS11_AToolApprovalOnABindingThatDoesNotAllowThemIsDenied: the agent asks to approve a tool call on a
// Binding with toolApprovals off (all of Phase 1). Courier denies it, says so in the thread in words of its
// own, the agent goes on without the tool, and the approval-outcome counter goes up by one.
//
// UNCONFIRMED(WP-02): that the agent's continuation after a denial is the text spike-approve gives it
// ("Resumed after the tool result."), which is what the stub does with a denied approval, and not what
// kagent does.
func TestS11_AToolApprovalOnABindingThatDoesNotAllowThemIsDenied(t *testing.T) {
	s := newScenario(t)
	if s.env.Tier == "ax" {
		t.Skip("AX/ADK exposes plain-text questions; structured tool approval metadata is a kagent capability")
	}
	labels := map[string]string{"binding": "courier/fast-s11", "outcome": "auto_denied"}
	const metric = "courier_approval_outcomes_total"
	before := s.counter(metric, labels)

	m := s.mention(ChannelS11, "US11", "add 3 and 5 "+s.stub.Gate("S11"))
	th := s.thread(ChannelS11, m.TS, "the notice and the agent's continuation", func(th []fakeslack.MessageView) bool {
		var notice, resumed bool
		for _, b := range BotMessages(th) {
			notice = notice || strings.Contains(b.Text, "does not allow approvals")
			resumed = resumed || (finished(b) && strings.Contains(b.Text, "Resumed after the tool result."))
		}
		return notice && resumed
	})
	for _, b := range BotMessages(th) {
		if strings.Contains(b.Text, "I have added 3 and 5") {
			t.Errorf("the thread says %q: the tool ran, though the Binding does not allow it", b.Text)
		}
	}
	err := Wait(s.ctx, 30*time.Second, "the approval-outcome counter to go up", func(ctx context.Context) (bool, error) {
		v, err := s.leaderCounter(ctx, metric, labels)
		return v > before, err
	})
	if err != nil {
		t.Fatalf("%v (it was %v)", err, before)
	}
	if got := s.counter(metric, labels); got != before+1 {
		t.Errorf("%s%v went from %v to %v; want one more: the denial is counted once", metric, labels, before, got)
	}
}
