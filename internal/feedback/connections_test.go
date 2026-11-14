package feedback_test

import (
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// TestAVoteIsCheckedAndAnsweredOnTheConnectionItCameIn: two apps in one workspace have the same channel and thread IDs, so
// who may vote is asked of the platform of the connection the press came in on, and the voter is thanked by that app.
func TestAVoteIsCheckedAndAnsweredOnTheConnectionItCameIn(t *testing.T) {
	r := newRig(t)
	keyB := store.ConversationKey{Connection: "agent-kourier/team", Channel: key.Channel, Thread: key.Thread}
	if err := r.st.UpsertSession(t.Context(), store.Session{Key: keyB, Binding: bindingID, Agent: agentID, LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	own := turn + "@b"
	r.message(t, keyB, own, "100.9")
	azB, ephB := &authz{members: map[string]bool{"UCAROL": true}}, &ephemerals{}
	h, err := feedback.New(feedback.Config{
		Store: r.st, Sessions: r.st, Auditor: r.aud, Notify: func(store.Press) {}, Now: func() time.Time { return r.now },
		Authorizers: chat.PlatformSet[interact.Authorizer]{key.Connection: r.az, keyB.Connection: azB},
		Ephemerals:  chat.PlatformSet[interact.Ephemeral]{key.Connection: r.eph, keyB.Connection: ephB},
		Logger:      testutil.DiscardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	in := chat.Interaction{
		User: "UCAROL", At: chat.Ref{Platform: "slack", Conn: keyB.Connection, Channel: key.Channel, Thread: key.Thread, ID: "100.9"},
		PromptID: feedback.Value{Kind: store.FeedbackInvestigation, Verdict: store.VerdictUseful, Thread: key.Thread, Turn: own}.String(),
	}
	if err := h.Press(r.tel.Ctx, in); err != nil {
		t.Fatal(err)
	}
	presses, err := r.st.ListPresses(t.Context())
	if err != nil || len(presses) != 1 {
		t.Fatalf("presses = %v, %v", presses, err)
	}
	if res, err := h.Process(r.tel.Ctx, presses[0]); res != interact.Done || err != nil {
		t.Fatalf("Process = %v, %v", res, err)
	}

	if votes, err := r.st.ListVotes(t.Context(), keyB); err != nil || len(votes) != 1 {
		t.Errorf("votes on b = %v, %v; want the one", votes, err)
	}
	if votes := r.votes(t); len(votes) != 0 {
		t.Errorf("votes on a = %v; want none", votes)
	}
	if r.az.asked != 0 || azB.asked == 0 {
		t.Errorf("a was asked %d times and b %d; want b alone", r.az.asked, azB.asked)
	}
	if got := ephB.list(); len(got) != 1 || got[0].target.Conn != keyB.Connection || len(r.eph.list()) != 0 {
		t.Errorf("b told %+v and a told %+v; want the thanks from b's app alone", got, r.eph.list())
	}
}

// TestAVoteOnAnotherConnectionsMessageIsIgnored: the message a press names is the turn's only on the connection of the turn's row.
// A press that came in on another app, whose session has the same channel and thread, names a message that is not the turn's,
// whatever its ID: it is dropped without a word, and no vote is stored.
func TestAVoteOnAnotherConnectionsMessageIsIgnored(t *testing.T) {
	r := newRig(t)
	keyB := store.ConversationKey{Connection: "agent-kourier/team", Channel: key.Channel, Thread: key.Thread}
	if err := r.st.UpsertSession(t.Context(), store.Session{Key: keyB, Binding: bindingID, Agent: agentID, LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	r.members("UCAROL")
	in := chat.Interaction{
		User: "UCAROL", At: chat.Ref{Platform: "slack", Conn: keyB.Connection, Channel: key.Channel, Thread: key.Thread, ID: "100.9"}, // the turn's message ID, on a's app
		PromptID: feedback.Value{Kind: store.FeedbackInvestigation, Verdict: store.VerdictUseful, Thread: key.Thread, Turn: turn}.String(), // the turn of a
	}
	if res, err := r.vote(t, in); res != interact.Done || err != nil {
		t.Fatalf("vote = %v, %v; want it finished", res, err)
	}
	if votes, err := r.st.ListVotes(t.Context(), keyB); err != nil || len(votes) != 0 {
		t.Errorf("votes on b = %v, %v; want none", votes, err)
	}
	if got := r.eph.list(); len(got) != 0 {
		t.Errorf("told %v; want nobody told", got)
	}
}
