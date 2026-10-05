package session

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// The renderer is told which buttons a turn's final message carries (spec 8.7): the investigation an alert started
// always, whoever the Binding is, and the answer to a person only when the Binding has feedback.enabled.
func TestTurnRefSaysWhichFeedbackTheFinalMessageCarries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		user     string // who wrote the turn; empty for an alert's first prompt
		feedback bool   // the Binding's feedback.enabled
		want     store.FeedbackKind
	}{
		{"an alert's first prompt, on a Binding without feedback", "", false, store.FeedbackInvestigation},
		{"an alert's first prompt, on a Binding with it", "", true, store.FeedbackInvestigation},
		{"a person's message, on a Binding with feedback", "U1", true, store.FeedbackAnswer},
		{"a person's message, on a Binding without", "U1", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sink := f.live()
			f.feedback = tc.feedback
			in := inbound("1")
			in.UserID = tc.user
			if _, err := f.submit(in); err != nil {
				t.Fatal(err)
			}
			<-sink.finals
			sink.mu.Lock()
			defer sink.mu.Unlock()
			if len(sink.refs) != 1 || sink.refs[0].Feedback != tc.want {
				t.Errorf("turn refs = %+v; want one with feedback %q", sink.refs, tc.want)
			}
		})
	}
}

// The alert, and the agent of the Binding, are written when the session is made and by nothing after: a reply in the thread, which names none, neither
// clears it nor replaces it, and a message that creates no session writes none.
func TestSubmitKeepsTheAlertOfTheSession(t *testing.T) {
	f := newFixture(t)
	first := inbound("1")
	first.UserID, first.Alert = "", "KubePodCrashLooping"
	if _, err := f.submit(first); err != nil {
		t.Fatal(err)
	}
	reply := inbound("2")
	reply.Create, reply.Alert = false, "SomethingElse"
	if _, err := f.submit(reply); err != nil {
		t.Fatal(err)
	}
	if sess, err := f.session(); err != nil || sess.Alert != "KubePodCrashLooping" || sess.Agent != "kagent/agent-1" {
		t.Errorf("session = %+v, %v; want the alert of the message that made it, and the agent of its Binding", sess, err)
	}
}

// A task found again after a restart is rendered under the turn the session recorded for it, and its final message carries
// the same buttons its first rendering would have: the author of the recorded turn says which.
func TestRecoveredTurnCarriesTheFeedbackOfItsRecordedTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		user string
		want store.FeedbackKind
	}{
		{"an alert's first prompt", SystemUser, store.FeedbackInvestigation},
		{"a person's turn on a Binding with feedback", "U1", store.FeedbackAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sink := restarted(t)
			f.feedback = true
			recordedTurn(t, f, store.TaskTurn{ID: inbound("1").MessageID, User: tc.user})
			f.recover(t)
			waitFinal(t, sink)
			sink.mu.Lock()
			defer sink.mu.Unlock()
			if len(sink.refs) != 1 || sink.refs[0].Feedback != tc.want {
				t.Errorf("turn refs = %+v; want one with feedback %q", sink.refs, tc.want)
			}
		})
	}
}
