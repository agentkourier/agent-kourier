package interact_test

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// cardSink is the renderer that says what its turns' replies show: the cards it is given, which it hands out only once the
// handler has seen the final event, as the renderer knows them only then.
type cardSink struct {
	*tapInner
	cards map[string]string
}

func (s cardSink) TurnCards(ctx context.Context, ref session.TurnRef) (agent.Handler, func() map[string]string) {
	return s.Turn(ctx, ref), func() map[string]string {
		s.mu.Lock()
		defer s.mu.Unlock()
		if n := len(s.events); n == 0 || !isFinal(s.events[n-1]) {
			return nil
		}
		return s.cards
	}
}

func isFinal(e agent.Event) bool { _, ok := e.(agent.FinalEvent); return ok }

var _ interact.CardSink = cardSink{}

// The tap hands the pause on with what the renderer says its reply shows of the calls the pause waits on, asked once the
// renderer has had the final event. A sink that says nothing of cards hands on none.
func TestThePauseCarriesTheCardsTheRendererShows(t *testing.T) {
	cards := map[string]string{"c2": "a=3, b=5"}
	for _, tc := range []struct {
		name string
		sink func(*tapInner) session.Sink
		want map[string]string
	}{
		{"a renderer that shows cards", func(in *tapInner) session.Sink { return cardSink{tapInner: in, cards: cards} }, cards},
		{"a sink that says nothing of cards", func(in *tapInner) session.Sink { return in }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &tapInner{}
			r := newTap(t, func(c *interact.TapConfig) { c.Sink = tc.sink(inner) })
			task := pausedTask("task-1", "msg-1", d0)
			r.tap.Turn(t.Context(), tapRef)(agent.FinalEvent{Task: &task, AwaitingApproval: []string{"c2"}})
			testutil.Eventually(t, "the hand-off", func() bool { return r.pauses.calls() == 1 })
			r.pauses.mu.Lock()
			got := r.pauses.got[0].Cards
			r.pauses.mu.Unlock()
			if !maps.Equal(got, tc.want) {
				t.Errorf("the pause's cards = %v; want %v", got, tc.want)
			}
		})
	}
}

// cardedApproval is a pause for the approval of two calls that have arguments.
func cardedApproval() a2a.Task {
	return pausedOn(map[string]any{"type": "tool_approval_request", "tools": []map[string]any{
		{"id": "t1", "call_id": "c1", "name": "delete_pod", "args": map[string]any{"name": "web-1"}},
		{"id": "t2", "call_id": "c2", "name": "add_numbers", "args": map[string]any{"a": 3, "b": 5}}}})
}

// The ending of an approval names a tool without its arguments when the reply the task paused in shows the tool's call on its
// card with the same summary (chat.Tool.OnCard): what the intake was handed with the pause is kept with the request, so that an
// ending made after a restart, or made again by the repair, says the same. A card of other arguments, or none, keeps them. The
// prompt shows every tool's arguments whole, whatever the cards, and a pause that comes again with no cards, as after a
// restart, changes nothing that was kept.
func TestTheEndingOfAnApprovalLeavesOutTheArgumentsItsReplyShows(t *testing.T) {
	r, task := newRig(t, allowApprovals), cardedApproval()
	cards := map[string]string{"c1": `name="web-1"`, "c2": "a=4, b=5"} // the second card is of other arguments
	if err := r.in.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Task: task, Cards: cards}); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, task)
	posted := r.slack.contents[outbox.ID(row.ID, store.OutboxReply)]
	if posted.Prompt == nil || len(posted.Prompt.Tools) != 2 || posted.Prompt.Tools[0].OnCard || posted.Prompt.Tools[1].OnCard || posted.Prompt.Tools[0].Args == "" {
		t.Fatalf("posted %+v; want the prompt of both tools, each with its arguments and no mark", posted.Prompt)
	}
	if err := r.paused(t, task); err != nil { // the same pause again, with no cards, as recovery hands it on
		t.Fatal(err)
	}
	if again := r.row(t, task); string(again.Request) != string(row.Request) {
		t.Errorf("the stored request became %s; want it kept as %s", again.Request, row.Request)
	}

	answer, err := json.Marshal(dialect.Answer{Decisions: []dialect.Decision{{ToolID: "t1", Approved: true}, {ToolID: "t2"}}})
	if err != nil {
		t.Fatal(err)
	}
	var old dialect.Interaction // the request as a version before the mark stored it
	if err := json.Unmarshal(row.Request, &old); err != nil {
		t.Fatal(err)
	}
	oldRequest, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		state   store.InteractionState
		request []byte
		want    []bool
	}{
		{"answered", store.InteractionAnswered, row.Request, []bool{true, false}},
		{"timed out", store.InteractionTimedOut, row.Request, []bool{true, false}},
		{"cancelled", store.InteractionCancelled, row.Request, []bool{true, false}},
		{"stored before the mark", store.InteractionAnswered, oldRequest, []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ended := row
			ended.State, ended.Request, ended.Answer, ended.Decider = tc.state, tc.request, answer, "UALICE1"
			out, err := interact.ReplacementOutgoing(ended)
			if err != nil || out.Ending == nil {
				t.Fatalf("ReplacementOutgoing = %+v, %v", out, err)
			}
			tools := out.Ending.Prompt.Tools
			if len(tools) != 2 || tools[0].OnCard != tc.want[0] || tools[1].OnCard != tc.want[1] || tools[0].Args == "" || tools[1].Args == "" {
				t.Errorf("ending tools %+v; want on their cards %v, each with its arguments", tools, tc.want)
			}
		})
	}
}

// The sweep that times an approval out makes its ending from the same stored request: the tool on its card is named without its
// arguments, and the other keeps them.
func TestTheSweepsEndingLeavesOutTheArgumentsItsReplyShows(t *testing.T) {
	var request map[string]any
	if err := json.Unmarshal(approvalOfTask(t, "task-1"), &request); err != nil {
		t.Fatal(err)
	}
	request["on_card"] = []string{"t1"}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	r := newSweepRig(t)
	r.approvalRow(t, "ix-1", "task-1", store.InteractionToolApproval, raw, t0.Add(timeout))
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(r.upd.ends) != 1 {
		t.Fatalf("%d endings; want the prompt replaced once", len(r.upd.ends))
	}
	if tools := r.upd.ends[0].Prompt.Tools; len(tools) != 2 || !tools[0].OnCard || tools[1].OnCard {
		t.Errorf("ending tools %+v; want the first on its card and the second not", tools)
	}
}
