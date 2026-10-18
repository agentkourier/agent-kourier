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

// The titles of the cards that stand for time, not for a tool call. They name nothing the agent did: the model's reasoning
// is never read, so a gap is not called thinking.
const (
	// WorkingTitle is the title of the idle card and of the activity card while it runs.
	WorkingTitle = "Working…"
	// WroteTitle is the title of an idle card that no tool call claimed, once text came: the gap was the model writing.
	WroteTitle = "Wrote the answer"
	// WorkedTitle is the title of the activity card when it is done.
	WorkedTitle = "Worked"
)

// WorkingStep is the idle card: Working…, under way, with no detail. A platform that appends a card's detail (Slack
// does) is sent the detail once, by the tool call that claims the card, so the card begins without one.
func WorkingStep(id string) chat.Step {
	return chat.Step{ID: id, Title: WorkingTitle, State: chat.StepRunning}
}

// WroteStep is an idle card that no tool call claimed, finished: it is retitled first, so that no card shows a tick on
// Working….
func WroteStep(id string) chat.Step {
	return chat.Step{ID: id, Title: WroteTitle, State: chat.StepDone}
}

// ActivityStep is the one card of the activity mode: it counts the tool calls that have their result, and how many of
// them failed, and names no tool and no argument. The counts are in the title, which a platform replaces; the summary,
// which is the detail a platform appends, is in the card only once it is done, so that it is sent once.
func ActivityStep(id string, steps, failed int, elapsed time.Duration, done bool) chat.Step {
	if !done {
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
	return chat.Step{ID: id, Title: WorkedTitle, Detail: summary, State: chat.StepDone}
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
