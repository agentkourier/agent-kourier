package render

import (
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

const (
	stopValueVersion = "stop1"
	stopValueSep     = "|"
	// StopLanePrefix prefixes the press-table lane of every stop button press so the press worker routes it to the stop handler.
	StopLanePrefix = stopValueVersion + stopValueSep
	// maxStopIDs bounds the thread and task IDs in a stop button's value, matching feedback.maxIDs so a button never nears a platform's value cap.
	maxStopIDs = 500
)

// StopValue is what a Stop button holds: the thread the task belongs to and the task ID it stops. It holds no agent or user text.
type StopValue struct {
	Thread string
	TaskID string
}

// String encodes v as "stop1|<thread>|<taskID>", with TaskID last so a task ID containing "|" round-trips verbatim.
func (v StopValue) String() string {
	return stopValueVersion + stopValueSep + v.Thread + stopValueSep + v.TaskID
}

// ParseStopValue parses a Stop button value produced by StopValue.String, returning false for any other value or version.
func ParseStopValue(s string) (StopValue, bool) {
	p := strings.SplitN(s, stopValueSep, 3)
	if len(p) != 3 || p[0] != stopValueVersion || p[1] == "" || p[2] == "" {
		return StopValue{}, false
	}
	return StopValue{Thread: p[1], TaskID: p[2]}, true
}

// StopLane is the worker lane for stop presses in thread: all stop presses in one thread serialize in order.
func StopLane(thread string) string { return StopLanePrefix + thread }

// StopParts returns the neutral button part that stops taskID in thread, or nil if either ID is empty or their combined length exceeds maxStopIDs.
func StopParts(thread, taskID string) []chat.Part {
	if thread == "" || taskID == "" || len(thread)+len(taskID) > maxStopIDs {
		return nil
	}
	return []chat.Part{
		chat.Buttons{Buttons: []chat.Button{
			{ID: "stop-task", Label: "Stop", Value: StopValue{Thread: thread, TaskID: taskID}.String()},
		}},
	}
}
