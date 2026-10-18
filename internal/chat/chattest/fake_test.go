package chattest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

var (
	ctx = context.Background()
	tgt = chat.Target{Conn: "agent-kourier-system/slack-main", Channel: "C1", Thread: "1.1"}
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
	if err := st.Stop(ctx, "", chat.Message{Fallback: "Hello", Parts: []chat.Part{chat.Body{Markdown: "Hello"}}}, "outbox-9"); err != nil {
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

func TestAStepIsBegunOnceAndThenMovedForward(t *testing.T) {
	f := NewFake()
	st, _ := f.Stream(ctx, tgt, "U1", "x")
	for _, s := range []chat.Step{
		{ID: "s1", Title: "search", Detail: "q=1"},
		{ID: "s2", Title: "fetch"},
		{ID: "s1", Title: "search", Detail: "q=1", State: chat.StepDone},
	} {
		if err := st.Step(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	want := []chat.Step{{ID: "s1", Title: "search", Detail: "q=1", State: chat.StepDone}, {ID: "s2", Title: "fetch"}}
	if got := f.Messages()[0].Steps; !slices.Equal(got, want) {
		t.Errorf("steps = %+v, want %+v: in the order begun, each in its last state", got, want)
	}
}

func TestAPlatformWithNoStepsSaysSoAndShowsNone(t *testing.T) {
	f := NewFake()
	f.SetCaps(chat.Caps{Streams: true, MaxText: 4000})
	st, _ := f.Stream(ctx, tgt, "U1", "x")
	if err := st.Step(ctx, chat.Step{ID: "s1", Title: "search"}); !errors.Is(err, chat.ErrNoSteps) {
		t.Errorf("Step = %v, want ErrNoSteps", err)
	}
	if got := f.Messages()[0].Steps; len(got) != 0 {
		t.Errorf("steps = %+v, want none", got)
	}
}

func TestAStepOfAStoppedStreamIsRefused(t *testing.T) {
	f := NewFake()
	st, _ := f.Stream(ctx, tgt, "U1", "x")
	if err := st.Stop(ctx, "", chat.Message{}, "t"); err != nil {
		t.Fatal(err)
	}
	if err := st.Step(ctx, chat.Step{ID: "s1", Title: "search"}); !errors.Is(err, chat.ErrNotStreaming) {
		t.Errorf("Step = %v, want ErrNotStreaming", err)
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

func TestUnreactTakesAReactionOffAndIsSuccessWhenItIsNotThere(t *testing.T) {
	f := NewFake()
	ref, _ := f.Post(ctx, tgt, say("x"), "t")
	for _, name := range []string{"eyes", "hourglass"} {
		if err := f.React(ctx, ref, name); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := f.Unreact(ctx, ref, "eyes"); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.Messages()[0].Reactions; len(got) != 1 || got[0] != "hourglass" {
		t.Errorf("reactions = %v, want only hourglass", got)
	}
	if err := f.Unreact(ctx, chat.Ref{ID: "nope"}, "eyes"); f.Classify(chat.OpReact, err).Class != chat.Permanent {
		t.Errorf("an unreaction on a message that is not there = %v, want permanent", err)
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

func TestGroupMembershipFailsClosed(t *testing.T) {
	f := NewFake()
	f.SetGroup("G1", "U1", "U2")
	f.SetGroup("G2", "U3")
	for _, tc := range []struct {
		name   string
		groups []string
		user   string
		want   bool
		err    bool
	}{
		{"in the first group", []string{"G1", "G2"}, "U1", true, false},
		{"in the second group", []string{"G1", "G2"}, "U3", true, false},
		{"in neither", []string{"G1", "G2"}, "U9", false, false},
		{"no groups", nil, "U1", false, false},
		{"a group the fake was not told of", []string{"G404"}, "U1", false, true},
		{"an unknown group before a hit is still an error", []string{"G404", "G1"}, "U1", false, true},
	} {
		got, err := f.IsInAnyGroup(ctx, tc.groups, tc.user)
		if got != tc.want || (err != nil) != tc.err {
			t.Errorf("%s: IsInAnyGroup(%v, %s) = %t, %v; want %t, error %t", tc.name, tc.groups, tc.user, got, err, tc.want, tc.err)
		}
		// A group that does not exist is the permanent kind: the contract of chat.Platform.IsInAnyGroup.
		if tc.err && !errors.Is(err, chat.ErrGroupsMisconfigured) {
			t.Errorf("%s: the error %v does not wrap chat.ErrGroupsMisconfigured", tc.name, err)
		}
	}
	// The two kinds of failure a caller must tell apart: one injected as a plain error may pass on a retry, and one that
	// wraps the sentinel never will. Both are false.
	f.FailNext("IsInAnyGroup", errors.New("boom"), fmt.Errorf("scope: %w", chat.ErrGroupsMisconfigured))
	if got, err := f.IsInAnyGroup(ctx, []string{"G1"}, "U1"); got || err == nil || errors.Is(err, chat.ErrGroupsMisconfigured) {
		t.Errorf("IsInAnyGroup with an injected plain failure = %t, %v; want false and an error that is not the permanent kind", got, err)
	}
	if got, err := f.IsInAnyGroup(ctx, []string{"G1"}, "U1"); got || !errors.Is(err, chat.ErrGroupsMisconfigured) {
		t.Errorf("IsInAnyGroup with an injected permanent failure = %t, %v; want false and chat.ErrGroupsMisconfigured", got, err)
	}
	if got, err := f.IsInAnyGroup(ctx, []string{"G1"}, "U1"); err != nil || !got {
		t.Errorf("IsInAnyGroup after the failure = %t, %v; want true", got, err)
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
		chat.ToolLine{Name: "t", Summary: "k=v"},
		chat.Buttons{Buttons: []chat.Button{{ID: "i", Label: "l", Value: "v"}}},
	}}
	got := map[chat.Where][]string{}
	for _, f := range Fields(chat.Say(m)) {
		got[f.Where] = append(got[f.Where], f.Text)
	}
	for w, want := range map[chat.Where]int{chat.WhereFallback: 1, chat.WhereHeader: 1, chat.WhereBody: 1, chat.WhereContext: 1, chat.WhereNote: 1, chat.WhereToolName: 1, chat.WhereToolArg: 1, chat.WhereButton: 1} {
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

func TestThreadBetweenReadsWhatWasPostedAndSaidInTheThread(t *testing.T) {
	f := NewFake()
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = f.Run(run, &sinkRec{}) }()
	at := func(id string) chat.Ref {
		return chat.Ref{Platform: "fake", Conn: tgt.Conn, Channel: "C1", Thread: "1.1", ID: id}
	}
	own, err := f.Post(ctx, tgt, say("an answer"), "t1") // the fake numbers its posts "n.000": this is 1.000, in the thread of tgt
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []chat.Inbound{
		{At: at("3.000001"), User: "U1", Text: "third"},
		{At: at("2.000001"), User: "U1", Text: "second"},
		{At: at("10.000001"), User: "U2", Text: "tenth"},
		{At: chat.Ref{Platform: "fake", Channel: "C1", Thread: "9.9", ID: "5.000"}, User: "U1", Text: "another thread"},
		{At: chat.Ref{Platform: "fake", Channel: "C2", Thread: "1.1", ID: "5.000"}, User: "U1", Text: "another channel"},
		{At: at("1.1"), User: "U1", Text: "the root"},
	} {
		if err := f.Say(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	texts := func(after, before string, limit int) (out []string, dropped int, err error) {
		msgs, dropped, err := f.ThreadBetween(ctx, tgt, after, before, limit)
		for _, m := range msgs {
			out = append(out, m.Message.Text)
		}
		return out, dropped, err
	}
	for name, tc := range map[string]struct {
		after, before string
		limit         int
		want          []string
		dropped       int
	}{
		"all, oldest first by ts, not by length": {"", "", 10, []string{"an answer", "second", "third", "tenth"}, 0},
		"exclusive bounds":                       {own.ID, "10.000001", 10, []string{"second", "third"}, 0},
		"after only":                             {"2.000001", "", 10, []string{"third", "tenth"}, 0},
		"before only":                            {"", "2.000001", 10, []string{"an answer"}, 0},
		"the newest, with the rest counted":      {"", "", 2, []string{"third", "tenth"}, 2},
		"an empty window":                        {"3.000001", "3.000001", 10, nil, 0},
		"an inverted window":                     {"10.000001", "2.000001", 10, nil, 0},
		"a short fraction is read as six digits": {"2.1", "", 10, []string{"third", "tenth"}, 0},
	} {
		got, dropped, err := texts(tc.after, tc.before, tc.limit)
		if err != nil || dropped != tc.dropped || !slices.Equal(got, tc.want) {
			t.Errorf("%s: ThreadBetween(%q, %q, %d) = %v, %d, %v; want %v, %d", name, tc.after, tc.before, tc.limit, got, dropped, err, tc.want, tc.dropped)
		}
	}
	msgs, _, _ := f.ThreadBetween(ctx, tgt, "", "2.000001", 10)
	if len(msgs) != 1 || !msgs[0].Own || msgs[0].Ignored || msgs[0].Message.At != own {
		t.Errorf("a post of the fake's own = %+v; want Own, at %+v", msgs, own)
	}
	if msgs, _, _ = f.ThreadBetween(ctx, tgt, "2.000001", "3.000002", 10); len(msgs) != 1 || msgs[0].Own || msgs[0].Message.User != "U1" {
		t.Errorf("a person's message = %+v; want theirs, not Own", msgs)
	}

	f.FailNext("ThreadBetween", &FakeError{Verdict: chat.Verdict{Class: chat.Transient, RetryAfter: 3 * time.Second}, Msg: "limited"})
	if _, _, err := f.ThreadBetween(ctx, tgt, "", "", 10); err == nil || f.Classify(chat.OpOther, err).RetryAfter != 3*time.Second {
		t.Errorf("a queued failure = %v; want the error the test chose", err)
	}
	if _, _, err := f.ThreadBetween(ctx, chat.Target{Channel: "C1"}, "", "", 10); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("ThreadBetween with no thread = %v; want ErrInvalid", err)
	}
	if _, _, err := f.ThreadBetween(ctx, tgt, "", "", 0); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("ThreadBetween with no room = %v; want ErrInvalid", err)
	}
}

func TestFieldsListsTheNameAndArgumentsOfEachToolOfAnApproval(t *testing.T) {
	approval := chat.Prompt{ID: "ix", Hint: "h", Subagent: "s", Tools: []chat.Tool{{Name: "kubectl", Args: "get\npods"}, {Name: "ping"}}}
	for name, o := range map[string]chat.Outgoing{
		"prompt": chat.Ask(approval),
		"ending": chat.End(chat.Ending{Outcome: chat.Answered, By: "U1", Prompt: approval, Verdicts: []chat.ToolVerdict{chat.ToolApproved, chat.ToolDenied}}),
	} {
		var names, args []string
		for _, f := range Fields(o) {
			switch f.Where {
			case chat.WhereToolName:
				names = append(names, f.Text)
			case chat.WhereToolArgs:
				args = append(args, f.Text)
			}
		}
		if !slices.Equal(names, []string{"kubectl", "ping"}) || !slices.Equal(args, []string{"get\npods", ""}) {
			t.Errorf("%s: tool names %q, arguments %q; want both tools in order", name, names, args)
		}
	}
}

func TestAnApprovalIsKeptAsPostedAndAPromptOfTooManyToolsIsTooLarge(t *testing.T) {
	f := NewFake()
	if f.MaxTools != 20 {
		t.Errorf("MaxTools = %d, want 20", f.MaxTools)
	}
	prompt := chat.Prompt{ID: "ix-1", Tools: []chat.Tool{{Name: "kubectl", Args: "get pods"}}}
	ref, err := f.Post(ctx, tgt, chat.Ask(prompt), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Messages()[0]; got.Ref != ref || !reflect.DeepEqual(got.Out.Prompt, &prompt) {
		t.Errorf("the fake kept %+v, want the prompt of tools as posted", got.Out)
	}
	big := chat.Prompt{ID: "ix-2", Tools: make([]chat.Tool, f.MaxTools+1)}
	if err := f.Fits(big); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("Fits of %d tools = %v, want ErrTooLarge", len(big.Tools), err)
	}
	if _, err := f.Post(ctx, tgt, chat.Ask(big), "t2"); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("Post of %d tools = %v, want ErrTooLarge", len(big.Tools), err)
	}
	if err := f.Fits(chat.Prompt{ID: "ix-3", Tools: big.Tools[:f.MaxTools]}); err != nil {
		t.Errorf("Fits of %d tools = %v", f.MaxTools, err)
	}
}

func TestAPressOfAnApprovalReachesTheSinkWithItsAction(t *testing.T) {
	f := NewFake()
	rec := &sinkRec{}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = f.Run(run, rec) }()
	press := chat.Interaction{User: "U1", PromptID: "ix-1", Action: chat.ActionDenyAll, Answers: []chat.Answer{{Question: 1, Values: []string{chat.ToolDenyValue}}}}
	if err := f.Press(ctx, press); err != nil {
		t.Fatal(err)
	}
	if len(rec.presses) != 1 || !reflect.DeepEqual(rec.presses[0], press) {
		t.Errorf("the sink got %+v, want the press whole, action included", rec.presses)
	}
}

func TestATooLongToolIsRefusedNotCutAndALongEndingIsShown(t *testing.T) {
	f := NewFake()
	f.SetCaps(chat.Caps{MaxText: 100, Limits: map[chat.Where]int{chat.WhereToolName: 20, chat.WhereToolArgs: 50}})
	tool := func(name, args string) chat.Prompt {
		return chat.Prompt{ID: "ix-1", Tools: []chat.Tool{{Name: "ok", Args: "{}"}, {Name: name, Args: args}}}
	}
	for name, tc := range map[string]struct {
		p    chat.Prompt
		fits bool
	}{
		"at both limits":                 {tool(strings.Repeat("n", 20), strings.Repeat("a", 50)), true},
		"a name one unit over":           {tool(strings.Repeat("n", 21), "{}"), false},
		"arguments one unit over":        {tool("n", strings.Repeat("a", 51)), false},
		"astral arguments over in units": {tool("n", strings.Repeat("\U0001F389", 26)), false},
		"over only before it is cleaned": {tool("n", strings.Repeat("a", 50)+strings.Repeat("\u200b", 30)), true},
		"a name over by its line breaks": {tool(strings.Repeat("n \n ", 8), "{}"), true},
	} {
		err := f.Fits(tc.p)
		if tc.fits && err != nil || !tc.fits && !errors.Is(err, chat.ErrTooLarge) {
			t.Errorf("%s: Fits = %v, want fits %t", name, err, tc.fits)
		}
		if _, err := f.Post(ctx, tgt, chat.Ask(tc.p), "t-"+name); tc.fits != (err == nil) {
			t.Errorf("%s: Post = %v, want it accepted: %t", name, err, tc.fits)
		}
	}
	over := tool(strings.Repeat("n", 21), strings.Repeat("a", 51))
	if _, err := f.Post(ctx, tgt, chat.End(chat.Ending{Outcome: chat.TimedOut, Prompt: over}), "t-end"); err != nil {
		t.Errorf("Post of an ending of a tool over the limit = %v, want it accepted: an ending is cut, not refused", err)
	}
}

func TestAThreadIsBusyFromTheMarkUntilTheClearOrAPostInIt(t *testing.T) {
	f := NewFake()
	other := chat.Target{Channel: "C1", Thread: "2.2"}
	for _, step := range []struct {
		name string
		do   func() error
		want [2]bool // busy in tgt, and in other
	}{
		{"nothing yet", func() error { return nil }, [2]bool{false, false}},
		{"a mark", func() error { return f.MarkBusy(ctx, tgt, true) }, [2]bool{true, false}},
		{"the mark again", func() error { return f.MarkBusy(ctx, tgt, true) }, [2]bool{true, false}},
		{"a post in another thread", func() error { _, err := f.Post(ctx, other, say("x"), "t1"); return err }, [2]bool{true, false}},
		{"a post in the thread", func() error { _, err := f.Post(ctx, tgt, say("x"), "t2"); return err }, [2]bool{false, false}},
		{"a mark after it", func() error { return f.MarkBusy(ctx, tgt, true) }, [2]bool{true, false}},
		{"a stream in the thread", func() error {
			if err := f.MarkBusy(ctx, tgt, true); err != nil {
				return err
			}
			_, err := f.Stream(ctx, tgt, "U1", "x")
			return err
		}, [2]bool{false, false}},
		{"a mark and a clear", func() error {
			if err := f.MarkBusy(ctx, tgt, true); err != nil {
				return err
			}
			return f.MarkBusy(ctx, tgt, false)
		}, [2]bool{false, false}},
		{"a clear of nothing", func() error { return f.MarkBusy(ctx, tgt, false) }, [2]bool{false, false}},
	} {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := [2]bool{f.Busy(tgt), f.Busy(other)}; got != step.want {
			t.Errorf("%s: busy = %v, want %v", step.name, got, step.want)
		}
	}
	if got := f.BusyCalls(); len(got) != 7 || got[0] != (BusyCall{Target: tgt, Busy: true}) || got[6].Busy {
		t.Errorf("BusyCalls = %+v, want the seven calls in order, each as made", got)
	}
}

func TestAMarkNeedsAThreadAndTheCaps(t *testing.T) {
	f := NewFake()
	if err := f.MarkBusy(ctx, chat.Target{Channel: "C1"}, true); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("MarkBusy of no thread = %v, want ErrInvalid", err)
	}
	f.SetCaps(chat.Caps{Streams: true, MaxText: 4000})
	if err := f.MarkBusy(ctx, tgt, true); !errors.Is(err, chat.ErrNoBusy) || f.Busy(tgt) {
		t.Errorf("MarkBusy with Busy false = %v, busy %v, want ErrNoBusy and no mark", err, f.Busy(tgt))
	}
	f.SetCaps(chat.Caps{Busy: true, MaxText: 4000})
	boom := errors.New("boom")
	f.FailNext("MarkBusy", boom)
	if err := f.MarkBusy(ctx, tgt, true); !errors.Is(err, boom) || f.Busy(tgt) {
		t.Errorf("a failed MarkBusy = %v, busy %v, want the error and no mark", err, f.Busy(tgt))
	}
}
