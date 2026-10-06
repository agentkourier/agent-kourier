package interact_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

func choicesOf(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("choice %d", i)
	}
	return out
}

// slackPlatform returns the Slack adapter over a fake Slack with a thread in C1, and the thread's root.
func slackPlatform(t *testing.T) (*fakeslack.Server, *slack.Platform, string) {
	t.Helper()
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	_, root, err := slackgo.New("xoxb-fake", slackgo.OptionAPIURL(fake.URL()+"/")).PostMessage("C1", slackgo.MsgOptionText("root", false))
	if err != nil {
		t.Fatal(err)
	}
	client := slack.New(slack.Config{BotToken: secret.New("xoxb-fake"), AppToken: secret.New("xapp-fake"), APIURL: fake.URL() + "/"})
	return fake, slack.NewPlatform(client, "ns/main"), root
}

// TestAPromptAndItsEndingAreAcceptedBySlack posts a prompt with every kind of question through the adapter to a
// fake Slack, which refuses a block type it does not know, and then edits it into each ending the Replacer
// makes. A prompt made here, as the Intake makes it, is laid out by the adapter, and the ending that is built
// from the stored row replaces it with no control left.
func TestAPromptAndItsEndingAreAcceptedBySlack(t *testing.T) {
	fake, platform, root := slackPlatform(t)
	in := dialect.Interaction{Kind: dialect.AskUser, TaskID: "t1", RequestID: "r1", Hint: "<!channel> pick", Nested: &dialect.Nested{Subagent: "planner"}, Questions: []dialect.Question{
		{Text: "Why?"},
		{Text: "Which?", Choices: []string{"a", "b"}},
		{Text: "Some?", Choices: []string{"a", "b"}, Multiple: true},
		{Text: "Many?", Choices: choicesOf(30)},
		{Text: "Long?", Choices: []string{strings.Repeat("x", 200)}},
	}}
	req, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	prompt := chat.Prompt{ID: "ix-1", Hint: in.Hint, Deadline: time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC), Subagent: "planner"}
	for _, q := range in.Questions {
		prompt.Questions = append(prompt.Questions, chat.Question{Text: q.Text, Choices: q.Choices, Multiple: q.Multiple})
	}
	if err := platform.Fits(prompt); err != nil {
		t.Fatalf("Fits = %v", err)
	}
	ref, err := platform.Post(t.Context(), chat.Target{Channel: "C1", Thread: root}, chat.Ask(prompt), "ix-1/reply")
	if err != nil {
		t.Fatalf("the fake refused the prompt: %v", err)
	}
	answer, _ := json.Marshal(dialect.Answer{RequestID: "r1", Answers: [][]string{{"my password is hunter2"}, {"a"}, {"a", "b"}, {"choice 3"}, {}}})
	for state, want := range map[store.InteractionState]string{store.InteractionAnswered: "Answered", store.InteractionTimedOut: "Timed out", store.InteractionCancelled: "Cancelled"} {
		row := store.Interaction{ID: "ix-1", Key: key, State: state, Request: req, Answer: answer, Decider: "UALICE1", DecidedAt: t0, MessageRef: ref.ID}
		out, err := interact.ReplacementOutgoing(row)
		if err != nil {
			t.Fatalf("%s: ReplacementOutgoing = %v", state, err)
		}
		if err := platform.Update(t.Context(), ref, out); err != nil {
			t.Fatalf("%s: the fake refused the ending: %v", state, err)
		}
		var shown fakeslack.Message
		for _, m := range fake.Thread("C1", root) {
			if m.TS == ref.ID {
				shown = m
			}
		}
		blocks := string(shown.Blocks)
		if shown.Text != want || strings.Contains(blocks, `"input"`) || strings.Contains(blocks, `"button"`) || strings.Contains(blocks, "hunter2") || strings.Contains(blocks, "<!channel>") {
			t.Errorf("%s: edited prompt = %q %s; want %q, with no input or button, no free text, and nothing live", state, shown.Text, blocks, want)
		}
	}
}

// TestAnEndingIsNotMadeOfAPendingInteraction: only a decided interaction has a replacement, and one whose
// request cannot be read has none to build.
func TestAnEndingIsNotMadeOfAPendingInteraction(t *testing.T) {
	if _, err := interact.ReplacementOutgoing(store.Interaction{State: store.InteractionPending, Request: []byte(`{}`)}); err == nil {
		t.Error("a pending interaction has a replacement")
	}
	if _, err := interact.ReplacementOutgoing(store.Interaction{State: store.InteractionTimedOut, Request: []byte(`not json`)}); err == nil {
		t.Error("an unreadable request has a replacement")
	}
	if _, err := interact.ReplacementOutgoing(store.Interaction{State: store.InteractionAnswered, Request: []byte(`{}`), Answer: []byte(`not json`)}); err == nil {
		t.Error("an unreadable answer has a replacement")
	}
}

// TestTheIntakeAsksTheRealPlatformWhetherAPromptFits: a pause of 49 questions, which the Slack adapter cannot
// lay out in one message, is refused as unusable (the task cancelled, the thread told) with no post, and one
// that fits is posted.
func TestTheIntakeAsksTheRealPlatformWhetherAPromptFits(t *testing.T) {
	r := newRig(t, nil)
	platform := slack.NewPlatform(nil, "ns/main") // Fits is pure: no client is needed
	intake, err := interact.New(intakeCfg(interact.Config{Store: r.st, Lookup: lookup{interact.Binding{Dialect: kagentv1.New(30 * time.Second), AskUser: true, Timeout: timeout}},
		Platform: platform, Outbox: r.outbox, Session: r.sess, Audit: r.audit, Now: func() time.Time { return t0 }}))
	if err != nil {
		t.Fatal(err)
	}
	questions := func(n int) []map[string]any {
		qs := make([]map[string]any, n)
		for i := range qs {
			qs[i] = map[string]any{"question": fmt.Sprintf("Question %d?", i)}
		}
		return qs
	}
	big := pausedOn(map[string]any{"type": "ask_user_request", "id": "req-big", "questions": questions(49)})
	if err := intake.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Task: big}); err != nil {
		t.Fatalf("Paused of 49 questions = %v; want it refused as unusable, and handled", err)
	}
	if len(r.sess.cancels) != 1 || len(r.slack.posted) != 0 {
		t.Errorf("%d cancels, %d posts; want the task cancelled and nothing posted", len(r.sess.cancels), len(r.slack.posted))
	}
	if err := platform.Fits(chat.Prompt{ID: "x", Questions: make([]chat.Question, 49)}); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("Fits of 49 questions = %v; want chat.ErrTooLarge", err)
	}
	small := pausedOn(map[string]any{"type": "ask_user_request", "id": "req-small", "questions": questions(3)})
	small.ID = "task-2"
	if err := intake.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Task: small}); err != nil || len(r.slack.posted) != 1 {
		t.Errorf("Paused of 3 questions = %v with %d posts; want the prompt posted", err, len(r.slack.posted))
	}
}

// TestTheLoserOfARaceIsToldWhoWonAsAMention goes through the Slack adapter: the winner of an answer is named to
// the person who pressed after in a real mention if its ID has the shape of Slack's, and as "someone" if it has
// not, so that an ID that is no ID can never be markup.
func TestTheLoserOfARaceIsToldWhoWonAsAMention(t *testing.T) {
	for _, tc := range []struct{ winner, want string }{
		{"UALICE1", "<@UALICE1>"},
		{"<!channel>", "someone"},
		{"UALICE1><!here>", "someone"},
	} {
		fake, platform, root := slackPlatform(t)
		r := newDecideRig(t)
		d, err := interact.NewDecider(decideCfg(interact.DecideConfig{Store: r.st, Auditor: r.aud, Ephemerals: platform, Session: r.sess, Sessions: r.st, Replacer: r.rep, Outbox: r.out}))
		if err != nil {
			t.Fatal(err)
		}
		r.pending(t)
		if err := r.st.SubmitInteraction(t.Context(), "ix-1", []byte(`{"RequestID":"req-1"}`), tc.winner, d0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		press := r.press(t, "UBOB22", d0.Add(2*time.Minute), "req-1")
		press.Interaction.Key.Thread = root
		if err := d.Decide(t.Context(), press); err != nil {
			t.Fatalf("Decide = %v", err)
		}
		es := fake.Ephemerals("C1")
		if len(es) != 1 || es[0].Recipient != "UBOB22" {
			t.Fatalf("winner %q: ephemerals %+v; want one, to UBOB22", tc.winner, es)
		}
		var blocks []struct {
			Elements []struct{ Type, Text string }
		}
		if err := json.Unmarshal(es[0].Blocks, &blocks); err != nil || len(blocks) != 1 || len(blocks[0].Elements) != 2 {
			t.Fatalf("winner %q: blocks %s, %v; want one context line of two elements", tc.winner, es[0].Blocks, err)
		}
		if got := blocks[0].Elements[0].Text; got != tc.want || blocks[0].Elements[1].Text != "answered this question first." {
			t.Errorf("winner %q: the line says %+v; want %q and the words", tc.winner, blocks[0].Elements, tc.want)
		}
	}
}
