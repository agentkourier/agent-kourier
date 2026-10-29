package live_test

import (
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// The tests below pin the steps of the update path (an alert thread, which has no person to stream to): on a platform that
// has steps, the first post shows the status line, since the outbox stores no steps, and every update after it, the final
// included, shows a card per tool call above the text in place of the line.

// alert starts a turn of the update path: no Recipient, in the progress mode given.
func (e *env) alert(t *testing.T, progress v1alpha1.ProgressMode, rated bool) *live.Run {
	t.Helper()
	turn := live.Turn{Mode: v1alpha1.OutputLive, Progress: progress}
	if rated {
		turn.Footer = footer
	}
	run, _ := e.startFor(t, turn)
	return run
}

func TestAnAlertThreadReplyEndsWithACardPerToolCallAboveItsAnswer(t *testing.T) {
	e := setup(t)
	run := e.alert(t, "", false)
	run.Handle(call("c1", "get_pods", map[string]any{"ns": "prod"}))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(result("c1", map[string]any{"items": 3}))
	run.Handle(call("c2", "get_logs", nil))
	run.Handle(result("c2", map[string]any{"error": "forbidden"}))
	run.Handle(call("c3", "get_rollout", nil))
	run.Handle(result("c3", map[string]any{"revision": 7}))
	run.Handle(text("a", "Rollout 7 broke the pods."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)

	post := e.rec.sent("/chat.postMessage")
	if len(post) != 1 || !strings.Contains(post[0].Get("blocks"), "Calling get_pods(ns=") || strings.Contains(post[0].Get("blocks"), "task_card") {
		t.Fatalf("posts %v; want one, with the status line and no card: the outbox stores no steps", post)
	}
	th := e.fake.Thread("C1", e.root)
	if len(th) != 2 {
		t.Fatalf("thread %+v; want the root and one reply", th)
	}
	m := th[1]
	want := []fakeslack.Task{
		{ID: "s1", Title: "get_pods", Status: "complete", Details: `ns="prod"`},
		{ID: "s2", Title: "get_logs", Status: "error"},
		{ID: "s3", Title: "get_rollout", Status: "complete"},
	}
	if got := m.Tasks(); !slices.Equal(got, want) {
		t.Fatalf("tasks %+v; want %+v: a card per call, in call order, each as its result left it", got, want)
	}
	if got := strings.Join(blockTypes(t, m.Blocks), ","); got != "task_card,task_card,task_card,markdown" {
		t.Errorf("blocks %s; want the cards above the answer", got)
	}
	if m.Text != "Rollout 7 broke the pods." {
		t.Errorf("fallback %q; want the answer", m.Text)
	}
	for _, f := range e.rec.sent("/chat.update") {
		if b := f.Get("blocks"); strings.Contains(b, "Calling") || strings.Contains(b, "Ran ") {
			t.Errorf("an update shows the status line beside the cards, which say the same: %s", b)
		}
	}
}

// The mode decides it: activity names no tool, and a final turn shows nothing until it ends, so neither shows a card. A
// platform without steps keeps the status line.
func TestAnAlertThreadReplyShowsNoCardsWhereTheTurnOrThePlatformHasNone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     v1alpha1.OutputMode
		progress v1alpha1.ProgressMode
		noSteps  bool
	}{
		{"activity", v1alpha1.OutputLive, v1alpha1.ProgressActivity, false},
		{"final", v1alpha1.OutputFinal, "", false},
		{"no steps", v1alpha1.OutputLive, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			if tc.noSteps {
				e.spy(t, false, nil)
			}
			run, _ := e.startFor(t, live.Turn{Mode: tc.mode, Progress: tc.progress})
			run.Handle(call("c1", "search", nil))
			run.Handle(result("c1", map[string]any{"items": 1}))
			run.Handle(text("a", "Found it."))
			run.Handle(agent.FinalEvent{})
			e.finish(t, run)
			for _, b := range e.requests() {
				if strings.Contains(b, "task_card") {
					t.Fatalf("a message shows a card: %s", b)
				}
			}
			if m := e.reply(); m.Text != "Found it." {
				t.Errorf("reply %q; want the answer", m.Text)
			}
		})
	}
}

// A long run shows the latest cards, under a line that counts the ones before them, and the footer's buttons still render.
func TestAnAlertThreadReplyShowsTheLatestCardsAndCountsTheRest(t *testing.T) {
	e := setup(t)
	release := e.hold()
	t.Cleanup(release)
	run := e.alert(t, "", true)
	const calls = 25
	for i := 1; i <= calls; i++ {
		id := fmt.Sprintf("c%d", i)
		run.Handle(call(id, fmt.Sprintf("tool%d", i), nil))
		run.Handle(result(id, map[string]any{"ok": true}))
	}
	run.Handle(call("c1", "tool1", nil)) // a call that is no longer shown, reported again: it is not a new card
	run.Handle(text("a", "Done."))
	run.Handle(agent.FinalEvent{})
	release()
	e.finish(t, run)

	m := e.reply()
	got := m.Tasks()
	if len(got) != 20 || got[0].ID != "s6" || got[0].Title != "tool6" || got[19].ID != "s25" || got[19].Title != "tool25" {
		t.Fatalf("tasks %+v; want the latest 20, tool6 to tool25, each keeping its ID", got)
	}
	types := blockTypes(t, m.Blocks)
	if types[0] != "context" || types[1] != "task_card" || !strings.Contains(string(m.Blocks), "5 earlier steps are not shown.") {
		t.Errorf("blocks %s; want the line that counts the 5 earlier steps above the cards", m.Blocks)
	}
	if buttons, questions := hasFooter(string(m.Blocks)); !buttons || questions != 1 {
		t.Errorf("blocks %s; want the footer and its buttons under the answer", m.Blocks)
	}
}

// A refusal of the cards, before the reply has any text, sends the update again as a post shows it, with the status line,
// and the turn goes on to its answer without cards: the steps never cost the reply.
func TestAnAlertThreadReplyWhoseCardsAreRefusedShowsTheLineAndItsAnswer(t *testing.T) {
	e := setup(t)
	refused := new(atomic.Int32)
	e.cfg.Platform = refusesSteps{Platform: e.cfg.Platform, refused: refused}
	e.live = newRenderer(t, e.cfg)
	run := e.alert(t, "", false)
	run.Handle(call("c1", "search", nil))
	eventually(t, "the first post", func() bool { return e.reply().Text == "Calling search()" })
	run.Handle(result("c1", map[string]any{"items": 1}))
	e.until(t, "the line, sent again", 3*time.Second, func() bool { return e.reply().Text == "Ran search()" })
	if n := refused.Load(); n != 1 {
		t.Fatalf("%d refusals; want the one", n)
	}
	run.Handle(text("a", "Found it."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if m := e.reply(); m.Text != "Found it." || len(m.Tasks()) != 0 {
		t.Errorf("message %q with tasks %+v; want the answer, with no card", m.Text, m.Tasks())
	}
	if n := refused.Load(); n != 1 || strings.Contains(e.logs.String(), "giving up") {
		t.Errorf("%d refusals, log:\n%s\nwant one refusal, and the turn not given up on", n, e.logs)
	}
}

// A Working… card that an update showed stays when the turn ends with no text, ended as how the turn ended, as a stream's does.
func TestAnAlertThreadReplyKeepsAWorkingCardItShowedWhenItEndsWithNoText(t *testing.T) {
	e := setup(t)
	run := e.alert(t, v1alpha1.ProgressStepsAndActivity, false)
	run.Handle(call("c1", "search", nil))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(result("c1", map[string]any{"items": 1}))
	e.until(t, "the Working… card", 8*time.Second, func() bool { got := e.tasks(); return len(got) == 2 && got[1].Title == "Working…" })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if got := e.reply().Tasks(); len(got) != 2 || got[0].Title != "search" || got[1].Title != "Finished" || got[1].Status != "complete" {
		t.Errorf("tasks %+v; want the call's card and the Working… card ended as Finished", got)
	}
}

// A pause for an approval leaves the call it waits on as a card that says so, as a stream's does.
func TestAnAlertThreadReplyThatPausesForAnApprovalSaysSoOnTheCard(t *testing.T) {
	e := setup(t)
	run := e.alert(t, "", false)
	run.Handle(call("c1", "search", nil))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(result("c1", map[string]any{"items": 1}))
	run.Handle(call("c2", "rollback", map[string]any{"to": 6}))
	run.Handle(result("c2", map[string]any{"error": "the call requires confirmation"}))
	run.Handle(agent.FinalEvent{Task: &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}, AwaitingApproval: []string{"c2"}})
	e.finish(t, run)
	want := []fakeslack.Task{
		{ID: "s1", Title: "search", Status: "complete"},
		{ID: "s2", Title: "Approval requested: rollback", Status: "complete", Details: "to=6"},
	}
	if got := e.reply().Tasks(); !slices.Equal(got, want) {
		t.Fatalf("tasks %+v; want %+v", got, want)
	}
}

// A turn whose final comes before its first post posts what it has without steps, "(no output)" for a turn with no text, and
// the update that follows shows the cards: no message is left without them, and none says "(no output)" under them.
func TestAnAlertThreadReplyThatEndsBeforeItsFirstPostStillShowsItsCards(t *testing.T) {
	e := setup(t)
	release := e.hold()
	t.Cleanup(release)
	run := e.alert(t, "", true)
	run.Handle(call("c1", "search", nil))
	run.Handle(result("c1", map[string]any{"items": 1}))
	run.Handle(agent.FinalEvent{})
	release()
	e.finish(t, run)
	m := e.reply()
	if got := m.Tasks(); len(got) != 1 || got[0].Title != "search" || got[0].Status != "complete" {
		t.Fatalf("tasks %+v; want the call's card", got)
	}
	if got := strings.Join(blockTypes(t, m.Blocks), ","); got != "task_card" {
		t.Errorf("blocks %s; want the card and nothing under it: no text to rate, so no footer", m.Blocks)
	}
	if m.Text != "search" {
		t.Errorf("fallback %q; want the card's title, not the post's placeholder", m.Text)
	}
}

// An update that shows cards and no text has a fallback of its own: the status line it replaces is not left as the
// message's text, which a notification shows.
func TestAnAlertThreadReplyOfCardsAloneReplacesTheStatusLinesText(t *testing.T) {
	e := setup(t)
	run := e.alert(t, "", false)
	run.Handle(call("c1", "search", nil))
	eventually(t, "the first post", func() bool { return e.reply().Text == "Calling search()" })
	run.Handle(result("c1", map[string]any{"items": 1}))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if m := e.reply(); m.Text != "search" || len(m.Tasks()) != 1 {
		t.Errorf("message %q with tasks %+v; want the card, and its title as the text", m.Text, m.Tasks())
	}
}

// In steps-and-activity a gap is a Working… card, as in a stream, which the next call takes over: no Working… line beside it.
func TestAnAlertThreadReplyShowsAGapAsACard(t *testing.T) {
	e := setup(t)
	run := e.alert(t, v1alpha1.ProgressStepsAndActivity, false)
	run.Handle(call("c1", "search", nil))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	run.Handle(result("c1", map[string]any{"items": 1}))
	e.until(t, "the Working… card", 8*time.Second, func() bool {
		got := e.reply().Tasks()
		return len(got) == 2 && got[1].Title == "Working…"
	})
	if b := string(e.reply().Blocks); strings.Count(b, "Working…") != 1 {
		t.Errorf("blocks %s; want Working… once, as the card", b)
	}
	run.Handle(text("a", "Found it."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if got := e.reply().Tasks(); len(got) != 2 || got[1].Title != "Wrote the answer" {
		t.Errorf("tasks %+v; want the gap's card ended by the text", got)
	}
}

// A recovered turn finds the message an earlier process posted, with its status line, and updates it with the cards of the
// calls the agent reports again: one message, the cards above the answer, and no second post.
func TestARecoveredAlertThreadReplyUpdatesTheEarlierPostWithItsCards(t *testing.T) {
	e := setup(t)
	if _, err := e.ob.Send(t.Context(), outbox.Post{Source: "turn-1", Role: "reply", Channel: "C1", Thread: e.root, Out: chat.Say(chat.Message{Fallback: "Calling search()"})}); err != nil {
		t.Fatal(err)
	}
	release := e.hold()
	t.Cleanup(release)
	run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recovered: true})
	run.Handle(call("c1", "search", nil)) // the earlier process's call, said again
	run.Handle(result("c1", map[string]any{"items": 1}))
	run.Handle(text("a", "the whole answer"))
	run.Handle(agent.FinalEvent{})
	release()
	e.finish(t, run)
	if posts := len(e.rec.sent("/chat.postMessage")); posts != 1 {
		t.Fatalf("%d posts; want the earlier one, updated", posts)
	}
	m := e.reply()
	if got := m.Tasks(); m.Text != "the whole answer" || len(got) != 1 || got[0].Title != "search" || got[0].Status != "complete" {
		t.Fatalf("message %q with tasks %+v; want the answer under the call's card", m.Text, got)
	}
}
