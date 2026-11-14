//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// The prompt agent's scenarios: the recipe's agent (examples/recipes/prompt-agent), reached through the a2a dialect with the bearer it
// requires, on the fast tier's fake model (fake_model.py in the recipe), with the recipe's test MCP server. The fake answers by rules read
// from the request alone: "runbook for NAME" calls the MCP tool runbook, "ask me" asks a question, a tool result is quoted, and anything
// else says how many messages of the person's the conversation holds and what the first was, which only the agent's history can tell.
// Each scenario has a thread of its own in ChannelPromptAgent, and its text is unique to the run.

// timedOutNotice is what Agent Kourier tells a thread whose question no one answered in time (internal/interact's sweep).
const timedOutNotice = "No one answered this question in time, so it was closed."

// promptQuestion is the question the fake model asks for "ask me".
const promptQuestion = "Which environment should I check?"

// promptScenario is newScenario for the prompt agent's scenarios: the agent and its Binding are the fast tier's alone.
func promptScenario(t *testing.T) *scenario {
	t.Helper()
	s := newScenario(t)
	if s.env.Tier != "fast" {
		t.Skip("the prompt agent runs in the fast tier only: the " + s.env.Tier + " tier does not bring it up")
	}
	return s
}

// lastBot is the newest message of the bot in a thread, and whether there is one.
func lastBot(th []fakeslack.MessageView) (fakeslack.MessageView, bool) {
	bots := BotMessages(th)
	if len(bots) == 0 {
		return fakeslack.MessageView{}, false
	}
	return bots[len(bots)-1], true
}

// questionOf is the prompt of the agent's text question in a thread: a bot message with a Submit button that asks promptQuestion.
func questionOf(th []fakeslack.MessageView) (fakeslack.MessageView, bool) {
	for _, b := range BotMessages(th) {
		if HasButton(b.Blocks, submitAction) && strings.Contains(string(b.Blocks), promptQuestion) {
			return b, true
		}
	}
	return fakeslack.MessageView{}, false
}

// promptBinding is the Binding of ChannelPromptAgent, as the audit log names it.
const promptBinding = "agent-kourier/fast-prompt-a2a"

// promptCancels is the CancelTask calls audited for the prompt agent's Binding after the entries of before: the scenarios run one at a time,
// so they are the running scenario's.
func promptCancels(entries, before []store.AuditEntry) []store.AuditEntry {
	var sequence int64
	for _, e := range before {
		sequence = max(sequence, e.Sequence)
	}
	var out []store.AuditEntry
	for _, e := range entries {
		if e.Sequence > sequence && e.Kind == store.AuditAgentCall && e.Binding == promptBinding && strings.HasPrefix(e.Outcome, "cancel:") {
			out = append(out, e)
		}
	}
	return out
}

// TestA2A_ThePromptAgentStreamsAReplyThatUsedAnMCPTool: a mention makes the agent call the MCP tool runbook, and its reply, which quotes
// what the MCP server returned, streams into the thread: it is seen while it streams, with part of its text, and then whole. "Runbook for
// NAME:" is the MCP server's text, so the reply shows the call reached it, with the agent's token, and came back.
func TestA2A_ThePromptAgentStreamsAReplyThatUsedAnMCPTool(t *testing.T) {
	s := promptScenario(t)
	alert := "Disk" + s.stub.Gate("PROMPTMCP")
	m := s.mention(ChannelPromptAgent, "UPROMPTA", "runbook for "+alert)

	s.thread(ChannelPromptAgent, m.TS, "the reply, while it streams", func(th []fakeslack.MessageView) bool {
		b, ok := lastBot(th)
		return ok && b.Streaming && strings.Contains(b.Text, "The runbook says:") && !strings.Contains(b.Text, "memory limit.")
	})
	th := s.thread(ChannelPromptAgent, m.TS, "the whole reply", func(th []fakeslack.MessageView) bool {
		b, ok := lastBot(th)
		return ok && finished(b) && strings.Contains(b.Text, "memory limit.")
	})
	reply, _ := lastBot(th)
	if want := "Runbook for " + alert + ": check the pod"; !strings.Contains(reply.Text, want) || reply.ThreadTS != m.TS {
		t.Errorf("the reply is %q in thread %q; want it to quote the MCP server's %q, in the mention's thread %q", reply.Text, reply.ThreadTS, want, m.TS)
	}
}

// TestA2A_ThePromptAgentContinuesTheSessionInAThread: a reply in the thread is the next turn of the same conversation: the agent's answer
// counts two messages of the person's and quotes the first, which only the history it kept for the thread's contextId holds.
func TestA2A_ThePromptAgentContinuesTheSessionInAThread(t *testing.T) {
	s := promptScenario(t)
	first := "hello " + s.stub.Gate("PROMPTCTX")
	m := s.mention(ChannelPromptAgent, "UPROMPTB", first)
	s.thread(ChannelPromptAgent, m.TS, "the first reply", func(th []fakeslack.MessageView) bool {
		b, ok := lastBot(th)
		return ok && finished(b) && strings.Contains(b.Text, "Messages from you in this conversation: 1. The first: "+first)
	})
	s.reply(ChannelPromptAgent, m.TS, "UPROMPTB", "and what did I say first?")
	s.thread(ChannelPromptAgent, m.TS, "the second reply, a message of its own, which quotes the first turn", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 2 && finished(bots[1]) && strings.Contains(bots[1].Text, "Messages from you in this conversation: 2. The first: "+first)
	})
}

// TestA2A_ThePromptAgentsQuestionIsAnsweredByATypedReply: the agent asks with its ask_user tool, which the a2a dialect shows as a text
// question; a member of the channel answers with a reply in the thread, and the same task goes on with that answer and completes.
func TestA2A_ThePromptAgentsQuestionIsAnsweredByATypedReply(t *testing.T) {
	s := promptScenario(t)
	const user = "UPROMPTC"
	s.know(user)
	if err := s.slack.SetMembers(s.ctx, ChannelPromptAgent, user); err != nil { // only a member may answer
		t.Fatal(err)
	}
	m := s.mention(ChannelPromptAgent, user, "ask me which environment "+s.stub.Gate("PROMPTASK"))
	th := s.thread(ChannelPromptAgent, m.TS, "the question", func(th []fakeslack.MessageView) bool { _, ok := questionOf(th); return ok })
	question, _ := questionOf(th)

	answer := "staging-" + s.stub.Gate("PROMPTANSWER")
	s.reply(ChannelPromptAgent, m.TS, user, answer)
	th = s.thread(ChannelPromptAgent, m.TS, "the answer to be taken and the task to complete", func(th []fakeslack.MessageView) bool {
		var closed, completed bool
		for _, b := range BotMessages(th) {
			closed = closed || (b.TS == question.TS && !HasAction(b.Blocks, submitAction))
			completed = completed || (finished(b) && strings.Contains(b.Text, "You answered: "+answer))
		}
		return closed && completed
	})
	err := Consistently(s.ctx, 3*time.Second, "exactly one continuation after the answer", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelPromptAgent, m.TS)
		n := 0
		for _, b := range BotMessages(th) {
			if strings.Contains(b.Text, "You answered:") {
				n++
			}
		}
		return n == 1, err
	})
	if err != nil {
		t.Errorf("%v\nthe thread:%s", err, describe(th))
	}
}

// TestA2A_ThePromptAgentsQuestionLeftToTimeOutIsCancelledAndTheThreadGoesOn: no one answers the agent's question, so at the Binding's
// timeout (20 seconds) Agent Kourier cancels the paused task, closes the prompt and tells the thread. The audit log holds the CancelTask
// call, which the agent answered without an error: the notice alone would not show it, since a pause released without a cancel leaves the
// next turn as it is. A mention in the thread afterwards is answered, from the same conversation: the agent repaired the history that
// the cancelled question left with a tool call unanswered.
func TestA2A_ThePromptAgentsQuestionLeftToTimeOutIsCancelledAndTheThreadGoesOn(t *testing.T) {
	s := promptScenario(t)
	const user = "UPROMPTD"
	before, err := s.leaderAuditEntries(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := "ask me which environment " + s.stub.Gate("PROMPTTIMEOUT")
	m := s.mention(ChannelPromptAgent, user, first)
	th := s.thread(ChannelPromptAgent, m.TS, "the question", func(th []fakeslack.MessageView) bool { _, ok := questionOf(th); return ok })
	question, _ := questionOf(th)

	s.thread(ChannelPromptAgent, m.TS, "the timeout: the prompt closed and the thread told", func(th []fakeslack.MessageView) bool {
		var closed, told bool
		for _, b := range BotMessages(th) {
			closed = closed || (b.TS == question.TS && !HasAction(b.Blocks, submitAction))
			told = told || strings.Contains(b.Text, timedOutNotice)
		}
		return closed && told
	})
	err = Wait(s.ctx, 30*time.Second, "the audit of the agent's paused task cancelled", func(ctx context.Context) (bool, error) {
		entries, err := s.leaderAuditEntries(ctx)
		return len(promptCancels(entries, before)) > 0, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := s.leaderAuditEntries(s.ctx); err != nil {
		t.Fatal(err)
	} else if rows := promptCancels(entries, before); len(rows) != 1 || rows[0].Outcome != "cancel: ok" || !strings.Contains(rows[0].Subject, "task=") {
		t.Errorf("the cancels audited for the prompt agent's Binding since the scenario began are %+v; want one, of a task, which the agent took (cancel: ok)", rows)
	}

	s.reply(ChannelPromptAgent, m.TS, user, botMention+"are you still there?")
	th = s.thread(ChannelPromptAgent, m.TS, "an answer to the mention after the timeout", func(th []fakeslack.MessageView) bool {
		b, ok := lastBot(th)
		return ok && finished(b) && strings.Contains(b.Text, "Messages from you in this conversation:")
	})
	if b, _ := lastBot(th); !strings.Contains(b.Text, "Messages from you in this conversation: 2. The first: "+first) {
		t.Errorf("the answer after the timeout is %q; want the second turn of the thread's conversation, which began with %q", b.Text, first)
	}
}
