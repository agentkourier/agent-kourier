//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// submitAction is the Submit button of a prompt, which is the one action of it that a press names.
const submitAction = "submit"

// promptOf is the prompt in a thread: the bot's message with a Submit button, or false.
func promptOf(th []fakeslack.MessageView) (fakeslack.MessageView, bool) {
	for _, m := range BotMessages(th) {
		if HasButton(m.Blocks, submitAction) {
			return m, true
		}
	}
	return fakeslack.MessageView{}, false
}

// TestS3_AnAskUserPromptTakesACompleteAnswerAndRefusesAnIncompleteOne: the agent asks two questions, a
// multiple choice and a free-text one, courier posts them as a prompt with a Submit button, and:
//   - a Submit with the second question unanswered is told to its sender alone, by number, and leaves the
//     prompt as it was, so what the others have typed is not reset, and the agent is not told anything;
//   - a complete Submit decides the question: the prompt is edited to its decided content, with no button, and
//     the agent goes on with exactly the answers that were given.
//
// UNCONFIRMED(D8): whether the paused task's final event makes the renderer post the question as a message
// of its own besides the prompt. The scenario finds the prompt by its Submit button and counts no messages.
// UNCONFIRMED(WP-04): that Slack sends the inputs' state with a button in a message, as the fake does.
func TestS3_AnAskUserPromptTakesACompleteAnswerAndRefusesAnIncompleteOne(t *testing.T) {
	s := newScenario(t)
	const user = "US3"
	// Only a member of the channel who is not a bot may answer: courier asks Slack.
	if err := s.slack.AddUser(s.ctx, fakeslack.User{ID: user, Name: "s3user", RealName: "S3 User"}); err != nil {
		t.Fatal(err)
	}
	if err := s.slack.SetMembers(s.ctx, ChannelS3, user); err != nil {
		t.Fatal(err)
	}
	m := s.mention(ChannelS3, user, "two questions "+s.stub.Gate("S3"))
	th := s.thread(ChannelS3, m.TS, "the prompt", func(th []fakeslack.MessageView) bool { _, ok := promptOf(th); return ok })
	prompt, _ := promptOf(th)
	press := func(values fakeslack.StateValues) {
		t.Helper()
		id, err := s.slack.Submit(s.ctx, user, prompt.TS, submitAction, values)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.slack.AckWait(s.ctx, 15*time.Second, id); err != nil {
			t.Fatalf("courier never acked the press: %v", err)
		}
	}

	// Incomplete: question 1 is answered, question 2 is not.
	press(fakeslack.StateValues{"q0": {"a0": ChoicesState("0", "1")}})
	err := Wait(s.ctx, stepTimeout, "the incomplete answer to be refused by an ephemeral", func(ctx context.Context) (bool, error) {
		es, err := s.slack.Ephemerals(ctx, ChannelS3)
		for _, e := range es {
			if e.Recipient == user && e.ThreadTS == m.TS && strings.Contains(e.Text, "answer question 2") {
				return true, err
			}
		}
		return false, err
	})
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.slack.Thread(s.ctx, ChannelS3, m.TS)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := promptOf(same); !ok || got.TS != prompt.TS || got.Text != prompt.Text || !bytes.Equal(got.Blocks, prompt.Blocks) || got.Edited != nil {
		t.Errorf("after an incomplete answer the prompt is %+v; want it as it was, never edited", got)
	}
	err = Consistently(s.ctx, 2*time.Second, "the agent is told nothing of an incomplete answer", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelS3, m.TS)
		for _, b := range BotMessages(th) {
			if strings.Contains(b.Text, "Answers:") {
				return false, err
			}
		}
		return true, err
	})
	if err != nil {
		t.Error(err)
	}

	// Complete.
	press(fakeslack.StateValues{"q0": {"a0": ChoicesState("0", "1")}, "q1": {"a1": TextState("blue")}})
	th = s.thread(ChannelS3, m.TS, "the prompt to be replaced and the agent to go on", func(th []fakeslack.MessageView) bool {
		var replaced, continued bool
		for _, b := range BotMessages(th) {
			if b.TS == prompt.TS {
				replaced = !HasAction(b.Blocks, submitAction) && b.Edited != nil
			}
			continued = continued || (finished(b) && strings.Contains(b.Text, "Answers:"))
		}
		return replaced && continued
	})
	for _, b := range BotMessages(th) {
		if !strings.Contains(b.Text, "Answers:") {
			continue
		}
		if !strings.Contains(b.Text, "PostgreSQL, MySQL") || !strings.Contains(b.Text, "blue") || strings.Contains(b.Text, "(none)") {
			t.Errorf("the agent says %q; want it to have been given both answers, as they were typed", b.Text)
		}
	}
}
