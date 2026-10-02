package chattest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

var (
	ctx = context.Background()
	tgt = chat.Target{Conn: "courier-system/slack-main", Channel: "C1", Thread: "1.1"}
)

func say(text string) chat.Outgoing { return chat.Say(chat.Message{Fallback: text}) }

func TestPostThenFindThenUpdate(t *testing.T) {
	f := NewFake()
	ref, err := f.Post(ctx, tgt, say("hello"), "outbox-1")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Platform != "fake" || ref.Channel != "C1" || ref.Thread != "1.1" || ref.ID == "" {
		t.Errorf("Ref = %+v, want one in the target's channel and thread", ref)
	}
	found, ok, err := f.Find(ctx, tgt, "outbox-1", time.Time{})
	if err != nil || !ok || found != ref {
		t.Errorf("Find = %+v, %t, %v; want the posted message", found, ok, err)
	}
	if _, ok, _ := f.Find(ctx, tgt, "outbox-2", time.Time{}); ok {
		t.Error("Find found a message that was never posted")
	}
	if _, ok, _ := f.Find(ctx, tgt, "outbox-1", time.Now().Add(time.Hour)); ok {
		t.Error("Find found a message posted before `since`")
	}

	if err := f.Update(ctx, ref, say("edited")); err != nil {
		t.Fatal(err)
	}
	if m := f.Messages()[0]; m.Out.Message.Fallback != "edited" || m.Updates != 1 {
		t.Errorf("message = %+v, want the edit applied once", m)
	}
	err = f.Update(ctx, chat.Ref{Platform: "fake", Channel: "C1", ID: "9.000"}, say("x"))
	if v := f.Classify(chat.OpPost, err); v.Class != chat.Permanent || v.Reason != chat.ReasonChannelGone {
		t.Errorf("an update of a message that is not there classifies as %+v, want permanent and channel_gone", v)
	}
}

func TestAMalformedOrTooLargeOutgoingIsRefused(t *testing.T) {
	f := NewFake()
	if _, err := f.Post(ctx, tgt, chat.Outgoing{}, "t"); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("Post of a zero Outgoing = %v, want ErrInvalid", err)
	}
	big := chat.Prompt{ID: "ix-1", Questions: make([]chat.Question, f.MaxQuestions+1)}
	if err := f.Fits(big); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("Fits = %v, want ErrTooLarge", err)
	}
	if _, err := f.Post(ctx, tgt, chat.Ask(big), "t"); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("Post of a prompt that is too large = %v, want ErrTooLarge", err)
	}
	if len(f.Messages()) != 0 {
		t.Error("a refused post was kept")
	}
}

func TestStreamGrowsThenStopsAsAMessageWithItsTag(t *testing.T) {
	f := NewFake()
	st, err := f.Stream(ctx, tgt, "U1", "Hel")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(ctx, "lo"); err != nil {
		t.Fatal(err)
	}
	if m := f.Messages()[0]; m.Streamed != "Hello" || !m.Streaming || m.Recipient != "U1" {
		t.Errorf("message = %+v, want a stream holding Hello, shown to U1", m)
	}
	if err := st.Stop(ctx, chat.Message{Fallback: "Hello", Parts: []chat.Part{chat.Body{Markdown: "Hello"}}}, "outbox-9"); err != nil {
		t.Fatal(err)
	}
	m := f.Messages()[0]
	if m.Streaming || m.Tag != "outbox-9" || m.Out.Message == nil {
		t.Errorf("message = %+v, want it stopped, tagged, with its final content", m)
	}
	if ref, ok, _ := f.Find(ctx, tgt, "outbox-9", time.Time{}); !ok || ref != st.Ref() {
		t.Error("Find does not know the stopped stream by its tag")
	}
	if err := st.Append(ctx, "more"); err == nil {
		t.Error("an append to a stopped stream succeeded")
	}
}

func TestAPlatformWithNoStreamsSaysSo(t *testing.T) {
	f := NewFake()
	f.SetCaps(chat.Caps{MaxText: 4000})
	if _, err := f.Stream(ctx, tgt, "U1", "x"); !errors.Is(err, chat.ErrNoStreams) {
		t.Errorf("Stream = %v, want ErrNoStreams", err)
	}
}

func TestReactionsAreIdempotentAndNeedAMessage(t *testing.T) {
	f := NewFake()
	ref, _ := f.Post(ctx, tgt, say("x"), "t")
	for range 2 {
		if err := f.React(ctx, ref, "eyes"); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.Messages()[0].Reactions; len(got) != 1 || got[0] != "eyes" {
		t.Errorf("reactions = %v, want eyes once", got)
	}
	if err := f.React(ctx, chat.Ref{ID: "nope"}, "eyes"); f.Classify(chat.OpReact, err).Class != chat.Permanent {
		t.Errorf("a reaction on a message that is not there = %v, want permanent", err)
	}
}

func TestMembershipAndBotChecksFailClosed(t *testing.T) {
	f := NewFake()
	f.AddUser(chat.User{ID: "U1", Name: "ada"}, false)
	f.AddUser(chat.User{ID: "B1"}, true)
	f.SetMembers("C1", "U1")
	if ok, err := f.IsMember(ctx, "C1", "U1"); err != nil || !ok {
		t.Errorf("IsMember(U1) = %t, %v", ok, err)
	}
	if ok, _ := f.IsMember(ctx, "C1", "U2"); ok {
		t.Error("a stranger is a member")
	}
	if bot, err := f.IsBot(ctx, "U1"); err != nil || bot {
		t.Errorf("IsBot(U1) = %t, %v", bot, err)
	}
	if bot, err := f.IsBot(ctx, "B1"); err != nil || !bot {
		t.Errorf("IsBot(B1) = %t, %v", bot, err)
	}
	if bot, err := f.IsBot(ctx, "nobody"); err == nil || !bot {
		t.Errorf("IsBot(nobody) = %t, %v, want a bot, with an error: fail closed", bot, err)
	}
	if u, err := f.ResolveUser(ctx, "U1"); err != nil || u.Name != "ada" {
		t.Errorf("ResolveUser = %+v, %v", u, err)
	}
	if _, err := f.ResolveUser(ctx, "nobody"); err == nil {
		t.Error("an unknown user was resolved")
	}
}

func TestInjectedFailuresComeInTurnAndClassify(t *testing.T) {
	f := NewFake()
	gone := &FakeError{Verdict: chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "channel_not_found"}, Msg: "gone"}
	revoked := &FakeError{Verdict: chat.Verdict{Class: chat.FatalAuth, Code: "token_revoked"}, Msg: "revoked"}
	f.FailNext("Post", gone, revoked)

	_, e1 := f.Post(ctx, tgt, say("a"), "t1")
	_, e2 := f.Post(ctx, tgt, say("b"), "t2")
	_, e3 := f.Post(ctx, tgt, say("c"), "t3")

	if !errors.Is(e1, gone) || !errors.Is(e2, revoked) || e3 != nil {
		t.Errorf("errors = %v, %v, %v; want the two injected, in order, then success", e1, e2, e3)
	}
	if v := f.Classify(chat.OpPost, fmt.Errorf("post: %w", e2)); v.Class != chat.FatalAuth || v.Code != "token_revoked" {
		t.Errorf("Classify(wrapped) = %+v, want FatalAuth with the code", v)
	}
	if v := f.Classify(chat.OpPost, errors.New("boom")); v.Class != chat.Transient {
		t.Errorf("Classify(unknown) = %+v, want Transient", v)
	}
}

type sinkRec struct {
	msgs    []chat.Inbound
	presses []chat.Interaction
}

func (s *sinkRec) Message(_ context.Context, m chat.Inbound) error {
	s.msgs = append(s.msgs, m)
	return nil
}
func (s *sinkRec) Interaction(_ context.Context, i chat.Interaction) error {
	s.presses = append(s.presses, i)
	return nil
}

func TestRunDeliversWhatIsSaidAndPressedUntilItsContextEnds(t *testing.T) {
	f := NewFake()
	sink := &sinkRec{}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.Run(rctx, sink) }()

	in := chat.Inbound{At: chat.Ref{Platform: "fake", Channel: "C1", ID: "1.1"}, User: "U1", Text: "hi", Mentioned: true}
	if err := f.Say(rctx, in); err != nil {
		t.Fatal(err)
	}
	press := chat.Interaction{User: "U1", PromptID: "ix-1", Answers: []chat.Answer{{Question: 0, Values: []string{"prod"}}}}
	if err := f.Press(rctx, press); err != nil {
		t.Fatal(err)
	}
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want the context's error", err)
	}
	if len(sink.msgs) != 1 || sink.msgs[0].Text != "hi" || len(sink.presses) != 1 || sink.presses[0].PromptID != "ix-1" {
		t.Errorf("the sink got %+v and %+v", sink.msgs, sink.presses)
	}
}

func TestRunCanFailWithTheFatalErrorOfARejectedCredential(t *testing.T) {
	f := NewFake()
	revoked := &FakeError{Verdict: chat.Verdict{Class: chat.FatalAuth, Code: "invalid_auth"}, Msg: "rejected"}
	f.FailRun(revoked)
	err := f.Run(ctx, &sinkRec{})
	if f.Classify(chat.OpOther, err).Class != chat.FatalAuth {
		t.Errorf("Run = %v, want an error that classifies as FatalAuth", err)
	}
}

func TestEphemeralsAreKeptAndNeedAMessage(t *testing.T) {
	f := NewFake()
	if err := f.Ephemeral(ctx, tgt, "U1", chat.Message{Fallback: "only you"}); err != nil {
		t.Fatal(err)
	}
	if got := f.Ephemerals(); len(got) != 1 || got[0].User != "U1" || got[0].Msg.Fallback != "only you" {
		t.Errorf("ephemerals = %+v", got)
	}
	if err := f.Ephemeral(ctx, tgt, "U1", chat.Message{}); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("an empty ephemeral = %v, want ErrInvalid", err)
	}
}

func TestFieldsListsEveryPieceOfTextWithItsPlace(t *testing.T) {
	m := chat.Message{Fallback: "f", Parts: []chat.Part{
		chat.Header{Text: "h"}, chat.Body{Markdown: "b"}, chat.Context{Text: "c"}, chat.Note{Text: "n"},
		chat.ToolLine{Name: "t", Args: map[string]string{"k": "v"}},
		chat.Buttons{Buttons: []chat.Button{{ID: "i", Label: "l", Value: "v"}}},
	}}
	got := map[chat.Where][]string{}
	for _, f := range Fields(chat.Say(m)) {
		got[f.Where] = append(got[f.Where], f.Text)
	}
	for w, want := range map[chat.Where]int{chat.WhereFallback: 1, chat.WhereHeader: 1, chat.WhereBody: 1, chat.WhereContext: 1, chat.WhereNote: 1, chat.WhereToolName: 1, chat.WhereToolArg: 2, chat.WhereButton: 1} {
		if len(got[w]) != want {
			t.Errorf("%s: %d fields %v, want %d", w, len(got[w]), got[w], want)
		}
	}
	e := Fields(chat.End(chat.Ending{Outcome: chat.Answered, By: "U1", Prompt: chat.Prompt{ID: "ix", Hint: "h", Subagent: "s", Questions: []chat.Question{{Text: "q", Choices: []string{"a", "b"}}}}, Answers: [][]string{{"x"}}}))
	places := map[chat.Where]int{}
	for _, f := range e {
		places[f.Where]++
	}
	for w, want := range map[chat.Where]int{chat.WhereHint: 1, chat.WhereSubagent: 1, chat.WhereQuestion: 1, chat.WhereChoice: 2, chat.WhereUser: 1, chat.WhereAnswer: 1} {
		if places[w] != want {
			t.Errorf("an ending: %s has %d fields, want %d", w, places[w], want)
		}
	}
}
