//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// TestS19_AnApprovalRunsTheTool: the approval-enabled Binding lets a person approve the stub's tool request. The
// edited prompt records the exact tool and verdict, the stub's result reaches the thread, and the decision is counted.
func TestS19_AnApprovalRunsTheTool(t *testing.T) {
	runFastToolApproval(t, fastToolApprovalCase{
		name:         "approve",
		channel:      ChannelS19,
		user:         "US19",
		binding:      "agent-kourier/fast-s19",
		action:       chat.ActionApproveAll,
		verdict:      "Approved",
		continuation: "I have added 3 and 5. The result is 8.",
		outcome:      "approved",
		auditOutcome: `answered approved=1 denied=0 approved_tools=["add_numbers"]`,
	})
}

// TestS19_AnApprovalRunsTheToolOverTheA2ADialect: S19 on an a2a Binding. The stub's card lists kagent's HITL extension, so
// the generic dialect asks for it, reads the structured approval and sends the structured decision.
func TestS19_AnApprovalRunsTheToolOverTheA2ADialect(t *testing.T) {
	runFastToolApproval(t, fastToolApprovalCase{
		name:         "approve-a2a",
		channel:      ChannelA2AS19,
		user:         "UA2AS19",
		binding:      "agent-kourier/fast-a2a-s19",
		action:       chat.ActionApproveAll,
		verdict:      "Approved",
		continuation: "I have added 3 and 5. The result is 8.",
		outcome:      "approved",
		auditOutcome: `answered approved=1 denied=0 approved_tools=["add_numbers"]`,
	})
}

// TestS20_ADenialContinuesTheTurn: the approval-enabled Binding sends a denial to the stub. The edited prompt records
// the denied tool, the stub's rejection continuation reaches the thread, and the decision is counted.
func TestS20_ADenialContinuesTheTurn(t *testing.T) {
	runFastToolApproval(t, fastToolApprovalCase{
		name:         "deny",
		channel:      ChannelS20,
		user:         "US20",
		binding:      "agent-kourier/fast-s20",
		action:       chat.ActionDenyAll,
		verdict:      "Denied",
		continuation: "Resumed after the tool result.",
		outcome:      "denied",
		auditOutcome: `answered approved=0 denied=1 denied_tools=["add_numbers"]`,
	})
}

type fastToolApprovalCase struct {
	name, channel, user, binding   string
	action                         chat.Action
	verdict, continuation, outcome string
	auditOutcome                   string
}

func runFastToolApproval(t *testing.T, tc fastToolApprovalCase) {
	t.Helper()
	s := newScenario(t)
	if s.env.Tier != "fast" {
		t.Skip("S19/S20 drive the fast stub approval agent; the full tier uses real-kagent scenarios and AX exposes plain-text questions")
	}
	labels := map[string]string{"binding": tc.binding, "outcome": tc.outcome}
	before := s.counter(approvalMetric, labels)
	beforeAudit := s.auditEntries(s.ctx)

	s.know(tc.user)
	if err := s.slack.SetMembers(s.ctx, tc.channel, tc.user); err != nil {
		t.Fatal(err)
	}
	m := s.mention(tc.channel, tc.user, "use the approval tool "+s.stub.Gate(tc.name))
	th := s.thread(tc.channel, m.TS, "the tool approval prompt", func(th []fakeslack.MessageView) bool {
		_, ok := ApprovalPrompt(th)
		return ok
	})
	prompt, ok := ApprovalPrompt(th)
	if !ok {
		t.Fatal("the thread has no approval prompt")
	}
	envelope, err := s.slack.PressApproval(s.ctx, tc.user, prompt, tc.action)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.slack.AckWait(s.ctx, 15*time.Second, envelope); err != nil {
		t.Fatalf("Agent Kourier never acked the approval press: %v", err)
	}

	th = s.thread(tc.channel, m.TS, "the decided approval and agent continuation", func(th []fakeslack.MessageView) bool {
		var ending, continuation bool
		for _, b := range BotMessages(th) {
			if b.TS == prompt.TS {
				ending = b.Edited != nil && b.Text == "Answered" && !HasButton(b.Blocks, chat.ActionApproveAll.String()) &&
					!HasButton(b.Blocks, chat.ActionDenyAll.String()) && strings.Contains(string(b.Blocks), "add_numbers") &&
					strings.Contains(string(b.Blocks), tc.verdict) && HasBlockText(b.Blocks, "<@"+tc.user+">")
			}
			continuation = continuation || (finished(b) && strings.Contains(b.Text, tc.continuation))
		}
		return ending && continuation
	})
	for _, b := range BotMessages(th) {
		if b.TS != prompt.TS && strings.Contains(b.Text, "I have added 3 and 5") && tc.outcome == "denied" {
			t.Errorf("the denied approval says %q: the tool result reached the thread", b.Text)
		}
		if b.TS != prompt.TS && strings.Contains(b.Text, "Resumed after the tool result") && tc.outcome == "approved" {
			t.Errorf("the approved approval says %q: the rejection continuation reached the thread", b.Text)
		}
	}
	waitApprovalMetric(t, s, approvalMetric, labels, before)
	decision := waitApprovalAudit(t, s, beforeAudit, tc.binding)
	assertApprovalAudit(t, decision, tc.user, tc.auditOutcome)
}

func (s *scenario) auditEntries(ctx context.Context) []store.AuditEntry {
	s.t.Helper()
	entries, err := s.leaderAuditEntries(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	return entries
}
