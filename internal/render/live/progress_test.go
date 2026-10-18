package live_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// These tests are about the progress modes (spec 8.3): what a turn shows while the agent is silent. The renderer's delay
// is the 3 seconds of its default. The clock is a fake one: a test turns it in half seconds until the thing it waits for
// is written, and the time that has passed is what it asserts, so that a Working… that came too soon fails it.

const (
	sa       = v1alpha1.ProgressStepsAndActivity
	activity = v1alpha1.ProgressActivity
	step     = 500 * time.Millisecond
)

// progressRun starts a turn of the mode, on a chat thread for a recipient, on an alert thread for none.
func (e *env) progressRun(t *testing.T, mode v1alpha1.ProgressMode, recipient string) *live.Run {
	t.Helper()
	run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: mode, Recipient: recipient})
	return run
}

// until turns the clock in half seconds until cond holds, and returns how much time that took. It fails if limit passes
// first. A run that is slow to act on a timer makes the time longer, never shorter: a test that bounds it above leaves room.
func (e *env) until(t *testing.T, what string, limit time.Duration, cond func() bool) time.Duration {
	t.Helper()
	var spent time.Duration
	for !cond() {
		if spent >= limit {
			t.Fatalf("after %v: %s did not happen", spent, what)
		}
		e.clk.advance(step)
		spent += step
		time.Sleep(2 * time.Millisecond)
	}
	return spent
}

// elapse turns the clock d without waiting for anything, and gives the run a moment to act on a timer that fell due.
func (e *env) elapse(d time.Duration) {
	for ; d > 0; d -= step {
		e.clk.advance(min(step, d))
	}
	time.Sleep(30 * time.Millisecond)
}

// tookSince is the time on the fake clock since started, in whole seconds as a summary says it. The tests keep it under a
// minute.
func (e *env) tookSince(started time.Time) string {
	return fmt.Sprintf("%d s", int(e.clk.Now().Sub(started).Round(time.Second)/time.Second))
}

// writesReach waits for the renderer to have made n requests in all.
func (e *env) writesReach(t *testing.T, n int) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d requests", n), func() bool { return e.writes() >= n })
}

// cards is the task_update chunks the stream was sent, in order, as "id title status" and, if the chunk had one, " [detail]".
func cards(t *testing.T, m fakeslack.Message) []string {
	t.Helper()
	var out []string
	for _, raw := range m.Chunks {
		var c struct{ Type, ID, Title, Status, Details, Output string }
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		if c.Type != "task_update" {
			continue
		}
		line := c.ID + " " + c.Title + " " + c.Status
		if c.Details != "" {
			line += " [" + c.Details + "]"
		}
		if c.Output != "" {
			line += " {" + c.Output + "}"
		}
		out = append(out, line)
	}
	return out
}

func wantCards(t *testing.T, m fakeslack.Message, want ...string) {
	t.Helper()
	if got := cards(t, m); !slices.Equal(got, want) {
		t.Fatalf("cards:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A gap longer than the delay begins a Working… card with no detail; the next tool call takes that card over, so the call
// and the gap are one card, whose title and detail the call sends once; and its result finishes it with its status alone.
func TestASlowGapBeginsAWorkingCardThatTheNextCallClaims(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Checking the pods."))
	e.writesReach(t, 1) // the stream starts
	if spent := e.until(t, "the Working… card", 5*time.Second, func() bool { return len(e.tasks()) == 1 }); spent < 3*time.Second || spent > 4*time.Second {
		t.Fatalf("the card began after %v of nothing new; want the delay, 3 s", spent)
	}
	wantCards(t, e.reply(), "s1 Working… in_progress")
	run.Handle(call("c1", "kubectl_get", map[string]any{"ns": "prod"}))
	e.until(t, "the claim", 3*time.Second, func() bool { return e.tasks()[0].Title == "kubectl_get" })
	run.Handle(result("c1", map[string]any{"output": "3 pods"}))
	e.until(t, "the card done", 3*time.Second, func() bool { return e.tasks()[0].Status == "complete" })
	run.Handle(text("b", "Three pods."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := e.reply()
	wantCards(t, m,
		"s1 Working… in_progress",
		`s1 kubectl_get in_progress [ns="prod"]`,
		"s1 kubectl_get complete",
	)
	if m.Streaming || m.Text != "Checking the pods.\n\nThree pods." {
		t.Errorf("message %+v; want the text, and the stream stopped", m)
	}
	e.assertInert(t)
}

// A call that comes before the delay has passed begins its card directly, as it does without the activity mode, and no
// Working… card is begun for the gap before it.
func TestACallBeforeTheDelayBeginsItsCardDirectly(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Checking."))
	e.writesReach(t, 1)
	e.elapse(2500 * time.Millisecond)
	if n := e.writes(); n != 1 {
		t.Fatalf("%d requests after a gap shorter than the delay; want the stream's start alone", n)
	}
	run.Handle(call("c1", "kubectl_get", map[string]any{"ns": "prod"}))
	e.until(t, "the card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
	run.Handle(result("c1", map[string]any{"output": "ok"}))
	e.until(t, "the card done", 3*time.Second, func() bool { return e.tasks()[0].Status == "complete" })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	wantCards(t, e.reply(), `s1 kubectl_get in_progress [ns="prod"]`, "s1 kubectl_get complete")
	if strings.Contains(e.wire(), "Working") {
		t.Errorf("the wire has a Working… for a gap that was shorter than the delay:\n%s", e.wire())
	}
}

// A gap no tool call claims ends when the text comes: the card is retitled before it is finished, so that no card shows
// a tick on Working…, and it is finished before the text that ended it is sent.
func TestAnUnclaimedWorkingCardIsRetitledAndFinishedWhenTextComes(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Checking."))
	e.writesReach(t, 1)
	e.until(t, "the Working… card", 5*time.Second, func() bool { return len(e.tasks()) == 1 })
	run.Handle(text("b", "The answer."))
	e.until(t, "the card finished", 3*time.Second, func() bool { return e.tasks()[0].Status == "complete" })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := e.reply()
	wantCards(t, m, "s1 Working… in_progress", "s1 Wrote the answer complete")
	if got, want := timeline(m), []string{"text:Checking.", "task:s1:in_progress", "task:s1:complete", "text:\n\nThe answer."}; !slices.Equal(got, want) {
		t.Errorf("chunks %v; want the card finished before the text that ended it %v", got, want)
	}
}

// An unclaimed card is finished before the stop when the turn ends with the gap open and no more text.
func TestAnUnclaimedWorkingCardIsFinishedBeforeTheStop(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Done."))
	e.writesReach(t, 1)
	e.until(t, "the Working… card", 5*time.Second, func() bool { return len(e.tasks()) == 1 })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	wantCards(t, e.reply(), "s1 Working… in_progress", "s1 Wrote the answer complete")
	paths := e.rec.orderOf()
	if paths[len(paths)-1] != "/chat.stopStream" {
		t.Errorf("the last request is %s; want the stop, after the card is finished", paths[len(paths)-1])
	}
}

// A turn that ends with no text at all did not write an answer: its card is ended, as a call with no result is, and not
// retitled.
func TestAnUnclaimedWorkingCardOfATurnWithNoTextIsEndedNotRetitled(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	e.until(t, "the Working… card", 5*time.Second, func() bool { return e.writes() >= 2 && len(e.tasks()) == 1 })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	got := cards(t, e.reply())
	if len(got) != 2 || got[0] != "s1 Working… in_progress" || !strings.HasPrefix(got[1], "s1 Working… complete") {
		t.Errorf("cards %v; want the card ended under its own title", got)
	}
}

// The delay begins at most one Working… card for a gap, and no other write comes of the clock: a long silence costs one
// request.
func TestALongGapBeginsOneWorkingCardAndNoOtherWrite(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Checking."))
	e.writesReach(t, 1)
	e.until(t, "the Working… card", 5*time.Second, func() bool { return len(e.tasks()) == 1 })
	e.writesReach(t, 2)
	e.elapse(2 * time.Minute)
	if n := e.writes(); n != 2 {
		t.Fatalf("%d requests after two minutes of nothing; want the stream's start and the one card", n)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
}

// A tool call that arrives while a Working… card is under way is one card with it, and costs no step of the limit beyond
// the card: with the card claimed by the first call, the fiftieth call has its card and the fifty-first is not shown.
func TestAClaimedCardIsOneOfTheFiftyAndNotTwo(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Checking."))
	e.writesReach(t, 1)
	e.until(t, "the Working… card", 5*time.Second, func() bool { return len(e.tasks()) == 1 })
	for i := 1; i <= 51; i++ {
		run.Handle(call(fmt.Sprint("c", i), "step", nil))
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	tasks := e.reply().Tasks()
	if len(tasks) != 50 || tasks[0].ID != "s1" || tasks[0].Title != "step" || tasks[49].ID != "s50" {
		t.Fatalf("%d cards, first %+v; want fifty, the first of them the claimed card", len(tasks), tasks[0])
	}
}

// The activity mode is one card for the whole wait, which begins at the first tool event and counts in its title, which
// a platform replaces, and not in its detail, which it appends. The summary is sent once, with the finish. Nothing the
// requests carry names a tool or holds an argument.
func TestTheActivityCardCountsInItsTitleAndSummarisesOnce(t *testing.T) {
	e := setup(t)
	started := e.clk.Now()
	run := e.progressRun(t, activity, "U1")
	e.elapse(time.Second)
	run.Handle(call("c1", "TOOLNAME_ONE", map[string]any{"q": "ARGVALUE_ONE"}))
	e.until(t, "the card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
	run.Handle(result("c1", map[string]any{"output": "SECRETOUT"}))
	e.until(t, "the first step counted", 3*time.Second, func() bool { return e.tasks()[0].Title == "Working… 1 step" })
	run.Handle(call("c2", "TOOLNAME_TWO", map[string]any{"q": "ARGVALUE_TWO"}))
	run.Handle(result("c2", map[string]any{"error": "no such pod"}))
	e.until(t, "the second step counted", 3*time.Second, func() bool { return e.tasks()[0].Title == "Working… 2 steps, 1 failed" })
	e.elapse(4 * time.Second)
	took := e.tookSince(started)
	run.Handle(text("a", "The answer."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := e.reply()
	wantCards(t, m,
		"s1 Working… in_progress",
		"s1 Working… 1 step in_progress",
		"s1 Working… 2 steps, 1 failed in_progress",
		"s1 Worked complete [Took "+took+", 2 steps (1 failed)]",
	)
	for _, secret := range []string{"TOOLNAME", "ARGVALUE", "SECRETOUT", "no such pod"} {
		if strings.Contains(e.wire(), secret) {
			t.Errorf("a request carries %q in the activity mode:\n%s", secret, e.wire())
		}
	}
	if m.Streaming || m.Text != "The answer." {
		t.Errorf("message %+v; want the text, and the stream stopped", m)
	}
}

// With no tool events, as on the generic dialect, the activity card begins when the delay has passed, counts nothing, and is
// done with the time alone.
func TestTheActivityCardBeginsAtTheDelayWhenNothingElseHappens(t *testing.T) {
	e := setup(t)
	started := e.clk.Now()
	run := e.progressRun(t, activity, "U1")
	if spent := e.until(t, "the card", 5*time.Second, func() bool { return len(e.tasks()) == 1 }); spent < 3*time.Second || spent > 4*time.Second {
		t.Fatalf("the card began after %v; want the delay, 3 s", spent)
	}
	e.elapse(5 * time.Second)
	took := e.tookSince(started)
	run.Handle(text("a", "Hello."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	wantCards(t, e.reply(), "s1 Working… in_progress", "s1 Worked complete [Took "+took+"]")
}

// The activity mode is for the wait before the text: a turn whose first event is text shows no card.
func TestTheActivityCardIsNotBegunAfterText(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, activity, "U1")
	run.Handle(text("a", "Hello."))
	e.writesReach(t, 1)
	e.elapse(time.Minute)
	run.Handle(call("c1", "search", nil))
	run.Handle(result("c1", map[string]any{"output": "x"}))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if got := cards(t, e.reply()); len(got) != 0 {
		t.Errorf("cards %v; want none after text in the activity mode", got)
	}
}

// Output final shows nothing until the turn ends, whatever the progress mode is: no timer writes, and one post.
func TestOutputFinalIgnoresTheProgressMode(t *testing.T) {
	for _, mode := range []v1alpha1.ProgressMode{sa, activity} {
		for _, recipient := range []string{"", "U1"} {
			t.Run(string(mode)+"/"+recipient, func(t *testing.T) {
				e := setup(t)
				run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputFinal, Progress: mode, Recipient: recipient})
				e.elapse(30 * time.Second)
				run.Handle(call("c1", "search", nil))
				run.Handle(result("c1", map[string]any{"output": "x"}))
				e.elapse(30 * time.Second)
				if n := e.writes(); n != 0 {
					t.Fatalf("%d requests before the end of a final-mode turn; want none", n)
				}
				run.Handle(text("a", "Done."))
				run.Handle(agent.FinalEvent{})
				e.finish(t, run)
				if n := e.writes(); n != 1 || e.reply().Text != "Done." {
					t.Errorf("%d requests, reply %q; want the one post", n, e.reply().Text)
				}
			})
		}
	}
}

// The steps mode never shows a Working… indicator and never keeps a timer.
func TestTheStepsModeShowsNoIndicator(t *testing.T) {
	for _, recipient := range []string{"", "U1"} {
		t.Run("recipient "+recipient, func(t *testing.T) {
			e := setup(t)
			run := e.progressRun(t, v1alpha1.ProgressSteps, recipient)
			run.Handle(text("a", "Checking."))
			e.writesReach(t, 1)
			e.elapse(time.Minute)
			if e.clk.waiting() != 0 || e.writes() != 1 {
				t.Errorf("%d timers pending and %d requests after a minute; want the steps mode to keep no clock", e.clk.waiting(), e.writes())
			}
			run.Handle(agent.FinalEvent{})
			e.finish(t, run)
			if strings.Contains(e.wire(), "Working") {
				t.Errorf("the steps mode wrote a Working…:\n%s", e.wire())
			}
		})
	}
}

// A platform with no steps shows no card in any mode, and no stream is begun for a card.
func TestNoCardIsShownOnAPlatformWithoutSteps(t *testing.T) {
	for _, mode := range []v1alpha1.ProgressMode{sa, activity} {
		t.Run(string(mode), func(t *testing.T) {
			e := setup(t)
			steps := e.spy(t, false, nil)
			run := e.progressRun(t, mode, "U1")
			run.Handle(text("a", "Hello"))
			e.writesReach(t, 1)
			e.elapse(time.Minute)
			run.Handle(call("c1", "search", nil))
			run.Handle(result("c1", map[string]any{"output": "x"}))
			run.Handle(agent.FinalEvent{})
			e.finish(t, run)
			if n := steps.Load(); n != 0 || e.updates() != 0 || len(e.reply().Tasks()) != 0 || e.reply().Text != "Hello" {
				t.Errorf("%d steps asked, %d updates, tasks %+v, text %q; want the text alone", n, e.updates(), e.reply().Tasks(), e.reply().Text)
			}
		})
	}
}

// A platform that refuses the steps for good, or whose stream is in a mode that cannot take them (markdown), shows no card
// in any mode: the Working… card is the first step asked for, it ends the steps of the turn, and the turn goes on with its
// text. The status line of a message that is updated is not put on a stream instead, since text appended to a stream cannot
// be taken back.
func TestARefusedWorkingCardEndsTheStepsOfTheTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"refused", errRefused}, {"markdown mode", chat.ErrNoSteps}} {
		for _, mode := range []v1alpha1.ProgressMode{sa, activity} {
			t.Run(tc.name+"/"+string(mode), func(t *testing.T) {
				e := setup(t)
				steps := e.spy(t, true, func(int) error { return tc.err })
				run := e.progressRun(t, mode, "U1")
				if mode == activity { // its card is for the wait before the text: a tool event begins it
					run.Handle(call("c0", "first", nil))
				} else {
					run.Handle(text("a", "Hello"))
				}
				eventually(t, "the card to be asked of the platform", func() bool {
					e.clk.advance(step)
					return steps.Load() >= 1
				})
				run.Handle(call("c1", "search", nil))
				run.Handle(text("b", "More"))
				run.Handle(agent.FinalEvent{})
				e.finish(t, run)
				if n := steps.Load(); n != 1 {
					t.Errorf("%d steps asked; want the one that was refused, and no more", n)
				}
				if m := e.reply(); m.Streaming || !strings.HasSuffix(m.Text, "More") || len(m.Tasks()) != 0 || e.updates() != 0 {
					t.Errorf("message %+v, %d updates; want the text, streamed and stopped, with no card and no status line", m, e.updates())
				}
			})
		}
	}
}

// requestsOnTheUpdatePath is every message the renderer sent to an alert thread, the blocks of each in the order sent.
func (e *env) sentLines() string { return strings.Join(e.requests(), "\n") }

// On an alert thread the line of a call that returned is replaced by Working… once the delay has passed with nothing new,
// and text replaces that: the line is Working…, never a stale Ran….
func TestAnAlertThreadShowsWorkingInAGapAfterACall(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "")
	run.Handle(call("c1", "search", map[string]any{"q": "pods"}))
	e.writesReach(t, 1)
	resultAt := e.clk.Now()
	run.Handle(result("c1", map[string]any{"output": "x"}))
	e.until(t, "the line of the call that returned", 3*time.Second, func() bool { return strings.Contains(e.said(), "Ran search") })
	posted := len(e.requests())
	e.until(t, "Working…", 8*time.Second, func() bool { return strings.Contains(e.said(), "Working…") })
	if since := e.clk.Now().Sub(resultAt); since < 3*time.Second {
		t.Fatalf("Working… %v after the result; want no less than the delay, 3 s", since)
	}
	if got := e.said(); strings.Contains(got, "search") {
		t.Errorf("blocks %s; want Working… in place of the line of the call that returned", got)
	}
	if n := len(e.requests()); n != posted+1 {
		t.Errorf("%d requests, %d before the gap; want one for Working…", n, posted)
	}
	run.Handle(text("a", "Found three pods."))
	e.until(t, "the text", 3*time.Second, func() bool { return e.reply().Text == "Found three pods." })
	if got := e.said(); strings.Contains(got, "Working") {
		t.Errorf("blocks %s; want the text to have replaced Working…", got)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
}

// A turn that says nothing at all gets a post of Working… alone once the delay has passed; a turn that ends with nothing to
// show leaves no Working… standing.
func TestAnAlertThreadPostsWorkingWhenTheTurnIsSilent(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "")
	if spent := e.until(t, "the post", 6*time.Second, func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 }); spent < 3*time.Second {
		t.Fatalf("posted after %v; want the delay, 3 s", spent)
	}
	eventually(t, "the message", func() bool { return e.reply().Text == "Working…" })
	if m := e.reply(); !strings.Contains(string(m.Blocks), `"context"`) {
		t.Fatalf("message %+v; want Working… as a line of small print and as the plain text", m)
	}
	e.elapse(time.Minute)
	if n := len(e.requests()); n != 1 {
		t.Fatalf("%d requests after a minute; want no write from the clock but the one", n)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if m := e.reply(); m.Text != "(no output)" {
		t.Errorf("final message %+v; want no output, and no Working… left standing", m)
	}
}

// A call that comes before the delay has its own line, and no Working… is written for the gap before it.
func TestAnAlertThreadShowsNoWorkingForAShortGap(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "")
	run.Handle(text("a", "Looking."))
	e.writesReach(t, 1)
	e.elapse(2500 * time.Millisecond)
	run.Handle(call("c1", "search", nil))
	e.until(t, "the line of the call", 3*time.Second, func() bool { return strings.Contains(e.said(), "Calling search") })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if strings.Contains(e.sentLines(), "Working") {
		t.Errorf("a write has Working… for a gap shorter than the delay:\n%s", e.sentLines())
	}
}

// The activity mode on an alert thread is one line, Working… with the minutes and the steps, which is rewritten by the
// writes that events cause and by nothing else, and which names no tool and holds no argument.
func TestTheActivityLineOnAnAlertThreadCountsAndNamesNoTool(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, activity, "")
	run.Handle(call("c1", "TOOLNAME_ONE", map[string]any{"q": "ARGVALUE_ONE"}))
	eventually(t, "the line", func() bool { return strings.Contains(e.said(), "Working…") })
	if got := e.said(); strings.Contains(got, "Working… (") {
		t.Fatalf("blocks %s; want Working… alone while no result is in", got)
	}
	run.Handle(result("c1", map[string]any{"output": "SECRETOUT"}))
	e.until(t, "the first step counted", 3*time.Second, func() bool { return strings.Contains(e.said(), "Working… (1 step)") })
	posted := len(e.requests())
	e.elapse(2*time.Minute + 5*time.Second) // no event: nothing is written
	if n := len(e.requests()); n != posted {
		t.Fatalf("%d requests after two minutes with no event; want no write from the clock:\n%s", n, strings.Join(e.requests(), "\n"))
	}
	run.Handle(call("c2", "TOOLNAME_TWO", map[string]any{"q": "ARGVALUE_TWO"}))
	run.Handle(result("c2", map[string]any{"error": "no such pod"}))
	e.until(t, "the line with the minutes", 3*time.Second, func() bool { return strings.Contains(e.said(), "Working… (2 min, 2 steps, 1 failed)") })
	run.Handle(text("a", "The answer."))
	e.until(t, "the text", 3*time.Second, func() bool { return e.reply().Text == "The answer." })
	if got := e.said(); strings.Contains(got, "Working") {
		t.Errorf("blocks %s; want the text to have replaced the line", got)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	for _, secret := range []string{"TOOLNAME", "ARGVALUE", "SECRETOUT", "no such pod", "Calling", "Ran "} {
		if strings.Contains(e.sentLines(), secret) || strings.Contains(e.wire(), secret) {
			t.Errorf("a request carries %q in the activity mode:\n%s", secret, e.wire())
		}
	}
}

// The steps mode on an alert thread keeps the line of the call that returned for as long as nothing follows it.
func TestTheStepsModeLeavesTheLineOfAReturnedCallStanding(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, v1alpha1.ProgressSteps, "")
	run.Handle(call("c1", "search", nil))
	e.writesReach(t, 1)
	run.Handle(result("c1", map[string]any{"output": "x"}))
	e.until(t, "the line of the call that returned", 3*time.Second, func() bool { return strings.Contains(e.said(), "Ran search") })
	e.elapse(time.Minute)
	if got := e.said(); !strings.Contains(got, "Ran search") || strings.Contains(got, "Working") {
		t.Errorf("blocks %s; want the line of the returned call, and no Working…", got)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
}

// advanceTo turns the clock to t exactly, so that a summary of the time taken does not depend on how many half seconds a
// loop turned it. t is no earlier than the clock.
func (e *env) advanceTo(t time.Time) {
	if d := t.Sub(e.clk.Now()); d > 0 {
		e.clk.advance(d)
	}
}

// progressScripts are the turns of the progress modes whose wire is recorded in testdata/golden (UPDATE_GOLDEN=1 rewrites
// them). Each steps the fake clock only as far as the thing it waits for, and fixes the time of the text that ends it.
var progressScripts = []struct {
	name string
	run  func(t *testing.T, e *env)
}{
	{"progress-a-slow-gap-claimed-by-a-call", func(t *testing.T, e *env) {
		run := e.progressRun(t, sa, "U1")
		run.Handle(text("a", "Checking the pods."))
		e.writesReach(t, 1)
		e.until(t, "the Working… card", 5*time.Second, func() bool { return len(e.tasks()) == 1 })
		run.Handle(call("c1", "kubectl_get", map[string]any{"namespace": "prod", "password": "hunter2"}))
		e.until(t, "the claim", 3*time.Second, func() bool { return e.tasks()[0].Title == "kubectl_get" })
		run.Handle(result("c1", map[string]any{"output": "3 pods"}))
		e.until(t, "the card done", 3*time.Second, func() bool { return e.tasks()[0].Status == "complete" })
		run.Handle(text("b", "Three pods, all ready."))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"progress-a-fast-gap-begins-the-tool-card", func(t *testing.T, e *env) {
		run := e.progressRun(t, sa, "U1")
		run.Handle(text("a", "Checking the pods."))
		e.writesReach(t, 1)
		e.elapse(2 * time.Second)
		run.Handle(call("c1", "kubectl_get", map[string]any{"namespace": "prod"}))
		e.until(t, "the card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
		run.Handle(result("c1", map[string]any{"output": "3 pods"}))
		e.until(t, "the card done", 3*time.Second, func() bool { return e.tasks()[0].Status == "complete" })
		run.Handle(text("b", "Three pods, all ready."))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"progress-an-unclaimed-gap-wrote-the-answer", func(t *testing.T, e *env) {
		run := e.progressRun(t, sa, "U1")
		run.Handle(text("a", "Checking the pods."))
		e.writesReach(t, 1)
		e.until(t, "the Working… card", 5*time.Second, func() bool { return len(e.tasks()) == 1 })
		run.Handle(text("b", "Three pods, all ready."))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"progress-activity-in-a-chat-thread", func(t *testing.T, e *env) {
		started := e.clk.Now()
		run := e.progressRun(t, activity, "U1")
		e.elapse(time.Second)
		run.Handle(call("c1", "kubectl_get", map[string]any{"namespace": "prod", "password": "hunter2"}))
		e.until(t, "the card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
		run.Handle(result("c1", map[string]any{"output": "3 pods"}))
		e.until(t, "one step", 3*time.Second, func() bool { return e.tasks()[0].Title == "Working… 1 step" })
		run.Handle(call("c2", "fetch", nil))
		run.Handle(result("c2", map[string]any{"error": "call rejected"}))
		e.until(t, "two steps", 3*time.Second, func() bool { return e.tasks()[0].Title == "Working… 2 steps, 1 failed" })
		e.advanceTo(started.Add(20 * time.Second))
		run.Handle(text("a", "Three pods, all ready."))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"progress-working-in-an-alert-thread", func(t *testing.T, e *env) {
		run := e.progressRun(t, sa, "")
		run.Handle(call("c1", "kubectl_get", map[string]any{"namespace": "prod"}))
		eventually(t, "the line of the call", func() bool { return strings.Contains(e.said(), "Calling kubectl_get") })
		run.Handle(result("c1", map[string]any{"output": "3 pods"}))
		e.until(t, "the line of the call that returned", 3*time.Second, func() bool { return strings.Contains(e.said(), "Ran kubectl_get") })
		e.until(t, "Working…", 8*time.Second, func() bool { return strings.Contains(e.said(), "Working…") })
		run.Handle(text("a", "Three pods, all ready."))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"progress-activity-in-an-alert-thread", func(t *testing.T, e *env) {
		started := e.clk.Now()
		run := e.progressRun(t, activity, "")
		run.Handle(call("c1", "kubectl_get", map[string]any{"namespace": "prod"}))
		eventually(t, "the line", func() bool { return strings.Contains(e.said(), "Working…") })
		run.Handle(result("c1", map[string]any{"output": "3 pods"}))
		e.until(t, "one step", 3*time.Second, func() bool { return strings.Contains(e.said(), "Working… (1 step)") })
		e.advanceTo(started.Add(2*time.Minute + 5*time.Second))
		run.Handle(call("c2", "fetch", nil))
		run.Handle(result("c2", map[string]any{"error": "call rejected"}))
		e.until(t, "the minutes", 3*time.Second, func() bool { return strings.Contains(e.said(), "(2 min, 2 steps, 1 failed)") })
		run.Handle(text("a", "Three pods, all ready."))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
}

func TestTheProgressModesWire(t *testing.T) {
	for _, s := range progressScripts {
		t.Run(s.name, func(t *testing.T) {
			e := setup(t)
			s.run(t, e)
			got := e.record(t)
			checkStepFieldsOnce(t, got)
			checkGolden(t, s.name, got)
		})
	}
}

// checkStepFieldsOnce fails if any step ID has its detail in more than one task_update chunk: Slack appends a card's detail
// (observed 2026-10-18), so a step that sent it twice shows it twice. The steps mode is exempt, as it was: its golden is the
// wire as it was before the progress modes.
func checkStepFieldsOnce(t *testing.T, g golden) {
	t.Helper()
	details := map[string]int{}
	for _, r := range g.Requests {
		chunks, _ := r.Form["chunks"].([]any)
		for _, raw := range chunks {
			c, _ := raw.(map[string]any)
			if c["type"] != "task_update" {
				continue
			}
			id, _ := c["id"].(string)
			if d, _ := c["details"].(string); d != "" {
				details[id]++
			}
		}
	}
	for id, n := range details {
		if n > 1 {
			t.Errorf("step %s sent its detail in %d chunks; want one, since Slack appends it", id, n)
		}
	}
}

// The sink passes the Binding's progress mode to the renderer, and a Binding that has none is the API's default,
// steps-and-activity: the turn shows Working… once the delay has passed. An explicit steps shows nothing, and output final
// ignores the mode.
func TestTheSinkTellsTheRendererTheProgressMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		progress    v1alpha1.ProgressMode
		output      v1alpha1.OutputMode
		wantWorking bool
	}{
		{"unset is the default", "", v1alpha1.OutputLive, true},
		{"steps-and-activity", sa, v1alpha1.OutputLive, true},
		{"activity", activity, v1alpha1.OutputLive, true},
		{"steps", v1alpha1.ProgressSteps, v1alpha1.OutputLive, false},
		{"final output", activity, v1alpha1.OutputFinal, false},
		{"unset, final output", "", v1alpha1.OutputFinal, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			key := thread
			key.Thread = e.root
			h := e.sink(t, e.ob).Turn(t.Context(), session.TurnRef{Key: key, MessageID: "turn-1", UserID: session.SystemUser, Output: tc.output, Progress: tc.progress})
			if tc.wantWorking {
				e.until(t, "Working…", 10*time.Second, func() bool { return e.writes() == 1 })
			} else {
				e.elapse(10 * time.Second)
			}
			eventually(t, "the message", func() bool { return !tc.wantWorking || e.reply().Text == "Working…" })
			if got := strings.Contains(e.said(), "Working…"); got != tc.wantWorking || (!tc.wantWorking && e.writes() != 0) {
				t.Errorf("Working… shown = %v after silence; want %v (requests %d)", got, tc.wantWorking, e.writes())
			}
			h(agent.FinalEvent{})
			e.drain(t, e.live)
		})
	}
}

// The delay is the renderer's setting: a Config that names one is held to it.
func TestTheDelayIsTheConfigs(t *testing.T) {
	e := setup(t)
	e.cfg.ActivityDelay = 5 * time.Second
	e.live = newRenderer(t, e.cfg)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Checking."))
	e.writesReach(t, 1)
	spent := e.until(t, "the Working… card", 8*time.Second, func() bool { return len(e.tasks()) == 1 })
	if spent < 5*time.Second || spent > 6*time.Second {
		t.Errorf("the card began after %v; want the configured 5 s", spent)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
}

// A call that is under way is no gap: a tool that takes longer than the delay has its own card, which shows the time passing,
// and no Working… card is begun beside it.
func TestACallUnderWayIsNoGap(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Checking."))
	e.writesReach(t, 1)
	run.Handle(call("c1", "slow_query", nil))
	e.until(t, "the card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
	e.elapse(time.Minute)
	if n := e.writes(); n != 2 {
		t.Fatalf("%d requests while one call was under way for a minute; want the start and the card", n)
	}
	run.Handle(result("c1", map[string]any{"output": "x"}))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	wantCards(t, e.reply(), "s1 slow_query in_progress", "s1 slow_query complete")
}

// A call and its result that both arrive before the card that waits is sent are sent as one chunk: the title, the detail
// and the outcome.
func TestAClaimedCardWhoseCallHasReturnedIsSentOnceInItsOutcome(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Checking."))
	e.writesReach(t, 1)
	e.until(t, "the Working… card", 5*time.Second, func() bool { return len(e.tasks()) == 1 })
	e.writesReach(t, 2)
	// The run is idle; the next two events reach it together.
	run.Handle(call("c1", "kubectl_get", map[string]any{"ns": "prod"}))
	run.Handle(result("c1", map[string]any{"error": "no such pod"}))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	got := cards(t, e.reply())
	if want := []string{"s1 Working… in_progress", `s1 kubectl_get error [ns="prod"]`}; !slices.Equal(got, want) {
		t.Fatalf("cards %v; want the Working… card, then the card of the call in its outcome, with its detail %v", got, want)
	}
}

// A gap after text shows Working… under the text on an alert thread, as a call under way shows its line beside it, and the
// next text takes it away.
func TestAnAlertThreadShowsWorkingUnderTheTextInAGapAfterIt(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "")
	run.Handle(text("a", "Checking the pods."))
	e.writesReach(t, 1)
	e.until(t, "Working…", 8*time.Second, func() bool { return strings.Contains(e.said(), "Working…") })
	if m := e.reply(); m.Text != "Checking the pods." || !strings.Contains(string(m.Blocks), "Checking the pods.") {
		t.Errorf("message %+v; want the text kept, with Working… under it", m)
	}
	run.Handle(text("a", "Checking the pods. Three, all ready."))
	e.until(t, "the longer text", 3*time.Second, func() bool { return e.reply().Text == "Checking the pods. Three, all ready." })
	if got := e.said(); strings.Contains(got, "Working") {
		t.Errorf("blocks %s; want the text to have taken Working… away", got)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
}
