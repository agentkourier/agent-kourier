package session

import (
	"context"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// personStart is the first turn of a session that a personMessage trigger begins: the person's own message, at the root of
// the thread, which the starter submits with its author.
func personStart() Inbound {
	in := inbound(key.Thread)
	in.Text, in.Origin = "A person reported something.", store.Origin{Source: store.OriginPersonMessage}
	return in
}

// A session that a person's message made takes only mentions, on a Binding with threadReplies: all too, as one that a bot's mention
// made does: the people who discuss the report in its thread do not make every word a prompt. A mentioned reply is a turn.
func TestASessionAPersonsMessageMadeTakesOnlyMentions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mentioned bool
		want      Disposition
	}{
		{"a reply with no mention", false, Unmentioned},
		{"a reply with a mention", true, Accepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.threadReplies = v1alpha1.ThreadRepliesAll
			seeded := store.Session{Key: key, ContextID: "c1", Binding: "ns/b1", Origin: store.Origin{Source: store.OriginPersonMessage}, LastActivity: t0}
			if err := f.st.UpsertSession(t.Context(), seeded); err != nil {
				t.Fatal(err)
			}
			if res, err := f.submitAny(reply("1700.02", tc.mentioned)); err != nil || res.Disposition != tc.want {
				t.Fatalf("Submit = %+v, %v; want %s", res, err, tc.want)
			}
		})
	}
}

// With its Binding gone, a session a person's message made says nothing to a reply that was not asking for anything.
func TestASessionAPersonsMessageMadeWithItsBindingGone(t *testing.T) {
	f := newFixture(t)
	seeded := store.Session{Key: key, ContextID: "c1", Binding: "ns/gone", Origin: store.Origin{Source: store.OriginPersonMessage}, LastActivity: t0}
	if err := f.st.UpsertSession(t.Context(), seeded); err != nil {
		t.Fatal(err)
	}
	res, err := f.submitAny(reply("1700.02", false))
	if err != nil || res.Admission != ignored(IgnoredBindingGone) {
		t.Fatalf("Submit = %+v, %v; want the reply left alone", res, err)
	}
}

// The first turn of a person's message is that person's, like any turn a person writes: the agent sees their attribution in front
// of the prompt, the audit rows name them, the answer is shown to them, and the final message carries rating buttons only on a
// Binding with feedback.enabled, as an answer's does. It tells the thread that a follow-up needs a mention, whatever the Binding's
// mode is.
func TestTheTurnOfAPersonsMessageIsThePersons(t *testing.T) {
	for _, tc := range []struct {
		name     string
		feedback bool
		want     store.FeedbackKind
	}{
		{"without feedback", false, ""},
		{"with feedback", true, store.FeedbackAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.threadReplies, f.feedback = v1alpha1.ThreadRepliesAll, tc.feedback
			f.attr, f.platform = (&kagentv1.Dialect{}).Attribute, dialect.Platform{ID: "slack", Name: "Slack"}
			f.m.cfg.Users = fakeUsers{"U1": {ID: "U1", Name: "Ada", Email: "ada@example.com"}}
			if _, err := f.submit(personStart()); err != nil {
				t.Fatal(err)
			}
			<-sink.finals
			f.agent.mu.Lock()
			msg := f.agent.msgs[0]
			f.agent.mu.Unlock()
			if want := "[Slack: Ada (U1) <ada@example.com>]\nA person reported something."; len(msg.Parts) != 1 || msg.Parts[0].Text() != want {
				t.Errorf("turn = %+v; want one part %q", msg.Parts, want)
			}
			sink.mu.Lock()
			refs := sink.refs
			sink.mu.Unlock()
			if len(refs) != 1 || refs[0].UserID != "U1" || refs[0].Feedback != tc.want || refs[0].ThreadReplies != v1alpha1.ThreadRepliesMention {
				t.Errorf("turn refs = %+v; want one shown to U1, with feedback %q, in mention mode", refs, tc.want)
			}
		})
	}
}

// The agent call of a person's message is audited as that person's, here a send the agent refused.
func TestTheCallOfAPersonsMessageIsAuditedAsThePersons(t *testing.T) {
	f := newFixture(t)
	f.agent.script = func(context.Context, int, agent.Handler) error { return agent.ErrForbidden }
	f.live()
	if _, err := f.submit(personStart()); err != nil {
		t.Fatal(err)
	}
	f.retired(t)
	f.amu.Lock()
	defer f.amu.Unlock()
	if len(f.actors) != 1 || f.actors[0].User != "U1" || f.actors[0].Binding != "ns/b1" {
		t.Errorf("audit rows by %+v; want one, by the person U1 of ns/b1", f.actors)
	}
}

// The task that a person's message begins is recorded as that start's, for the limit on running starts of its trigger to read; a
// later turn of the session, a mentioned reply, begins a task that is no start's.
func TestTheTaskOfAPersonsMessageIsTheStarts(t *testing.T) {
	f := newFixture(t)
	sink := f.live()
	if _, err := f.submit(personStart()); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if sess, err := f.session(); err != nil || sess.Turn.Mention != key.Thread {
		t.Fatalf("session = %+v, %v; want the task the start's, %s", sess, err, key.Thread)
	}
	if _, err := f.submit(reply("1700.05", true)); err != nil {
		t.Fatal(err)
	}
	waitFinal(t, sink)
	if sess, err := f.session(); err != nil || sess.Turn.MessageRef != "1700.05" || sess.Turn.Mention != "" {
		t.Errorf("session = %+v, %v; want the reply's task, no start's", sess, err)
	}
}

// The turn a task records holds a start's place if a bot's mention began it, or the person's message at the root of a session that
// a personMessage trigger made, and no other turn's: not a reply in that session, and not the root of an alert's.
func TestTheTurnOfATaskSaysWhoseStartItIs(t *testing.T) {
	person, alert := store.Session{Origin: store.Origin{Source: store.OriginPersonMessage}}, store.Session{Origin: store.Origin{Source: "botMessage"}}
	for _, tc := range []struct {
		name string
		t    tstate
		want string
	}{
		{"a person's message", tstate{key: key, sess: person, r: store.Reply{ID: "p", MessageRef: key.Thread}}, key.Thread},
		{"a reply in its thread", tstate{key: key, sess: person, r: store.Reply{ID: "r", MessageRef: "1700.09"}}, ""},
		{"an alert", tstate{key: key, sess: alert, r: store.Reply{ID: "a", MessageRef: key.Thread}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.t.outputTurn().Mention; got != tc.want {
				t.Errorf("Mention = %q; want %q", got, tc.want)
			}
		})
	}
}
