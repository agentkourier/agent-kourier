package chat

import "errors"

// ErrNoSteps is returned by Stream.Step on a platform without progress steps (Caps.Steps false). A caller that
// asked the platform first, as it should, never sees it.
var ErrNoSteps = errors.New("chat: the platform shows no progress steps")

// StepState is how far a Step has got.
type StepState int

// The states of a Step. The zero value is Running, which is how a step begins.
const (
	// StepRunning is a step under way.
	StepRunning StepState = iota
	// StepDone is a step that finished.
	StepDone
	// StepFailed is a step that finished and reported that it failed.
	StepFailed
	// StepEnded is a step that was still under way when the stream ended, and whose outcome is not known: no
	// result for it arrived. It is not success, and a platform must not show it as one.
	StepEnded
)

func (s StepState) String() string {
	switch s {
	case StepRunning:
		return "running"
	case StepDone:
		return "done"
	case StepFailed:
		return "failed"
	case StepEnded:
		return "ended"
	}
	return "unknown"
}

// Step is one unit of the agent's work, such as a tool call, shown as progress in a stream while the reply is
// written. A step is begun by Stream.Step with the state Running, and finished by Stream.Step again with the
// same ID and another state. Nothing in a stream is taken back: a step is only ever moved forward.
type Step struct {
	// ID names the step in its stream. It is courier's own, never text the agent wrote, and a caller does not
	// reuse one in a stream.
	ID string
	// Title is what the step is, such as a tool's name: literal text, one line, untrusted (see the package
	// comment), and must not be empty after cleaning, or the adapter shows it as blank.
	Title string
	// Detail is a short line about it, such as the redacted summary of a tool's arguments (the secrets taken out by
	// the caller, never the raw arguments): literal text, one line, untrusted. It may be empty.
	Detail string
	// State is how far the step has got.
	State StepState
}
