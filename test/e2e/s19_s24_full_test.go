//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

type fullApprovalCase struct {
	channel, user, binding, scenario, tool string
	action                                 chat.Action
	verdict, outcome, auditOutcome         string
	continuation                           string
	mcpDelta                               int
}

func fullApprovalScenario(t *testing.T) *scenario {
	t.Helper()
	s := newScenario(t)
	if s.env.Tier != "full" {
		t.Skip("S19-S24 drive the full tier's kagent approval scenarios")
	}
	return s
}

// TestS19_FullApprovalRunsTheTool covers the approval path through kagent and the scripted MCP server.
func TestS19_FullApprovalRunsTheTool(t *testing.T) {
	runFullApproval(t, fullApprovalCase{
		channel:      ChannelS19,
		user:         "US19",
		binding:      "agent-kourier/full-s19",
		scenario:     "approval",
		tool:         "add_numbers",
		action:       chat.ActionApproveAll,
		verdict:      "Approved",
		outcome:      "approved",
		auditOutcome: `answered approved=1 denied=0 approved_tools=["add_numbers"]`,
		continuation: "Approval result received.",
		mcpDelta:     1,
	})
}

// TestS20_FullDenialContinuesTheTurn covers the denial path without executing the tool.
func TestS20_FullDenialContinuesTheTurn(t *testing.T) {
	runFullApproval(t, fullApprovalCase{
		channel:      ChannelS20,
		user:         "US20",
		binding:      "agent-kourier/full-s20",
		scenario:     "approval",
		tool:         "add_numbers",
		action:       chat.ActionDenyAll,
		verdict:      "Denied",
		outcome:      "denied",
		auditOutcome: `answered approved=0 denied=1 denied_tools=["add_numbers"]`,
		continuation: "Approval result received.",
		mcpDelta:     0,
	})
}

func runFullApproval(t *testing.T, tc fullApprovalCase) {
	t.Helper()
	s := fullApprovalScenario(t)
	labels := map[string]string{"binding": tc.binding, "outcome": tc.outcome}
	metricBefore := s.counter(approvalMetric, labels)
	auditBefore := s.auditEntries(s.ctx)
	mcpBefore := s.mcpCount(tc.scenario, tc.tool)

	if err := s.slack.SetMembers(s.ctx, tc.channel, tc.user); err != nil {
		t.Fatal(err)
	}
	m := s.mention(tc.channel, tc.user, "use the approval tool")
	prompt := s.thread(tc.channel, m.TS, "the full-tier approval prompt", func(thread []fakeslack.MessageView) bool {
		_, ok := ApprovalPrompt(thread)
		return ok
	})
	approval, ok := ApprovalPrompt(prompt)
	if !ok {
		t.Fatal("the thread has no full-tier approval prompt")
	}
	envelope, err := s.slack.PressApproval(s.ctx, tc.user, approval, tc.action)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.slack.AckWait(s.ctx, 15*time.Second, envelope); err != nil {
		t.Fatalf("Agent Kourier never acked the approval press: %v", err)
	}

	thread := s.thread(tc.channel, m.TS, "the decided full-tier approval and continuation", func(thread []fakeslack.MessageView) bool {
		return fullApprovalAnswered(thread, approval, tc.user, tc.verdict) && fullApprovalContinued(thread, tc.continuation)
	})
	if !fullApprovalAnswered(thread, approval, tc.user, tc.verdict) {
		t.Errorf("full-tier approval prompt was not replaced with the %s answer", tc.verdict)
	}

	waitApprovalMetric(t, s, approvalMetric, labels, metricBefore)
	s.waitMCPCount(tc.scenario, tc.tool, mcpBefore, tc.mcpDelta)
	decision := waitApprovalAudit(t, s, auditBefore, tc.binding)
	assertApprovalAudit(t, decision, tc.user, tc.auditOutcome)
}

// TestS21_FullApprovalTimesOut verifies the sweep, persisted timeout audit, and no tool execution.
func TestS21_FullApprovalTimesOut(t *testing.T) {
	s := fullApprovalScenario(t)
	const (
		user    = "US21"
		binding = "agent-kourier/full-s21"
		script  = "approval"
		tool    = "add_numbers"
	)
	labels := map[string]string{"binding": binding, "outcome": "timed_out"}
	metricBefore := s.counter(approvalMetric, labels)
	auditBefore := s.auditEntries(s.ctx)
	mcpBefore := s.mcpCount(script, tool)
	if err := s.slack.SetMembers(s.ctx, ChannelS21, user); err != nil {
		t.Fatal(err)
	}
	m := s.mention(ChannelS21, user, "let the approval time out")
	promptThread := s.thread(ChannelS21, m.TS, "the timeout approval prompt", func(thread []fakeslack.MessageView) bool {
		_, ok := ApprovalPrompt(thread)
		return ok
	})
	prompt, ok := ApprovalPrompt(promptThread)
	if !ok {
		t.Fatal("the timeout thread has no approval prompt")
	}
	thread := s.thread(ChannelS21, m.TS, "the timed-out approval and its denial continuation", func(thread []fakeslack.MessageView) bool {
		return fullApprovalTimedOut(thread, prompt) && fullApprovalContinued(thread, "Approval result received.")
	})
	if !fullApprovalTimedOut(thread, prompt) {
		t.Error("full-tier timeout prompt was not replaced with Timed out")
	}
	waitApprovalMetric(t, s, approvalMetric, labels, metricBefore)
	s.waitMCPCount(script, tool, mcpBefore, 0)
	decision := waitApprovalAudit(t, s, auditBefore, binding)
	assertApprovalAudit(t, decision, "", "timed_out")
}

// TestS22_FullApprovalChecksMembershipAndApproverGroup exercises both refusal audits before an approver wins.
func TestS22_FullApprovalChecksMembershipAndApproverGroup(t *testing.T) {
	s := fullApprovalScenario(t)
	const (
		member  = "US22"
		outside = "UOUTSIDE22"
		group   = "S0123456789"
		binding = "agent-kourier/full-s22"
		script  = "approval"
		tool    = "add_numbers"
	)
	labels := map[string]string{"binding": binding, "outcome": "approved"}
	metricBefore := s.counter(approvalMetric, labels)
	auditBefore := s.auditEntries(s.ctx)
	mcpBefore := s.mcpCount(script, tool)
	s.know(outside)
	if err := s.slack.SetMembers(s.ctx, ChannelS22, member); err != nil {
		t.Fatal(err)
	}
	if err := s.slack.SetUserGroup(s.ctx, group); err != nil {
		t.Fatal(err)
	}
	m := s.mention(ChannelS22, member, "check approval authorization")
	prompt := s.thread(ChannelS22, m.TS, "the group-gated approval prompt", func(thread []fakeslack.MessageView) bool {
		_, ok := ApprovalPrompt(thread)
		return ok
	})
	approval, ok := ApprovalPrompt(prompt)
	if !ok {
		t.Fatal("the group-gated thread has no approval prompt")
	}

	press, err := s.slack.PressApproval(s.ctx, member, approval, chat.ActionApproveAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.slack.AckWait(s.ctx, 15*time.Second, press); err != nil {
		t.Fatalf("Agent Kourier never acked the non-approver press: %v", err)
	}
	waitFullEphemeral(t, s, ChannelS22, m.TS, member, "Only approvers of this Binding can decide this request.")
	decision := waitApprovalAudit(t, s, auditBefore, binding)
	assertApprovalAudit(t, decision, member, "denied_not_approver")
	auditBefore = s.auditEntries(s.ctx)

	press, err = s.slack.PressApproval(s.ctx, outside, approval, chat.ActionApproveAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.slack.AckWait(s.ctx, 15*time.Second, press); err != nil {
		t.Fatalf("Agent Kourier never acked the non-member press: %v", err)
	}
	decision = waitApprovalAudit(t, s, auditBefore, binding)
	assertApprovalAudit(t, decision, outside, "denied")
	auditBefore = s.auditEntries(s.ctx)

	if err := s.slack.SetUserGroup(s.ctx, group, member); err != nil {
		t.Fatal(err)
	}
	press, err = s.slack.PressApproval(s.ctx, member, approval, chat.ActionApproveAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.slack.AckWait(s.ctx, 15*time.Second, press); err != nil {
		t.Fatalf("Agent Kourier never acked the authorized press: %v", err)
	}
	s.thread(ChannelS22, m.TS, "the authorized group approval", func(thread []fakeslack.MessageView) bool {
		return fullApprovalAnswered(thread, approval, member, "Approved") && fullApprovalContinued(thread, "Approval result received.")
	})
	waitApprovalMetric(t, s, approvalMetric, labels, metricBefore)
	s.waitMCPCount(script, tool, mcpBefore, 1)
	decision = waitApprovalAudit(t, s, auditBefore, binding)
	assertApprovalAudit(t, decision, member, `answered approved=1 denied=0 approved_tools=["add_numbers"]`)
}

// TestS23_FullApprovalCanDecideEachTool verifies independent verdicts and one partial outcome metric.
func TestS23_FullApprovalCanDecideEachTool(t *testing.T) {
	s := fullApprovalScenario(t)
	const (
		user     = "US23"
		binding  = "agent-kourier/full-s23"
		script   = "approval-multi"
		approved = "add_numbers"
		denied   = "check_status"
	)
	labels := map[string]string{"binding": binding, "outcome": "partially_approved"}
	metricBefore := s.counter(approvalMetric, labels)
	auditBefore := s.auditEntries(s.ctx)
	approvedBefore := s.mcpCount(script, approved)
	deniedBefore := s.mcpCount(script, denied)
	if err := s.slack.SetMembers(s.ctx, ChannelS23, user); err != nil {
		t.Fatal(err)
	}
	m := s.mention(ChannelS23, user, "use both approval tools")
	prompt := s.thread(ChannelS23, m.TS, "the multi-tool approval prompt", func(thread []fakeslack.MessageView) bool {
		approval, ok := ApprovalPrompt(thread)
		if !ok {
			return false
		}
		_, _, _, second := QuestionControl(approval.Blocks, 1)
		return second
	})
	approval, ok := ApprovalPrompt(prompt)
	if !ok {
		t.Fatal("the multi-tool thread has no approval prompt")
	}
	envelope, err := s.slack.PressApproval(s.ctx, user, approval, chat.ActionSubmit, chat.ToolApproved, chat.ToolDenied)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.slack.AckWait(s.ctx, 15*time.Second, envelope); err != nil {
		t.Fatalf("Agent Kourier never acked the multi-tool approval: %v", err)
	}
	s.thread(ChannelS23, m.TS, "the decided multi-tool approval", func(thread []fakeslack.MessageView) bool {
		return fullApprovalPartial(thread, approval, user, approved, denied) && fullApprovalContinued(thread, "Approval results received.")
	})
	waitApprovalMetric(t, s, approvalMetric, labels, metricBefore)
	s.waitMCPCount(script, approved, approvedBefore, 1)
	s.waitMCPCount(script, denied, deniedBefore, 0)
	auditOutcome := `answered approved=1 denied=1 approved_tools=["add_numbers"] denied_tools=["check_status"]`
	decision := waitApprovalAudit(t, s, auditBefore, binding)
	assertApprovalAudit(t, decision, user, auditOutcome)
}

// TestS24_FullApprovalIgnoresADuplicatePress verifies first-wins ordering when a second channel member presses a stale prompt.
func TestS24_FullApprovalIgnoresADuplicatePress(t *testing.T) {
	s := fullApprovalScenario(t)
	const (
		winner  = "US24"
		loser   = "US24DENY"
		binding = "agent-kourier/full-s24"
		script  = "approval"
		tool    = "add_numbers"
	)
	labels := map[string]string{"binding": binding, "outcome": "approved"}
	deniedLabels := map[string]string{"binding": binding, "outcome": "denied"}
	metricBefore := s.counter(approvalMetric, labels)
	deniedMetricBefore := s.counter(approvalMetric, deniedLabels)
	auditBefore := s.auditEntries(s.ctx)
	mcpBefore := s.mcpCount(script, tool)
	s.know(winner)
	s.know(loser)
	if err := s.slack.SetMembers(s.ctx, ChannelS24, winner, loser); err != nil {
		t.Fatal(err)
	}
	m := s.mention(ChannelS24, winner, "press approval twice")
	prompt := s.thread(ChannelS24, m.TS, "the duplicate-press approval prompt", func(thread []fakeslack.MessageView) bool {
		_, ok := ApprovalPrompt(thread)
		return ok
	})
	approval, ok := ApprovalPrompt(prompt)
	if !ok {
		t.Fatal("the duplicate-press thread has no approval prompt")
	}
	first, err := s.slack.PressApproval(s.ctx, winner, approval, chat.ActionApproveAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.slack.AckWait(s.ctx, 15*time.Second, first); err != nil {
		t.Fatalf("Agent Kourier never acked the first duplicate press: %v", err)
	}
	s.thread(ChannelS24, m.TS, "the first duplicate press winning", func(thread []fakeslack.MessageView) bool {
		return fullApprovalAnswered(thread, approval, winner, "Approved") && fullApprovalContinued(thread, "Approval result received.")
	})
	second, err := s.slack.PressStaleApproval(s.ctx, loser, approval, chat.ActionDenyAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.slack.AckWait(s.ctx, 15*time.Second, second); err != nil {
		t.Fatalf("Agent Kourier never acked the second duplicate press: %v", err)
	}
	waitFullEphemeralWithWinner(t, s, ChannelS24, m.TS, loser, winner, "decided this request first.")
	rows := waitApprovalAudits(t, s, auditBefore, binding, 1)
	wantAnswered := `answered approved=1 denied=0 approved_tools=["add_numbers"]`
	if len(rows) != 1 || rows[0].Outcome != wantAnswered {
		t.Fatalf("duplicate approval audits = %+v; want exactly the winner's answered row", rows)
	}
	assertApprovalAudit(t, rows[0], winner, wantAnswered)
	waitApprovalMetric(t, s, approvalMetric, labels, metricBefore)
	s.waitMCPCount(script, tool, mcpBefore, 1)
	if got := s.counter(approvalMetric, deniedLabels); got != deniedMetricBefore {
		t.Errorf("duplicate deny press changed %s%v from %v to %v; want no increment", approvalMetric, deniedLabels, deniedMetricBefore, got)
	}
}

func fullApprovalAnswered(thread []fakeslack.MessageView, prompt fakeslack.MessageView, user, verdict string) bool {
	for _, message := range BotMessages(thread) {
		if message.TS == prompt.TS && message.Edited != nil && message.Text == "Answered" &&
			!HasButton(message.Blocks, chat.ActionApproveAll.String()) && !HasButton(message.Blocks, chat.ActionDenyAll.String()) &&
			strings.Contains(string(message.Blocks), "add_numbers") && strings.Contains(string(message.Blocks), verdict) &&
			HasBlockText(message.Blocks, "<@"+user+">") {
			return true
		}
	}
	return false
}

func fullApprovalTimedOut(thread []fakeslack.MessageView, prompt fakeslack.MessageView) bool {
	for _, message := range BotMessages(thread) {
		if message.TS == prompt.TS && message.Edited != nil && message.Text == "Timed out" &&
			!HasButton(message.Blocks, chat.ActionApproveAll.String()) && !HasButton(message.Blocks, chat.ActionDenyAll.String()) {
			return true
		}
	}
	return false
}

func fullApprovalPartial(thread []fakeslack.MessageView, prompt fakeslack.MessageView, user, approved, denied string) bool {
	for _, message := range BotMessages(thread) {
		if message.TS == prompt.TS && message.Edited != nil && message.Text == "Answered" &&
			!HasButton(message.Blocks, chat.ActionApproveAll.String()) && !HasButton(message.Blocks, chat.ActionDenyAll.String()) &&
			strings.Contains(string(message.Blocks), approved) && strings.Contains(string(message.Blocks), denied) &&
			strings.Contains(string(message.Blocks), "Approved") && strings.Contains(string(message.Blocks), "Denied") &&
			HasBlockText(message.Blocks, "<@"+user+">") {
			return true
		}
	}
	return false
}

func fullApprovalContinued(thread []fakeslack.MessageView, text string) bool {
	for _, message := range BotMessages(thread) {
		if finished(message) && strings.Contains(message.Text, text) {
			return true
		}
	}
	return false
}

func waitFullEphemeral(t *testing.T, s *scenario, channel, thread, recipient, text string) {
	t.Helper()
	err := Wait(s.ctx, 30*time.Second, "the approval refusal notice", func(ctx context.Context) (bool, error) {
		notices, err := s.slack.Ephemerals(ctx, channel)
		if err != nil {
			return false, err
		}
		for _, notice := range notices {
			if notice.Recipient == recipient && notice.ThreadTS == thread && strings.Contains(notice.Text, text) {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func waitFullEphemeralWithWinner(t *testing.T, s *scenario, channel, thread, recipient, winner, text string) {
	t.Helper()
	err := Wait(s.ctx, 30*time.Second, "the approval refusal notice naming the winner", func(ctx context.Context) (bool, error) {
		notices, err := s.slack.Ephemerals(ctx, channel)
		if err != nil {
			return false, err
		}
		for _, notice := range notices {
			if notice.Recipient == recipient && notice.ThreadTS == thread &&
				strings.Contains(notice.Text, text) && HasBlockText(notice.Blocks, "<@"+winner+">") {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (s *scenario) mcpCount(scenario, tool string) int {
	s.t.Helper()
	counters, err := s.mcpCounters(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	return mcpCount(counters, scenario, tool)
}
