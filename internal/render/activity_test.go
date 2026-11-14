package render

import (
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// The idle card is Working… with no detail: Slack appends a card's details, so the tool call that claims the card
// sends its detail once, and the card begins without one.
func TestWorkingStepHasATitleAndNoDetail(t *testing.T) {
	got := WorkingStep("s3")
	want := chat.Step{ID: "s3", Title: "Working…", State: chat.StepRunning}
	if got != want {
		t.Errorf("WorkingStep = %+v, want %+v", got, want)
	}
}

// A card that no tool call claimed is retitled before it is finished, so that no card shows a tick on Working…, and only text
// that came while it was open says it wrote the answer. A task that did not finish gets an error, never a tick.
func TestWorkingEndedIsRetitledByHowItEnded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		e     Ending
		title string
		state chat.StepState
	}{
		{"text", EndedByText, "Wrote the answer", chat.StepDone},
		{"text that a tool call followed", EndedByUpdate, "Wrote an update", chat.StepDone},
		{"a completed task", EndedCompleted, "Finished", chat.StepDone},
		{"a pause for input", EndedPaused, "Waiting for your answer", chat.StepDone},
		{"a task that did not finish", EndedFailed, "Stopped", chat.StepFailed},
		{"not ended", NotEnded, "Working…", chat.StepRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := WorkingEnded("s3", tc.e)
			if want := (chat.Step{ID: "s3", Title: tc.title, State: tc.state}); got != want {
				t.Errorf("WorkingEnded = %+v, want %+v", got, want)
			}
		})
	}
}

// The activity card carries its counts in the title, which Slack replaces, and its summary as the detail of the
// finishing chunk only: a detail is appended, so the running card has none. How it ended says in its title.
func TestActivityStepCountsInTheTitleAndSummarisesWhenEnded(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		steps, failed, updates int
		elapsed                time.Duration
		e                      Ending
		wantTitle              string
		wantDetail             string
		wantState              chat.StepState
	}{
		{"nothing yet", 0, 0, 0, 5 * time.Second, NotEnded, "Working…", "", chat.StepRunning},
		{"one step", 1, 0, 0, 5 * time.Second, NotEnded, "Working… 1 step", "", chat.StepRunning},
		{"steps", 12, 0, 0, 70 * time.Second, NotEnded, "Working… 12 steps", "", chat.StepRunning},
		{"steps and failures", 12, 3, 0, 70 * time.Second, NotEnded, "Working… 12 steps, 3 failed", "", chat.StepRunning},
		{"text, with failures", 16, 7, 0, 206*time.Second + 400*time.Millisecond, EndedByText, "Worked", "Took 3 min 26 s, 16 steps (7 failed)", chat.StepDone},
		{"completed, none failed", 4, 0, 0, 42 * time.Second, EndedCompleted, "Worked", "Took 42 s, 4 steps", chat.StepDone},
		{"one step", 1, 0, 0, 2 * time.Minute, EndedCompleted, "Worked", "Took 2 min, 1 step", chat.StepDone},
		{"no step count, as on a dialect with no tool events", 0, 0, 0, 90 * time.Second, EndedCompleted, "Worked", "Took 1 min 30 s", chat.StepDone},
		{"paused", 2, 0, 0, 9 * time.Second, EndedPaused, "Waiting for your answer", "Took 9 s, 2 steps", chat.StepDone},
		{"failed", 2, 1, 0, 9 * time.Second, EndedFailed, "Stopped", "Took 9 s, 2 steps (1 failed)", chat.StepFailed},
		{"working updates, as on an agent that marks no tool part", 0, 0, 5, 20 * time.Second, NotEnded, "Working… 5 updates", "", chat.StepRunning},
		{"one update", 0, 0, 1, 20 * time.Second, NotEnded, "Working… 1 update", "", chat.StepRunning},
		{"updates, ended", 0, 0, 5, 20 * time.Second, EndedCompleted, "Worked", "Took 20 s, 5 updates", chat.StepDone},
		{"updates, stopped", 0, 0, 2, 20 * time.Second, EndedFailed, "Stopped", "Took 20 s, 2 updates", chat.StepFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ActivityStep("s1", Counts{Steps: tc.steps, Failed: tc.failed, Updates: tc.updates}, tc.elapsed, tc.e)
			want := chat.Step{ID: "s1", Title: tc.wantTitle, Detail: tc.wantDetail, State: tc.wantState}
			if got != want {
				t.Errorf("ActivityStep = %+v, want %+v", got, want)
			}
		})
	}
}

// The alert thread's line gives the elapsed whole minutes and the counts, and never a tool name or an argument.
func TestActivityLine(t *testing.T) {
	for _, tc := range []struct {
		elapsed                time.Duration
		steps, failed, updates int
		want                   string
	}{
		{0, 0, 0, 0, "Working…"},
		{59 * time.Second, 0, 0, 0, "Working…"},
		{59 * time.Second, 3, 0, 0, "Working… (3 steps)"},
		{2*time.Minute + 59*time.Second, 12, 0, 0, "Working… (2 min, 12 steps)"},
		{2 * time.Minute, 12, 3, 0, "Working… (2 min, 12 steps, 3 failed)"},
		{time.Minute, 0, 0, 0, "Working… (1 min)"},
		{-time.Second, 1, 0, 0, "Working… (1 step)"},
		{2 * time.Minute, 0, 0, 7, "Working… (2 min, 7 updates)"},
	} {
		c := Counts{Steps: tc.steps, Failed: tc.failed, Updates: tc.updates}
		if got := ActivityLine(tc.elapsed, c); got != tc.want {
			t.Errorf("ActivityLine(%v, %+v) = %q, want %q", tc.elapsed, c, got, tc.want)
		}
	}
}

// A status line goes under the text, and is the message's text where it has none.
func TestWithStatusAddsALineAndFillsAnEmptyFallback(t *testing.T) {
	bare := WithStatus(chat.Message{}, "Working…")
	if len(bare.Parts) != 1 || bare.Parts[0] != (chat.Context{Text: "Working…"}) || bare.Fallback != "Working…" {
		t.Errorf("WithStatus on an empty message = %+v; want the line and a fallback that says it", bare)
	}
	with := WithStatus(chat.Message{Fallback: "Looking", Parts: []chat.Part{chat.Body{Markdown: "Looking"}}}, "Working…")
	if len(with.Parts) != 2 || with.Parts[1] != (chat.Context{Text: "Working…"}) || with.Fallback != "Looking" {
		t.Errorf("WithStatus on a message with text = %+v; want the line under the text and the fallback kept", with)
	}
}

// The progress line is the agent's own text, untrusted: redacted before anything is shown, on one line, and capped as a tool's
// summary is.
func TestProgressLineIsRedactedOneLineAndCapped(t *testing.T) {
	long := strings.Repeat("checking ", 40)
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", "Reading the pod logs", "Reading the pod logs"},
		{"lines joined", "Reading\n  the logs\r\n", "Reading the logs"},
		{"a secret", "Logging in with password=hunter2 now", "Logging in with password=[redacted] now"},
		{"hidden characters", "Read\u200bing", "Reading"},
		{"nothing that shows", " \n\u200b ", ""},
		{"capped", long, chat.Truncate(strings.TrimSpace(long), maxToolArgs)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProgressLine(tc.in); got != tc.want {
				t.Errorf("ProgressLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
