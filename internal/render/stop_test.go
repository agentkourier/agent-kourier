package render_test

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/render"
)

func TestStopValueRoundTripsAndRejectsInvalid(t *testing.T) {
	for _, want := range []render.StopValue{
		{Thread: "1700.1", TaskID: "task-1"},
		{Thread: "1700.1", TaskID: "task|with|pipes"},
	} {
		encoded := want.String()
		got, ok := render.ParseStopValue(encoded)
		if !ok || got != want {
			t.Errorf("ParseStopValue(%q) = %+v, %v; want %+v, true", encoded, got, ok, want)
		}
	}
	for _, bad := range []string{
		"",
		"stop1",
		"stop1|1700.1",
		"stop1||task-1",
		"stop1|1700.1|",
		"stop2|1700.1|task-1",
		"fb1|investigation|useful|1700.1|turn-1",
		"ix-0123",
	} {
		if got, ok := render.ParseStopValue(bad); ok {
			t.Errorf("ParseStopValue(%q) = %+v, true; want false", bad, got)
		}
	}
}

func TestStopPartsBuildsValidNeutralButtonsAndRespectsCap(t *testing.T) {
	parts := render.StopParts("1700.1", "task-42")
	if len(parts) != 1 {
		t.Fatalf("StopParts returned %d parts; want 1", len(parts))
	}
	btns, ok := parts[0].(chat.Buttons)
	if !ok || len(btns.Buttons) != 1 {
		t.Fatalf("part[0] = %#v; want chat.Buttons with 1 button", parts[0])
	}
	btn := btns.Buttons[0]
	if btn.ID != "stop-task" || btn.Label != "Stop" || btn.Value != "stop1|1700.1|task-42" {
		t.Errorf("button = %+v; want ID=stop-task Label=Stop Value=stop1|1700.1|task-42", btn)
	}
	if err := (chat.Message{Fallback: "working", Parts: parts}).Validate(); err != nil {
		t.Errorf("Validate = %v; want nil", err)
	}
	if got := render.StopParts("", "task-1"); got != nil {
		t.Errorf("empty thread -> %v; want nil", got)
	}
	if got := render.StopParts("1700.1", ""); got != nil {
		t.Errorf("empty taskID -> %v; want nil", got)
	}
	atCap := render.StopParts(strings.Repeat("a", 250), strings.Repeat("b", 250))
	if len(atCap) != 1 {
		t.Errorf("at cap (500 bytes) -> %v; want 1 part", atCap)
	}
	overCap := render.StopParts(strings.Repeat("a", 250), strings.Repeat("b", 251))
	if overCap != nil {
		t.Errorf("over cap (501 bytes) -> %v; want nil", overCap)
	}
}

func TestStopLaneUsesPrefix(t *testing.T) {
	lane := render.StopLane("1700.1")
	if lane != "stop1|1700.1" || !strings.HasPrefix(lane, render.StopLanePrefix) {
		t.Errorf("StopLane = %q (prefix %q); want stop1|1700.1", lane, render.StopLanePrefix)
	}
}
