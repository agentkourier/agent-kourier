package render

import (
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

// A card that no tool call claimed is retitled before it is finished, so that no card shows a tick on Working….
func TestWroteStepIsDone(t *testing.T) {
	got := WroteStep("s3")
	want := chat.Step{ID: "s3", Title: "Wrote the answer", State: chat.StepDone}
	if got != want {
		t.Errorf("WroteStep = %+v, want %+v", got, want)
	}
}

// The activity card carries its counts in the title, which Slack replaces, and its summary as the detail of the
// finishing chunk only: a detail is appended, so the running card has none.
func TestActivityStepCountsInTheTitleAndSummarisesWhenDone(t *testing.T) {
	for _, tc := range []struct {
		name           string
		steps, failed  int
		elapsed        time.Duration
		done           bool
		wantTitle      string
		wantDetail     string
		wantStepsState chat.StepState
	}{
		{"nothing yet", 0, 0, 5 * time.Second, false, "Working…", "", chat.StepRunning},
		{"one step", 1, 0, 5 * time.Second, false, "Working… 1 step", "", chat.StepRunning},
		{"steps", 12, 0, 70 * time.Second, false, "Working… 12 steps", "", chat.StepRunning},
		{"steps and failures", 12, 3, 70 * time.Second, false, "Working… 12 steps, 3 failed", "", chat.StepRunning},
		{"done with failures", 16, 7, 206*time.Second + 400*time.Millisecond, true, "Worked", "Took 3 min 26 s, 16 steps (7 failed)", chat.StepDone},
		{"done, none failed", 4, 0, 42 * time.Second, true, "Worked", "Took 42 s, 4 steps", chat.StepDone},
		{"done, one step", 1, 0, 2 * time.Minute, true, "Worked", "Took 2 min, 1 step", chat.StepDone},
		{"done with no step count, as on a dialect with no tool events", 0, 0, 90 * time.Second, true, "Worked", "Took 1 min 30 s", chat.StepDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ActivityStep("s1", tc.steps, tc.failed, tc.elapsed, tc.done)
			want := chat.Step{ID: "s1", Title: tc.wantTitle, Detail: tc.wantDetail, State: tc.wantStepsState}
			if got != want {
				t.Errorf("ActivityStep = %+v, want %+v", got, want)
			}
		})
	}
}

// The alert thread's line gives the elapsed whole minutes and the counts, and never a tool name or an argument.
func TestActivityLine(t *testing.T) {
	for _, tc := range []struct {
		elapsed       time.Duration
		steps, failed int
		want          string
	}{
		{0, 0, 0, "Working…"},
		{59 * time.Second, 0, 0, "Working…"},
		{59 * time.Second, 3, 0, "Working… (3 steps)"},
		{2*time.Minute + 59*time.Second, 12, 0, "Working… (2 min, 12 steps)"},
		{2 * time.Minute, 12, 3, "Working… (2 min, 12 steps, 3 failed)"},
		{time.Minute, 0, 0, "Working… (1 min)"},
		{-time.Second, 1, 0, "Working… (1 step)"},
	} {
		if got := ActivityLine(tc.elapsed, tc.steps, tc.failed); got != tc.want {
			t.Errorf("ActivityLine(%v, %d, %d) = %q, want %q", tc.elapsed, tc.steps, tc.failed, got, tc.want)
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
