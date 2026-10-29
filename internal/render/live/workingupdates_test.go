package live_test

import (
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
)

// These tests are about the working updates of an agent (agent.ProgressEvent, spec D45 (1)): activity whether or not anything
// in them is understood, and the agent's own progress line when they hold text.

func update(s string) agent.ProgressEvent { return agent.ProgressEvent{Text: s} }

// In steps-and-activity, a working update with text stands for Working… until the next event: the card the delay begins is
// titled with it, an update with no text puts Working… back, and the next tool call takes the card over.
func TestAProgressLineTitlesTheWorkingCard(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Checking."))
	e.writesReach(t, 1)
	run.Handle(update("Reading the pod logs"))
	if spent := e.until(t, "the card", 3*time.Second, func() bool { return len(e.tasks()) == 1 }); spent < 3*time.Second {
		t.Fatalf("the card began after %v; want the delay, 3 s: a line begins no card of its own", spent)
	}
	run.Handle(update(""))
	e.until(t, "Working… again", 3*time.Second, func() bool { return e.tasks()[0].Title == "Working…" })
	run.Handle(update("Reading the events"))
	if spent := e.until(t, "the retitle", 3*time.Second, func() bool { return e.tasks()[0].Title == "Reading the events" }); spent >= 3*time.Second {
		t.Fatalf("the open card was retitled after %v; want it at the next write", spent)
	}
	run.Handle(call("c1", "kubectl_get", map[string]any{"ns": "prod"}))
	e.until(t, "the claim", 3*time.Second, func() bool { return e.tasks()[0].Title == "kubectl_get" })
	run.Handle(result("c1", map[string]any{"output": "3 pods"}))
	run.Handle(text("b", "Three pods."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	m := e.reply()
	wantCards(t, m,
		"s1 Reading the pod logs in_progress",
		"s1 Working… in_progress",
		"s1 Reading the events in_progress",
		`s1 kubectl_get in_progress [ns="prod"]`,
		"s1 kubectl_get complete",
	)
	if m.Text != "Checking.\n\nThree pods." {
		t.Errorf("text %q; want the answer alone, never the progress line", m.Text)
	}
	e.assertInert(t)
}

// kagent 0.9.x repeats its answer in a working update just before the artifact that holds it. A line that text follows within
// the delay makes no card, so such a turn shows only its tool steps and its text.
func TestAProgressLineThatTextFollowsMakesNoCard(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(call("c1", "k8s_get_resources", nil))
	e.until(t, "the card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
	run.Handle(result("c1", map[string]any{"output": "46"}))
	run.Handle(update("The cluster has 46 namespaces."))
	e.elapse(time.Second)
	run.Handle(text("a", "The cluster has 46 namespaces."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if got := cards(t, e.reply()); len(got) != 2 || !strings.HasPrefix(got[0], "s1 k8s_get_resources") || !strings.HasPrefix(got[1], "s1 k8s_get_resources") {
		t.Errorf("cards:\n%s\nwant the tool's card alone", strings.Join(got, "\n"))
	}
}

// Text that comes while the progress line titles the card ends the card as any Working… card that text ends. The card is the
// delay's.
func TestTextEndsACardTheProgressLineTitled(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Checking."))
	e.writesReach(t, 1)
	run.Handle(update("Reading the pod logs"))
	e.until(t, "the card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
	run.Handle(text("b", "All ready."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	wantCards(t, e.reply(), "s1 Reading the pod logs in_progress", "s1 Wrote the answer complete")
}

// A working update with no text shows nothing in steps-and-activity and leaves the gap running: an agent that sends empty
// updates more often than the delay still shows Working… once the delay has passed since the last thing shown.
func TestEmptyUpdatesDoNotHoldWorkingOff(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(text("a", "Checking."))
	e.writesReach(t, 1)
	for range 2 {
		e.elapse(time.Second)
		run.Handle(update(""))
	}
	if n := len(e.tasks()); n != 0 {
		t.Fatalf("%d cards after 2 s of empty updates; want none before the delay", n)
	}
	e.until(t, "the Working… card", 2*time.Second, func() bool { return len(e.tasks()) == 1 })
	run.Handle(text("b", "Done."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	wantCards(t, e.reply(), "s1 Working… in_progress", "s1 Wrote the answer complete")
}

// A tool call under way is its own sign of work: an update's line is not shown beside it.
func TestAProgressLineIsNotShownWhileACallIsUnderWay(t *testing.T) {
	for _, recipient := range []string{"", "U1"} {
		t.Run("recipient "+recipient, func(t *testing.T) {
			e := setup(t)
			run := e.progressRun(t, sa, recipient)
			run.Handle(call("c1", "search", nil))
			e.writesReach(t, 1)
			run.Handle(update("NOTELINE"))
			e.elapse(time.Second)
			run.Handle(result("c1", map[string]any{"output": "x"}))
			run.Handle(text("a", "Found it."))
			run.Handle(agent.FinalEvent{})
			e.finish(t, run)
			if strings.Contains(e.wire(), "NOTELINE") {
				t.Errorf("a request carries the line given while a call was under way:\n%s", e.wire())
			}
		})
	}
}

// On an alert thread the progress line stands for the Working… line, at once, redacted, and the text replaces it. It is never
// part of the answer.
func TestAnAlertThreadShowsTheProgressLine(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "")
	run.Handle(update("Logging in with\npassword=hunter2"))
	if spent := e.until(t, "the line", 3*time.Second, func() bool { return e.reply().Text == "Logging in with password=[redacted]" }); spent >= 3*time.Second {
		t.Fatalf("the line was posted after %v; want it at once", spent)
	}
	if got := e.said(); !strings.Contains(got, `"context"`) {
		t.Fatalf("blocks %s; want the line as small print", got)
	}
	run.Handle(update(""))
	e.until(t, "Working… again", 3*time.Second, func() bool { return strings.Contains(e.said(), "Working…") })
	run.Handle(text("a", "The answer."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if m := e.reply(); m.Text != "The answer." || strings.Contains(m.Text, "Logging") {
		t.Errorf("final message %+v; want the answer alone", m)
	}
	if strings.Contains(e.wire(), "hunter2") {
		t.Errorf("a request carries the secret of a progress line:\n%s", e.wire())
	}
}

// The activity mode counts working updates while the turn has had no tool event, begins its card at the first, and shows none
// of their text.
func TestTheActivityCardCountsWorkingUpdates(t *testing.T) {
	e := setup(t)
	started := e.clk.Now()
	run := e.progressRun(t, activity, "U1")
	run.Handle(update("SECRETWORDS"))
	if spent := e.until(t, "the card", 3*time.Second, func() bool { return len(e.tasks()) == 1 }); spent >= 3*time.Second {
		t.Fatalf("the card began after %v; want it at the first update, as at a tool event", spent)
	}
	e.until(t, "the first update counted", 3*time.Second, func() bool { return e.tasks()[0].Title == "Working… 1 update" })
	run.Handle(update(""))
	e.until(t, "the second", 3*time.Second, func() bool { return e.tasks()[0].Title == "Working… 2 updates" })
	e.elapse(4 * time.Second)
	took := e.tookSince(started)
	run.Handle(text("a", "The answer."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	wantCards(t, e.reply(),
		"s1 Working… 1 update in_progress",
		"s1 Working… 2 updates in_progress",
		"s1 Worked complete [Took "+took+", 2 updates]",
	)
	if strings.Contains(e.wire(), "SECRETWORDS") {
		t.Errorf("a request carries an update's text in the activity mode:\n%s", e.wire())
	}
}

// Once a tool event has come the activity mode counts steps, and the updates no longer show.
func TestTheActivityCardCountsStepsOnceThereAreTools(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, activity, "U1")
	run.Handle(update(""))
	e.until(t, "the update counted", 3*time.Second, func() bool { return len(e.tasks()) == 1 && e.tasks()[0].Title == "Working… 1 update" })
	run.Handle(call("c1", "search", nil))
	run.Handle(result("c1", map[string]any{"output": "x"}))
	run.Handle(update(""))
	e.until(t, "the step counted", 3*time.Second, func() bool { return e.tasks()[0].Title == "Working… 1 step" })
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if got := cards(t, e.reply()); !strings.Contains(got[len(got)-1], ", 1 step]") || strings.Contains(got[len(got)-1], "update") {
		t.Errorf("last card %q; want the summary to count the step alone", got[len(got)-1])
	}
}

// On an alert thread the activity line counts the updates, and names nothing they said.
func TestTheActivityLineCountsWorkingUpdates(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, activity, "")
	run.Handle(update("SECRETWORDS"))
	e.until(t, "the line", 3*time.Second, func() bool { return strings.Contains(e.said(), "Working… (1 update)") })
	run.Handle(update(""))
	run.Handle(update(""))
	e.until(t, "the count", 3*time.Second, func() bool { return strings.Contains(e.said(), "Working… (3 updates)") })
	run.Handle(text("a", "The answer."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if strings.Contains(e.wire(), "SECRETWORDS") {
		t.Errorf("a request carries an update's text in the activity mode:\n%s", e.wire())
	}
}

// The steps mode shows nothing of working updates.
func TestTheStepsModeShowsNoWorkingUpdate(t *testing.T) {
	for _, recipient := range []string{"", "U1"} {
		t.Run("recipient "+recipient, func(t *testing.T) {
			e := setup(t)
			run := e.progressRun(t, v1alpha1.ProgressSteps, recipient)
			run.Handle(text("a", "Checking."))
			e.writesReach(t, 1)
			run.Handle(update("NOTELINE"))
			run.Handle(update(""))
			e.elapse(time.Minute)
			if e.writes() != 1 {
				t.Errorf("%d requests; want the updates to write nothing", e.writes())
			}
			run.Handle(agent.FinalEvent{})
			e.finish(t, run)
			if strings.Contains(e.wire(), "NOTELINE") || strings.Contains(e.wire(), "Working") {
				t.Errorf("the steps mode showed an update:\n%s", e.wire())
			}
		})
	}
}

// A call with no ID is under way until the next event. A progress line is that event: the gap that the call held off begins,
// and the card the delay begins carries the line.
func TestAProgressLineEndsACallWithNoID(t *testing.T) {
	e := setup(t)
	run := e.progressRun(t, sa, "U1")
	run.Handle(call("", "search", nil))
	e.until(t, "the call's card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
	run.Handle(update("Reading the results"))
	if spent := e.until(t, "the Working… card", 3*time.Second, func() bool { return len(e.tasks()) == 2 }); spent < 3*time.Second {
		t.Fatalf("the card began after %v; want the delay, 3 s", spent)
	}
	run.Handle(text("a", "Found it."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if got := cards(t, e.reply()); !strings.Contains(strings.Join(got, "\n"), "s2 Reading the results in_progress") {
		t.Errorf("cards:\n%s\nwant the delay's card titled with the line", strings.Join(got, "\n"))
	}
}
