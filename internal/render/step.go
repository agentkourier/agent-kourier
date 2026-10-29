package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// Step returns the progress step of a tool call in the state given: the tool's name as its title and the redacted summary
// of its arguments as its detail. They are the name and summary of the tool line (Tool), the same redacted summary that
// the status line of the update path shows, put on one line and capped as it is: the raw arguments never reach a step. id is Agent Kourier's own name for the step in its stream, never the agent's call ID.
func Step(id string, call agent.ToolCallEvent, state chat.StepState) chat.Step {
	line := Tool(call.Name, call.Args)
	return chat.Step{ID: id, Title: line.Name, Detail: line.Summary, State: state}
}

// Failed says whether the response of a tool call reports that the call failed. A response is whatever the tool
// returned, so it is read for the two shapes that are known: an "error" entry that holds something, as kagent reports a
// rejected or unconfirmed call (test/fixtures/kagent/approval), and an "isError" or "is_error" entry that is true, as an
// MCP tool does. An error entry holds something if it is true, a string that is not empty, or a map or a list that is
// not: false, null, "", {}, [] and a number, such as the 0 of a status, say that nothing went wrong or say nothing. Anything
// else, including text that says "error", is a success: a tool's own words are the agent's to report.
func Failed(response any) bool {
	m, ok := response.(map[string]any)
	if !ok {
		return false
	}
	switch e := m["error"].(type) {
	case bool:
		if e {
			return true
		}
	case string:
		if e != "" {
			return true
		}
	case map[string]any:
		if len(e) > 0 {
			return true
		}
	case []any:
		if len(e) > 0 {
			return true
		}
	}
	for _, key := range []string{"isError", "is_error"} {
		if failed, _ := m[key].(bool); failed {
			return true
		}
	}
	return false
}

// EarlierSteps is the line above the steps of a message that shows only the latest of them: how many came before, n of them.
func EarlierSteps(n int) string {
	if n == 1 {
		return "1 earlier step is not shown."
	}
	return fmt.Sprintf("%d earlier steps are not shown.", n)
}

// ApprovalTitle begins the title of the step of a tool call that waits for a person to approve it, which the tool's name
// ends.
const ApprovalTitle = "Approval requested: "

// AwaitingApproval is the step of a tool call that the task paused to have approved, in the state it is left in when the
// stream stops at the pause: retitled to say that the approval was asked for, and done, since the call has not failed
// and a platform may show a step that is not done at the stop of its stream as failed (Slack does). Its detail is the
// call's, as Step has it. The answer's own message shows the call again, with how it ended.
func AwaitingApproval(id string, call agent.ToolCallEvent) chat.Step {
	st := Step(id, call, chat.StepDone)
	st.Title = ApprovalTitle + st.Title
	return st
}

// The titles of the cards that stand for time, not for a tool call. They name nothing the agent did: the model's reasoning
// is never read, so a gap is not called thinking.
const (
	// WorkingTitle is the title of the idle card and of the activity card while it runs.
	WorkingTitle = "Working…"
	// WroteTitle is the title of an idle card that no tool call claimed, once text came while it was open and while no tool
	// call has followed that text: the gap was the model writing, and what it wrote is the answer for all that is known.
	WroteTitle = "Wrote the answer"
	// UpdateTitle is the title of such a card once a tool call follows the text: the text was written on the way, and the
	// answer comes after the call.
	UpdateTitle = "Wrote an update"
	// FinishedTitle is the title of an idle card that the end of a completed task closed, with no text since it opened.
	FinishedTitle = "Finished"
	// WorkedTitle is the title of the activity card when its task completed, or text came.
	WorkedTitle = "Worked"
	// WaitingTitle is the title of either card when the task paused to ask for input: what is shown next is the question.
	WaitingTitle = "Waiting for your answer"
	// StoppedTitle is the title of either card when the task failed, was cancelled or was rejected. The card is an error, not
	// a tick: nothing was finished.
	StoppedTitle = "Stopped"
)

// Ending is how a card that stands for time ended. The zero value is a card that is still under way.
type Ending int

// The endings of a card that stands for time.
const (
	// NotEnded is a card under way.
	NotEnded Ending = iota
	// EndedByText is a card that text came to while it was open.
	EndedByText
	// EndedByUpdate is a card that text came to while it was open, which a tool call followed: the text was not the answer.
	EndedByUpdate
	// EndedCompleted is a card that the end of a completed task closed.
	EndedCompleted
	// EndedPaused is a card that a pause for input closed.
	EndedPaused
	// EndedFailed is a card that a task that failed, was cancelled or was rejected closed.
	EndedFailed
)

// endState is the state of a card that ended as e: done, or failed for a task that did not finish.
func (e Ending) endState() chat.StepState {
	if e == EndedFailed {
		return chat.StepFailed
	}
	return chat.StepDone
}

// WorkingStep is the idle card: Working…, under way, with no detail. A platform that appends a card's detail (Slack
// does) is sent the detail once, by the tool call that claims the card, so the card begins without one.
func WorkingStep(id string) chat.Step {
	return chat.Step{ID: id, Title: WorkingTitle, State: chat.StepRunning}
}

// WorkingEnded is an idle card that no tool call claimed, finished as e: it is retitled first, so that no card shows a tick
// on Working…, and a task that did not finish gets an error and not a tick.
func WorkingEnded(id string, e Ending) chat.Step {
	title := FinishedTitle
	switch e {
	case EndedByText:
		title = WroteTitle
	case EndedByUpdate:
		title = UpdateTitle
	case EndedPaused:
		title = WaitingTitle
	case EndedFailed:
		title = StoppedTitle
	case NotEnded:
		return WorkingStep(id)
	}
	return chat.Step{ID: id, Title: title, State: e.endState()}
}

// ActivityStep is the one card of the activity mode: it counts the tool calls that have their result, and how many of
// them failed, and names no tool and no argument. The counts are in the title, which a platform replaces; the summary,
// which is the detail a platform appends, is in the card only once it has ended, so that it is sent once. A card that
// ended for text or a completed task is Worked; one that a pause closed is Waiting, and one that a task that did not finish
// closed is Stopped, an error.
func ActivityStep(id string, steps, failed int, elapsed time.Duration, e Ending) chat.Step {
	if e == NotEnded {
		title := WorkingTitle
		if counts := counted(steps, failed); counts != "" {
			title += " " + counts
		}
		return chat.Step{ID: id, Title: title, State: chat.StepRunning}
	}
	summary := "Took " + spoken(elapsed)
	if steps > 0 {
		summary += ", " + plural(steps, "step")
		if failed > 0 {
			summary += fmt.Sprintf(" (%d failed)", failed)
		}
	}
	title := WorkedTitle
	switch e {
	case EndedPaused:
		title = WaitingTitle
	case EndedFailed:
		title = StoppedTitle
	}
	return chat.Step{ID: id, Title: title, Detail: summary, State: e.endState()}
}

// ActivityLine is the activity mode's status line for a message that is updated: Working… with the whole minutes elapsed
// and the counts. It is rewritten only when an event makes a write, so the minutes are those of that moment.
func ActivityLine(elapsed time.Duration, steps, failed int) string {
	var parts []string
	if m := int(elapsed / time.Minute); m > 0 {
		parts = append(parts, fmt.Sprintf("%d min", m))
	}
	if c := counted(steps, failed); c != "" {
		parts = append(parts, c)
	}
	if len(parts) == 0 {
		return WorkingTitle
	}
	return WorkingTitle + " (" + strings.Join(parts, ", ") + ")"
}

// counted is "12 steps, 3 failed": the steps that have their result, and the failures among them, each left out at zero.
func counted(steps, failed int) string {
	var parts []string
	if steps > 0 {
		parts = append(parts, plural(steps, "step"))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// spoken is a duration in whole seconds, as "3 min 26 s", "2 min" or "42 s".
func spoken(d time.Duration) string {
	secs := int(d.Round(time.Second) / time.Second)
	secs = max(secs, 0)
	switch m, s := secs/60, secs%60; {
	case m == 0:
		return fmt.Sprintf("%d s", s)
	case s == 0:
		return fmt.Sprintf("%d min", m)
	default:
		return fmt.Sprintf("%d min %d s", m, s)
	}
}
