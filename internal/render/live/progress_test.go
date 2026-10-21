package live_test

import (
	"encoding/json"
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
	step     = 250 * time.Millisecond
)

// progressRun starts a turn of the mode, on a chat thread for a recipient, on an alert thread for none.
func (e *env) progressRun(t *testing.T, mode v1alpha1.ProgressMode, recipient string) *live.Run {
	t.Helper()
	run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: mode, Recipient: recipient})
	return run
}

// until turns the clock in quarter seconds, a moment apart, until cond holds, and returns how much time that took. A run that is
// slow to act on a timer that fell due arms the next one late, which makes the time longer and never shorter, so a test that
// asserts a lower bound on it is exact. limit is how long the thing should take: once limit and some lag have passed, the clock
// stops, and the run is given real time to act on the timers that are due, until the test's own deadline.
func (e *env) until(t *testing.T, what string, limit time.Duration, cond func() bool) time.Duration {
	t.Helper()
	const lag = 5 * time.Second
	var spent time.Duration
	for ; !cond() && spent < limit+lag; spent += step {
		e.clk.advance(step)
		time.Sleep(10 * time.Millisecond)
	}
	eventually(t, what, func() bool { e.clk.advance(0); return cond() }) // fires what is due, and moves nothing
	return spent
}

// elapse turns the clock d without waiting for anything, and gives the run a moment to act on a timer that fell due.
func (e *env) elapse(d time.Duration) {
	for ; d > 0; d -= time.Second {
		e.clk.advance(min(time.Second, d))
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
	if spent := e.until(t, "the Working… card", 3*time.Second, func() bool { return len(e.tasks()) == 1 }); spent < 3*time.Second {
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

// An unclaimed card is finished before the stop when the turn ends with the gap open and no more text. No text came while it
// was open, so it does not say that it wrote the answer.
func TestAnUnclaimedWorkingCardIsFinishedBeforeTheStop(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Done."))
	e.writesReach(t, 1)
	e.until(t, "the Working… card", 5*time.Second, func() bool { return len(e.tasks()) == 1 })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	wantCards(t, e.reply(), "s1 Working… in_progress", "s1 Finished complete")
	paths := e.rec.orderOf()
	if paths[len(paths)-1] != "/chat.stopStream" {
		t.Errorf("the last request is %s; want the stop, after the card is finished", paths[len(paths)-1])
	}
}

// How the task ended is how a card that is under way ends: a tick for a completed task and for a pause for input, an error for
// a task that failed or was cancelled, in both modes that show a card. The task did not finish, so nothing in its stream
// ticks.
func TestACardUnderWayEndsByHowTheTaskEnded(t *testing.T) {
	for _, mode := range []v1alpha1.ProgressMode{sa, activity} {
		for _, tc := range []struct {
			name   string
			final  agent.FinalEvent
			status string
			title  string
		}{
			{"completed", agent.FinalEvent{}, "complete", "Finished"},
			{"failed", agent.FinalEvent{Task: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}}, "error", "Stopped"},
			{"canceled", agent.FinalEvent{Task: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}}, "error", "Stopped"},
			{"input required", agent.FinalEvent{Task: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}}, "complete", "Waiting for your answer"},
		} {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				e := setup(t)
				run := e.progressRun(t, mode, "U1")
				if mode == activity {
					if tc.title == "Finished" {
						tc.title = "Worked"
					}
					run.Handle(call("c1", "search", nil))
					run.Handle(result("c1", map[string]any{"output": "x"}))
				}
				e.until(t, "the card", 5*time.Second, func() bool { return len(e.tasks()) == 1 })
				run.Handle(tc.final)
				e.finish(t, run)
				got := e.reply().Tasks()
				if len(got) != 1 || got[0].Status != tc.status || got[0].Title != tc.title {
					t.Errorf("tasks %+v; want the card %q as %s", got, tc.title, tc.status)
				}
			})
		}
	}
}

// A silent turn whose card was never shown and that ends empty makes no stream for the card: it ends as a turn with nothing to
// show did before the card, with no request.
func TestACardThatWasNeverShownMakesNoStreamAtTheEnd(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(agent.FinalEvent{Task: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}}) // before the delay: no card yet
	e.finish(t, run)
	if n := e.writes(); n != 0 {
		t.Errorf("%d requests for a turn that failed with nothing to show; want none", n)
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
	// The gap is used: a run that kept it open would arm the delay again at once, and again after each timer, without end.
	if n := e.clk.waiting(); n != 0 {
		t.Fatalf("%d timers pending after the card; want the gap used, and the run waiting for an event", n)
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
	run.Handle(result("c2", map[string]any{"error": "NOSUCHPOD"}))
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
	for _, secret := range []string{"TOOLNAME", "ARGVALUE", "SECRETOUT", "NOSUCHPOD"} {
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
	if spent := e.until(t, "the card", 3*time.Second, func() bool { return len(e.tasks()) == 1 }); spent < 3*time.Second {
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
	run.Handle(result("c2", map[string]any{"error": "NOSUCHPOD"}))
	e.until(t, "the line with the minutes", 3*time.Second, func() bool { return strings.Contains(e.said(), "Working… (2 min, 2 steps, 1 failed)") })
	run.Handle(text("a", "The answer."))
	e.until(t, "the text", 3*time.Second, func() bool { return e.reply().Text == "The answer." })
	if got := e.said(); strings.Contains(got, "Working") {
		t.Errorf("blocks %s; want the text to have replaced the line", got)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	for _, secret := range []string{"TOOLNAME", "ARGVALUE", "SECRETOUT", "NOSUCHPOD", "Calling", "Ran "} {
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
		// The order is what makes the wire and the summary's time the same every time. The last events come first, and the run
		// waits for its window: it has read the clock for it only once its timer is pending. The clock is then set to the time
		// the summary should give, which fires that timer, so the second step is written at once. The text and the final come
		// after that write, to a run that waits for its next window, and reach it together; set the clock after them, or before
		// the timer is pending, and the text is written as an append of its own, or the second step never is.
		run.Handle(call("c2", "fetch", nil))
		run.Handle(result("c2", map[string]any{"error": "call rejected"}))
		eventually(t, "the run to wait for its window", func() bool { return e.clk.waiting() == 1 })
		e.advanceTo(started.Add(20 * time.Second))
		eventually(t, "two steps", func() bool { return e.tasks()[0].Title == "Working… 2 steps, 1 failed" })
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
		// The second call and its result are one write only if they reach the run while its window is shut. The write of the
		// first step shut it, and the clock stands still until both are in, so the run cannot write between them, however it is
		// scheduled. Set the clock first, and the window is open when the call comes: the run may write the call alone, as
		// (2 min, 1 step). The clock is then set to the time the line should give, which opens the window, and the run writes
		// both with the minutes of that moment.
		run.Handle(call("c2", "fetch", nil))
		run.Handle(result("c2", map[string]any{"error": "call rejected"}))
		e.advanceTo(started.Add(2*time.Minute + 5*time.Second))
		eventually(t, "the minutes", func() bool { return strings.Contains(e.said(), "(2 min, 2 steps, 1 failed)") })
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

// The sink passes the Binding's progress mode to the renderer: the turn shows Working… once the delay has passed in the modes
// that show activity, and not in steps, which is also what a turn with no mode is (the API fills a Binding's, so a turn
// without one is not a Binding's). Output final ignores the mode.
func TestTheSinkTellsTheRendererTheProgressMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		progress    v1alpha1.ProgressMode
		output      v1alpha1.OutputMode
		wantWorking bool
	}{
		{"unset is steps", "", v1alpha1.OutputLive, false},
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
	spent := e.until(t, "the Working… card", 5*time.Second, func() bool { return len(e.tasks()) == 1 })
	if spent < 5*time.Second {
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

// A call with no ID can never have a result, so it is under way until the next event: no Working… card begins beside its
// own card, and Working… does not replace its line on an alert thread.
func TestACallWithNoIDHasNoWorkingBesideIt(t *testing.T) {
	t.Run("chat thread", func(t *testing.T) {
		e := setup(t)
		run := e.progressRun(t, sa, "U1")
		run.Handle(text("a", "Checking."))
		e.writesReach(t, 1)
		run.Handle(call("", "search", nil))
		e.until(t, "the card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
		e.elapse(time.Minute)
		if n := e.writes(); n != 2 {
			t.Fatalf("%d requests a minute after a call with no ID; want the stream's start and its card", n)
		}
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
		wantCards(t, e.reply(), "s1 search in_progress", "s1 search complete {No result was reported.}")
	})
	t.Run("alert thread", func(t *testing.T) {
		e := setup(t)
		run := e.progressRun(t, sa, "")
		run.Handle(call("", "search", nil))
		eventually(t, "the line", func() bool { return strings.Contains(e.said(), "Calling search") })
		e.elapse(time.Minute)
		if got := e.said(); !strings.Contains(got, "Calling search") || strings.Contains(got, "Working") {
			t.Errorf("blocks %s; want the line of the call, which has no result to wait for, and no Working…", got)
		}
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	})
	t.Run("activity counts it", func(t *testing.T) {
		e := setup(t)
		run := e.progressRun(t, activity, "U1")
		run.Handle(call("", "search", nil))
		e.until(t, "the call counted", 3*time.Second, func() bool { return len(e.tasks()) == 1 && e.tasks()[0].Title == "Working… 1 step" })
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	})
}

// A call reported again after its result leaves Working… where it is on an alert thread, and does not bring back the line of
// the call.
func TestACallReportedAgainDoesNotBringBackItsLine(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "")
	run.Handle(call("c1", "search", nil))
	e.writesReach(t, 1)
	run.Handle(result("c1", map[string]any{"output": "x"}))
	e.until(t, "Working…", 8*time.Second, func() bool { return strings.Contains(e.said(), "Working…") })
	run.Handle(call("c1", "search", nil))
	e.elapse(2 * time.Second)
	if got := e.said(); !strings.Contains(got, "Working…") || strings.Contains(got, "search") {
		t.Errorf("blocks %s; want Working… still, and no line of the call that was reported again", got)
	}
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
}

// The modes go on after a stream turn is taken over by the update path (a recovered turn that finds a message that was never a
// stream): the message shows Working… in a gap, and the activity mode its line.
func TestTheProgressModesGoOnAfterTheTurnMovesToTheUpdatePath(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode v1alpha1.ProgressMode
		want string
	}{{"steps-and-activity", sa, "Working…"}, {"activity", activity, "Working… (1 step)"}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.postedByUpdate(t)
			again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true, Progress: tc.mode})
			if tc.mode == activity {
				again.Handle(call("c1", "search", nil))
				again.Handle(result("c1", map[string]any{"output": "x"}))
			} else {
				again.Handle(text("a", "partial"))
			}
			e.until(t, tc.want, 8*time.Second, func() bool { return strings.Contains(e.said(), tc.want) })
			if len(e.rec.sent("/chat.startStream")) != 0 {
				t.Error("the taken-over turn started a stream")
			}
			again.Handle(text("a", "the whole answer"))
			again.Handle(agent.FinalEvent{})
			e.finish(t, again)
			if m := e.reply(); m.Text != "the whole answer" || strings.Contains(string(m.Blocks), "Working") {
				t.Errorf("final message %+v; want the answer, with no indicator left", m)
			}
		})
	}
}

// A turn that moves from the stream to the update path and ends with nothing to show follows the rule of a turn that never
// streamed: where a message stands it is updated to "(no output)", and where there is none nothing is posted. It is never a
// write of a message with no parts, which would fail for good.
func TestAMovedTurnThatEndsWithNothingToShowDoesNotWriteAnEmptyMessage(t *testing.T) {
	// The first write is made with all the events: the call, its result and the final.
	events := func(run *live.Run) {
		run.Handle(call("c1", "search", nil))
		run.Handle(result("c1", map[string]any{"output": "x"}))
		run.Handle(agent.FinalEvent{})
	}
	t.Run("the stream is refused: no message stands, and none is posted", func(t *testing.T) {
		e := setup(t)
		e.cfg.Platform = spy{Platform: e.cfg.Platform, caps: e.cfg.Platform.Capabilities(), steps: new(atomic.Int32), streamFail: fmt.Errorf("start: %w", errRefused)}
		e.live = newRenderer(t, e.cfg)
		release := e.hold()
		t.Cleanup(release)
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
		events(run)
		release()
		e.finish(t, run)
		if n := e.writes(); n != 0 || len(e.fake.Thread("C1", e.root)) != 1 {
			t.Errorf("%d requests, thread %+v; want nothing posted for a turn that showed nothing", n, e.fake.Thread("C1", e.root))
		}
		if l := e.logs.String(); strings.Contains(l, "cannot be sent") || strings.Contains(l, "no text and no parts") {
			t.Errorf("a write of an empty message was tried:\n%s", l)
		}
		// The turn ends as having nothing to show, not as a write that failed and was retried until the run gave up.
		if l := e.logs.String(); strings.Contains(l, "writing the message failed") || strings.Contains(l, "giving up") {
			t.Errorf("the end of a turn with nothing to show was retried as a failed write:\n%s", l)
		}
	})
	t.Run("a message stands: it says there was no output", func(t *testing.T) {
		e := setup(t)
		e.postedByUpdate(t)
		release := e.hold()
		t.Cleanup(release)
		again, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1", Recovered: true})
		events(again)
		release()
		e.finish(t, again)
		if m := e.reply(); m.Text != "(no output)" {
			t.Errorf("message %+v; want it updated to no output", m)
		}
		if l := e.logs.String(); strings.Contains(l, "cannot be sent") || strings.Contains(l, "no text and no parts") {
			t.Errorf("a write of an empty message was tried:\n%s", l)
		}
	})
}

// A stream that was started for a step and whose steps the platform then refuses (Slack in markdown mode takes no task_update
// chunk) is an empty message. A turn that ends with nothing to show must stop it, with the text of a turn that has no output, or
// it is left open for good. It is the same in the steps mode: a tool call, and then an empty end.
func TestAnEmptyStreamIsStoppedWhenTheTurnEndsWithNothingToShow(t *testing.T) {
	failed := agent.FinalEvent{Task: &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}}
	for _, tc := range []struct {
		name  string
		mode  v1alpha1.ProgressMode
		drive func(t *testing.T, e *env, run *live.Run)
		final agent.FinalEvent
	}{
		{"steps-and-activity, silent, failed", sa, func(t *testing.T, e *env, _ *live.Run) {
			e.until(t, "the stream to start for the card", 3*time.Second, func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
		}, failed},
		{"activity, silent, failed", activity, func(t *testing.T, e *env, _ *live.Run) {
			e.until(t, "the stream to start for the card", 3*time.Second, func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
		}, failed},
		{"steps, a call and an empty end", v1alpha1.ProgressSteps, func(t *testing.T, e *env, run *live.Run) {
			run.Handle(call("c1", "search", nil))
			run.Handle(result("c1", map[string]any{"output": "x"}))
			eventually(t, "the stream to start for the step", func() bool { return len(e.rec.sent("/chat.startStream")) == 1 })
		}, agent.FinalEvent{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.spy(t, true, func(int) error { return chat.ErrNoSteps })
			// The turn has a footer: a turn that ended with nothing to show is not one to rate (Turn.Footer), so the stop
			// that ends its empty stream carries no buttons, as the update path's "(no output)" carries none.
			run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: tc.mode, Recipient: "U1", Footer: footer})
			tc.drive(t, e, run)
			run.Handle(tc.final)
			e.finish(t, run)
			if n := len(e.rec.sent("/chat.stopStream")); n != 1 {
				t.Errorf("%d stops; want the empty stream stopped, once", n)
			}
			m := e.reply()
			if m.Streaming || m.Text != "(no output)" {
				t.Errorf("message %+v; want it stopped, saying there was no output", m)
			}
			if buttons, questions := hasFooter(string(m.Blocks)); buttons || questions > 0 {
				t.Errorf("the stopped empty stream asks for a rating: %s", m.Blocks)
			}
		})
	}
}
