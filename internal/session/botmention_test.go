package session

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// A session that a bot's mention made takes only people's mentions, on a Binding with threadReplies: all too; a session made another
// way keeps its Binding's rule. A bot's next mention is a start, and is not held back.
func TestASessionABotsMentionMadeTakesOnlyMentions(t *testing.T) {
	mention, alert := store.Origin{Source: store.OriginBotMention}, store.Origin{Source: "botMessage"}
	for _, tc := range []struct {
		name      string
		origin    store.Origin
		mentioned bool
		create    bool
		want      Disposition
	}{
		{"a person's reply with no mention", mention, false, false, Unmentioned},
		{"a person's reply with a mention", mention, true, false, Accepted},
		{"a bot's next mention", mention, true, true, Accepted},
		{"a person's reply in an alert's thread", alert, false, false, Accepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.threadReplies = v1alpha1.ThreadRepliesAll
			seeded := store.Session{Key: key, ContextID: "c1", Binding: "ns/b1", Origin: tc.origin, LastActivity: t0}
			if err := f.st.UpsertSession(t.Context(), seeded); err != nil {
				t.Fatal(err)
			}
			in := reply("1700.02", tc.mentioned)
			in.Create = tc.create
			if res, err := f.submitAny(in); err != nil || res.Disposition != tc.want {
				t.Fatalf("Submit = %+v, %v; want %s", res, err, tc.want)
			}
		})
	}
}

// With its Binding gone, a session a bot's mention made still says nothing to a reply that was not asking for anything.
func TestASessionABotsMentionMadeWithItsBindingGone(t *testing.T) {
	f := newFixture(t)
	seeded := store.Session{Key: key, ContextID: "c1", Binding: "ns/gone", Origin: store.Origin{Source: store.OriginBotMention}, LastActivity: t0}
	if err := f.st.UpsertSession(t.Context(), seeded); err != nil {
		t.Fatal(err)
	}
	res, err := f.submitAny(reply("1700.02", false))
	if err != nil || res.Admission != ignored(IgnoredBindingGone) {
		t.Fatalf("Submit = %+v, %v; want the reply left alone", res, err)
	}
}

// The first turn of a mention's session tells the thread what the session takes: its final message ends with the follow-up hint, whatever
// the Binding's mode is.
func TestTheTurnOfABotsMentionCarriesTheMentionMode(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	f.threadReplies = v1alpha1.ThreadRepliesAll
	in := inbound("1")
	in.UserID, in.WholeThread, in.Origin = "", true, store.Origin{Source: store.OriginBotMention}
	if _, err := f.submit(in); err != nil {
		t.Fatal(err)
	}
	<-sink.finals
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.refs) != 1 || sink.refs[0].ThreadReplies != v1alpha1.ThreadRepliesMention {
		t.Errorf("turn refs = %+v; want one in mention mode", sink.refs)
	}
}

// Takes says what Submit would do with a mention, and writes nothing.
func TestTakesSaysWhatSubmitWouldDoAndWritesNothing(t *testing.T) {
	const expired = 2 * time.Hour // past ns/b1's threadTTL of 1 h
	start := func() Inbound {
		in := inbound("1700.02")
		in.Create = true
		return in
	}
	t.Run("a thread with no session", func(t *testing.T) {
		f := newFixture(t)
		if got, err := f.m.Takes(t.Context(), start()); err != nil || !got {
			t.Errorf("Takes = %v, %v; want true", got, err)
		}
		if _, err := f.session(); err == nil {
			t.Error("Takes made a session")
		}
	})
	t.Run("a live session", func(t *testing.T) {
		f := newFixture(t)
		f.seed("ns/b1", 0)
		if got, err := f.m.Takes(t.Context(), start()); err != nil || !got {
			t.Errorf("Takes = %v, %v; want true", got, err)
		}
		if f.st.touches != 0 || len(f.drain()) != 0 {
			t.Errorf("%d touches and a queued turn; want nothing written", f.st.touches)
		}
	})
	t.Run("a session past its threadTTL", func(t *testing.T) {
		f := newFixture(t)
		f.seed("ns/b1", expired)
		if got, err := f.m.Takes(t.Context(), start()); err != nil || got {
			t.Errorf("Takes = %v, %v; want false", got, err)
		}
	})
	t.Run("a session whose Binding left", func(t *testing.T) {
		f := newFixture(t)
		f.seed("ns/gone", 0)
		if got, err := f.m.Takes(t.Context(), start()); err != nil || got {
			t.Errorf("Takes = %v, %v; want false", got, err)
		}
	})
}

// A mention's task is recorded as the mention's, and stays so when a person's typed answer takes over the turn's message and
// reaction: the limit on running mentions reads it, and it decides which buttons the final message carries.
func TestAMentionsTaskStaysTheMentionsThroughAnAnswer(t *testing.T) {
	f := newFixture(t)
	f.ackConfigured, f.ackEnabled, f.ackEmoji = true, true, "eyes"
	f.textReplies = true
	answerStarted, releaseAnswer := make(chan struct{}), make(chan struct{})
	f.agent.script = pauseFirst
	f.agent.get = func(a2a.TaskID) (*a2a.Task, error) {
		return &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, nil
	}
	f.agent.answer = func(_ *a2a.Message, h agent.Handler) error {
		close(answerStarted)
		<-releaseAnswer
		return turn(h, 0, a2a.TaskStateCompleted)
	}
	sink := f.live()
	first := inbound("1")
	first.UserID, first.WholeThread, first.MessageRef = "", true, "1700.05"
	if _, err := f.submit(first); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if sess, err := f.session(); err != nil || sess.Turn.Mention != "1700.05" {
		t.Fatalf("session = %+v, %v; want the task the mention's", sess, err)
	}
	f.setTextReply(func(ctx context.Context, k Key, r store.Reply, task a2a.Task) (TextReplyOutcome, error) {
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(r.Text))
		msg.ID, msg.TaskID, msg.ContextID = r.ID+"/answer", task.ID, task.ContextID
		return TextReplyConsumed, f.m.Answer(ctx, k, msg)
	})
	answer := inbound("2")
	answer.MessageRef = "1700.09"
	if _, err := f.submit(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-answerStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the answer was not sent")
	}
	if sess, err := f.session(); err != nil || sess.Turn.MessageRef != answer.MessageRef || sess.Turn.Mention != "1700.05" {
		t.Errorf("session = %+v, %v; want the answer's message and the mention's task", sess, err)
	}
	close(releaseAnswer)
	waitFinal(t, sink)
}

// The turn a task records is the mention's if the mention's reply began it, or if the task was found again as the mention's, and no
// other turn's.
func TestTheTurnOfATaskSaysWhetherItIsAMentions(t *testing.T) {
	for _, tc := range []struct {
		name string
		t    tstate
		want string
	}{
		{"a mention's turn", tstate{r: store.Reply{ID: "m", WholeThread: true, MessageRef: "1.5"}}, "1.5"},
		{"a person's turn", tstate{r: store.Reply{ID: "p", MessageRef: "1.9"}}, ""},
		{"a task found again as a mention's", tstate{began: store.TaskTurn{ID: "m", MessageRef: "1.9", Mention: "1.5"}}, "1.5"},
		{"a task found again as a person's", tstate{began: store.TaskTurn{ID: "p", MessageRef: "1.9"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.t.outputTurn().Mention; got != tc.want {
				t.Errorf("Mention = %q; want %q", got, tc.want)
			}
		})
	}
}
