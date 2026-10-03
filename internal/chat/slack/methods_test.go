package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

func TestUpdateReplacesTheMessageAndItsBlocks(t *testing.T) {
	w := newWorld(t)
	ref, err := w.p.Post(t.Context(), w.target, chat.Say(chat.Message{Fallback: "first", Parts: []chat.Part{chat.Header{Text: "one"}}}), "t1")
	if err != nil {
		t.Fatal(err)
	}

	if err := w.p.Update(t.Context(), ref, chat.Say(chat.Message{Fallback: "second", Parts: []chat.Part{chat.Header{Text: "two"}}})); err != nil {
		t.Fatal(err)
	}

	m, blocks := w.last(t)
	if m.Text != "second" || textOf(blocks[0]) != "two" || m.Edited == nil {
		t.Errorf("message = %q %q edited %v; want the second content, marked as edited", m.Text, textOf(blocks[0]), m.Edited)
	}
	if err := w.p.Update(t.Context(), ref, chat.Outgoing{}); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("Update with a malformed message = %v, want ErrInvalid", err)
	}
}

func TestAStreamGrowsAndStopsWithItsBlocksAndTag(t *testing.T) {
	w := newWorld(t)
	st, err := w.p.Stream(t.Context(), w.target, "U1", "Hel")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(t.Context(), "lo <!here> [x](y)"); err != nil {
		t.Fatal(err)
	}
	m, _ := w.last(t)
	if m.Text != `Hello &lt;!here&gt; \[x\](y)` {
		t.Errorf("streamed text = %q, want what was started and appended, escaped as a markdown block's text is", m.Text)
	}
	if !m.Streaming || st.Ref().ID != m.TS || st.Ref().Thread != w.target.Thread {
		t.Fatalf("message = %+v, ref %+v; want a streaming message, and its Ref", m, st.Ref())
	}
	if err := st.Stop(t.Context(), "", chat.Message{Fallback: "Hello", Parts: []chat.Part{chat.Body{Markdown: "Hello"}}}, "outbox-9"); err != nil {
		t.Fatal(err)
	}
	m, blocks := w.last(t)
	if m.Streaming || m.Metadata == nil || m.Metadata.EventPayload["outbox_id"] != "outbox-9" || blocks[0]["text"] != "Hello" {
		t.Errorf("message = %+v blocks %v; want it stopped, tagged, with the final markdown block", m, blocks)
	}
	if err := st.Stop(t.Context(), "", chat.Message{}, "outbox-9"); !errors.Is(err, chat.ErrNotStreaming) {
		t.Errorf("a second Stop = %v, want ErrNotStreaming", err)
	}
	if err := st.Append(t.Context(), "more"); !errors.Is(err, chat.ErrNotStreaming) {
		t.Errorf("an Append after the stop = %v, want ErrNotStreaming", err)
	}
}

// Stop takes the last of the text with it, as one call; a zero final sends no blocks and leaves the message as it
// was streamed; and a stream that another run started is stopped by its Ref, and a message that was never a stream
// says so.
func TestAStreamStopsWithItsLastTextAndAStreamOfAnotherRunByItsRef(t *testing.T) {
	w := newWorld(t)
	st, err := w.p.Stream(t.Context(), w.target, "U1", "Hel")
	if err != nil {
		t.Fatal(err)
	}
	other := w.p.StreamAt(st.Ref())

	if err := other.Stop(t.Context(), "lo", chat.Message{}, "outbox-1"); err != nil {
		t.Fatal(err)
	}

	m, blocks := w.last(t)
	if m.Streaming || m.Text != "Hello" || len(blocks) != 0 || m.Metadata == nil || m.Metadata.EventPayload["outbox_id"] != "outbox-1" {
		t.Errorf("message = %+v blocks %v; want it stopped with the last text, no blocks, and the tag", m, blocks)
	}
	posted, err := w.p.Post(t.Context(), w.target, chat.Say(chat.Message{Fallback: "x"}), "outbox-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.p.StreamAt(posted).Stop(t.Context(), "", chat.Message{}, "outbox-2"); !errors.Is(err, chat.ErrNotStreaming) {
		t.Errorf("Stop of a message that was never a stream = %v, want ErrNotStreaming", err)
	}
}

func TestEphemeralIsShownToOnePersonInTheThread(t *testing.T) {
	w := newWorld(t)
	w.fake.SetMembers("C1", "U1")

	if err := w.p.Ephemeral(t.Context(), w.target, "U1", chat.Message{Fallback: "only you", Parts: []chat.Part{chat.Note{Text: "<@U2> *x*"}}}); err != nil {
		t.Fatal(err)
	}

	eph := w.fake.Ephemerals("C1")
	var blocks []map[string]any
	if len(eph) == 1 {
		_ = json.Unmarshal(eph[0].Blocks, &blocks)
	}
	if len(eph) != 1 || eph[0].Recipient != "U1" || eph[0].Text != "only you" || len(blocks) != 1 || textOf(blocks[0]) != "&lt;@U2&gt; *x*" {
		t.Errorf("ephemerals = %+v, want one for U1 with the text, escaped", eph)
	}
	if err := w.p.Ephemeral(t.Context(), w.target, "U1", chat.Message{}); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("an empty ephemeral = %v, want ErrInvalid", err)
	}
}

func TestReactAndTheChecksOfWhoIsInTheChannel(t *testing.T) {
	w := newWorld(t)
	ref, err := w.p.Post(t.Context(), w.target, chat.Say(chat.Message{Fallback: "x"}), "t")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // again is success
		if err := w.p.React(t.Context(), ref, "eyes"); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.fake.Reactions("C1", ref.ID); len(got) != 1 || got[0].Name != "eyes" {
		t.Errorf("reactions = %+v, want eyes once", got)
	}
	err = w.p.React(t.Context(), chat.Ref{Channel: "C1", ID: "9.9"}, "eyes")
	if v := w.p.Classify(chat.OpReact, err); v.Class != chat.Permanent || v.Reason != chat.ReasonChannelGone {
		t.Errorf("a reaction on a message that is not there classifies as %+v (%v), want permanent and channel_gone", v, err)
	}
	for range 2 { // again is success
		if err := w.p.Unreact(t.Context(), ref, "eyes"); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.fake.Reactions("C1", ref.ID); len(got) != 0 {
		t.Errorf("reactions after an unreaction = %+v, want none", got)
	}
	err = w.p.Unreact(t.Context(), chat.Ref{Channel: "C1", ID: "9.9"}, "eyes")
	if v := w.p.Classify(chat.OpReact, err); v.Class != chat.Permanent || v.Reason != chat.ReasonChannelGone {
		t.Errorf("an unreaction on a message that is not there classifies as %+v (%v), want permanent and channel_gone", v, err)
	}

	w.fake.AddUser(fakeslack.User{ID: "U1", Name: "ada", Email: "ada@example.test"})
	w.fake.AddUser(fakeslack.User{ID: "B1", Name: "robot", IsBot: true})
	w.fake.SetMembers("C1", "U1")
	if ok, err := w.p.IsMember(t.Context(), "C1", "U1"); err != nil || !ok {
		t.Errorf("IsMember(U1) = %t, %v", ok, err)
	}
	if ok, err := w.p.IsMember(t.Context(), "C1", "U2"); err != nil || ok {
		t.Errorf("IsMember(U2) = %t, %v; want not a member", ok, err)
	}
	if bot, err := w.p.IsBot(t.Context(), "B1"); err != nil || !bot {
		t.Errorf("IsBot(B1) = %t, %v", bot, err)
	}
	if bot, err := w.p.IsBot(t.Context(), "nobody"); err == nil || !bot {
		t.Errorf("IsBot(nobody) = %t, %v; want a bot, with an error: fail closed", bot, err)
	}
	u, err := w.p.ResolveUser(t.Context(), "U1")
	if err != nil || u != (chat.User{ID: "U1", Name: "ada", Email: "ada@example.test"}) {
		t.Errorf("ResolveUser = %+v, %v", u, err)
	}
}

func TestACapabilityIsWhatSlackCanDo(t *testing.T) {
	c := newWorld(t).p.Capabilities()
	if !c.Streams || !c.Ephemeral || !c.Edits || !c.Inputs {
		t.Errorf("Capabilities = %+v, want streams, ephemerals, edits and inline inputs", c)
	}
	for _, w := range chat.AllWheres() {
		if w == chat.WhereUser {
			continue // a user ID is not shown as text
		}
		if c.Limit(w) <= len(chat.Truncated) {
			t.Errorf("the limit of %s is %d, which cannot hold the truncation marker", w, c.Limit(w))
		}
	}
}

// recorder is a chat.Sink that keeps what it is given.
type recorder struct {
	mu      sync.Mutex
	msgs    []chat.Inbound
	presses []chat.Interaction
	err     error
}

func (r *recorder) Message(_ context.Context, m chat.Inbound) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, m)
	return r.err
}

func (r *recorder) Interaction(_ context.Context, i chat.Interaction) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.presses = append(r.presses, i)
	return r.err
}

func (r *recorder) wait(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		r.mu.Lock()
		ok := done()
		r.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// Run hands the neutral sink what people write, as the model has it: where the message is and its thread, who wrote
// it, whether it mentions courier and whether it is a direct message; and what they press, with the answers read from
// the controls' state.
func TestRunGivesTheNeutralSinkMessagesAndPresses(t *testing.T) {
	w := newWorld(t)
	rec := &recorder{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.p.Run(ctx, rec) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(5 * time.Second); w.fake.Connections() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
	}

	mention, err := w.fake.Mention("C1", "U1", botMark+" why?")
	if err != nil {
		t.Fatal(err)
	}
	reply, err := w.fake.Reply("C1", w.target.Thread, "U2", "more")
	if err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "the mention and the reply", func() bool {
		seen := map[string]bool{}
		for _, m := range rec.msgs {
			seen[m.At.ID] = true
		}
		return seen[mention.TS] && seen[reply.TS]
	})

	byTS := map[string]chat.Inbound{}
	for _, m := range rec.msgs {
		byTS[m.At.ID] = m
	}
	if m := byTS[mention.TS]; !m.Mentioned || m.Direct || m.User != "U1" || m.At.Channel != "C1" || m.At.Platform != "slack" || m.At.Conn != "courier-system/slack-main" || !strings.Contains(m.Text, "why?") {
		t.Errorf("the mention = %+v", m)
	}
	if m := byTS[reply.TS]; m.Mentioned || m.At.Thread != w.target.Thread || m.User != "U2" || m.Text != "more" {
		t.Errorf("the reply = %+v; want it in the thread, not a mention", m)
	}

	ask := w.post(t, chat.Ask(askOf(chat.Question{Text: "free?"}, chat.Question{Text: "pick", Choices: choices(3)})))
	m, _ := w.last(t)
	_ = ask
	values := fakeslack.StateValues{"q0": {"t0": {"type": "plain_text_input", "value": "prod"}}, "q1": {"r1": {"type": "radio_buttons", "selected_option": map[string]any{"value": "2"}}}}
	if _, err := w.fake.Submit("U1", m.TS, "submit", values); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "the press", func() bool { return len(rec.presses) >= 1 })
	p := rec.presses[0]
	if p.User != "U1" || p.PromptID != "ix-0123456789abcdef" || p.At.ID != m.TS || p.At.Channel != "C1" || len(p.Answers) != 2 ||
		p.Answers[0].Question != 0 || p.Answers[0].Values[0] != "prod" || p.Answers[1].Question != 1 || p.Answers[1].Values[0] != "2" {
		t.Errorf("press = %+v, want who, the prompt's ID, the message, and an answer for each question", p)
	}
}

// What the sink returns is what Slack is told: an error leaves the event unacknowledged, so Slack sends it again.
func TestASinkThatCannotTakeAMessageLeavesItUnacknowledged(t *testing.T) {
	w := newWorld(t)
	rec := &recorder{err: errors.New("not now")}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.p.Run(ctx, rec) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(5 * time.Second); w.fake.Connections() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
	}

	if _, err := w.fake.Mention("C1", "U1", botMark+" hi"); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "the message to be tried again", func() bool { return len(rec.msgs) >= 2 })
}
