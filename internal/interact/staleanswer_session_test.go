package interact_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// askedAgain is task-1 paused on the question request, asked in the status message msgID: a later question of the same task
// is another message, as kagent sends it.
func askedAgain(request, msgID string) a2a.Task {
	task := askUser(request)
	task.Status.Message.ID = msgID
	return task
}

// decision is user's answer, choice "a", to the question of row, as the press pipeline hands it to the decider.
func decision(t *testing.T, row store.Interaction, b interact.Binding, user string, at time.Time) interact.PressDecision {
	t.Helper()
	var in dialect.Interaction
	if err := json.Unmarshal(row.Request, &in); err != nil {
		t.Fatal(err)
	}
	return interact.PressDecision{Interaction: row, In: in, Answer: dialect.Answer{RequestID: in.RequestID, Answers: [][]string{{"a"}}},
		User: user, PressedAt: at, BindingName: "ns/b", Binding: b}
}

// TestARecordedAnswerSentAgainDoesNotAnswerALaterQuestion runs the real session manager, the intake behind its tap and the
// decider over one store. Alice's press decides question A of task-1, and the answer continues the task, which then asks
// question B. Alice's press on A comes again: a delivery Slack makes after its key is forgotten, a press with no key, or Alice
// pressing again on a prompt whose controls were never taken off. It is the winner's press decided again, which sends A's
// recorded answer. The task is paused on B, not on A, so the session refuses it and nothing is told: the agent never gets A's
// answer for B, and B stays open for its own answer, which is then taken.
func TestARecordedAnswerSentAgainDoesNotAnswerALaterQuestion(t *testing.T) {
	ag := newScriptedAgent()
	ag.sendAt = func(int) a2a.Task { return askedAgain("req-A", "status-A") }
	ag.answerTo = func(n int) *a2a.Task {
		if n == 0 {
			next := askedAgain("req-B", "status-B")
			return &next
		}
		return nil
	}
	p := start(t, sqlitetest.Open(t), ag, atTimes(&clockAt{}, t0.Add(time.Minute)), true)
	p.binds.b.AskUser = true
	b := p.binds.b
	eph := &ephemerals{}
	d, err := interact.NewDecider(decideCfg(interact.DecideConfig{Store: p.st, Auditor: p.aud, Ephemerals: chattest.Same[interact.Ephemeral](eph),
		Session: p.mgr, Sessions: p.st, Outbox: &enqueued{}, Now: p.clk.now, Logger: testutil.DiscardLogger()}))
	if err != nil {
		t.Fatal(err)
	}
	question := func(request, msgID string) store.Interaction {
		t.Helper()
		in, err := (&kagentv1.Dialect{}).ParseInteraction(askedAgain(request, msgID))
		if err != nil || in == nil {
			t.Fatal(in, err)
		}
		var row store.Interaction
		testutil.Eventually(t, "the prompt of "+request, func() bool {
			row, err = p.st.GetInteraction(t.Context(), dialect.InteractionID(*in))
			return err == nil && row.MessageRef != ""
		}, testutil.Within(10*time.Second), testutil.Every(2*time.Millisecond))
		return row
	}

	if _, err := p.mgr.Submit(t.Context(), session.Inbound{Key: key, Binding: "ns/b", MessageID: "slack:main:C1:1.2", MessageRef: "1.2", UserID: "U1", Text: "hello", Create: true}); err != nil {
		t.Fatal(err)
	}
	a := question("req-A", "status-A")
	if err := d.Decide(t.Context(), decision(t, a, b, "UALICE1", p.clk.now())); err != nil {
		t.Fatalf("Alice's press on A = %v", err)
	}
	q := question("req-B", "status-B") // the answer to A continued the task, which asks B

	if err := d.Decide(t.Context(), decision(t, a, b, "UALICE1", p.clk.now())); err != nil {
		t.Fatalf("Alice's press on A again = %v; want it done", err)
	}
	if err := d.Decide(t.Context(), decision(t, q, b, "UBOB22", p.clk.now())); err != nil {
		t.Fatalf("Bob's press on B = %v; want it decided and sent", err)
	}
	testutil.Eventually(t, "the task to complete", func() bool { return !p.mgr.HoldsTask(key, "task-1") }, testutil.Within(10*time.Second), testutil.Every(2*time.Millisecond))

	ag.mu.Lock()
	var sent []string
	for _, m := range ag.answers {
		sent = append(sent, m.ID)
	}
	ag.mu.Unlock()
	if want := []string{dialect.AnswerID(a.ID), dialect.AnswerID(q.ID)}; len(sent) != 2 || sent[0] != want[0] || sent[1] != want[1] {
		t.Errorf("the agent was sent %q; want A's answer, then B's, and never A's again", sent)
	}
	if row, err := p.st.GetInteraction(t.Context(), q.ID); err != nil || row.State != store.InteractionAnswered || row.Decider != "UBOB22" || row.DeliveredAt.IsZero() {
		t.Errorf("B is %+v, %v; want it answered by UBOB22 and delivered", row, err)
	}
	if got := eph.list(); len(got) != 0 {
		t.Errorf("told %q; want nobody told: the press decided again says nothing, and Bob's was taken", got)
	}
}
