package live

import (
	"context"
	"fmt"
	"math"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// down is the platform of a turn whose connection no platform runs: every write fails with err, which wraps
// chat.ErrConnectionDown, and Classify says that is transient, so the run treats the platform as down (a write is tried again,
// and given up on after Config.Attempts, as for a platform that does not answer) while the outbox row of the reply waits for
// the connection. It can do nothing a platform can (no streams, steps, edits or busy mark), so a run on it neither streams nor
// marks the thread busy, and it cuts no text: the message that goes through the outbox is cut by the platform that posts it,
// when the connection runs. The methods a run calls are the ones defined here; the embedded Platform is nil and stands for the rest, which
// a run never calls on a platform that has no capability for them.
type down struct {
	chat.Platform
	err error
}

func (d down) Capabilities() chat.Caps { return chat.Caps{MaxText: math.MaxInt32} }

func (d down) Classify(chat.Op, error) chat.Verdict { return chat.Verdict{Class: chat.Transient} }

func (d down) Update(context.Context, chat.Ref, chat.Outgoing) error { return d.fail("update") }

func (d down) Stream(context.Context, chat.Target, string, string) (chat.Stream, error) {
	return nil, d.fail("stream")
}

func (d down) StreamAt(r chat.Ref) chat.Stream { return downStream{r, d.err} }

// RefAt is the Ref a platform that was up would have made, as far as it can be known without one.
func (d down) RefAt(channel, thread, id string) chat.Ref {
	return chat.Ref{Channel: channel, Thread: thread, ID: id}
}

func (d down) fail(what string) error { return fmt.Errorf("live: %s: %w", what, d.err) }

// downStream is the stream of a message on a platform that is down: it makes no call and every one fails.
type downStream struct {
	ref chat.Ref
	err error
}

func (s downStream) Ref() chat.Ref { return s.ref }

func (s downStream) Append(context.Context, string) error { return s.fail("append") }

func (s downStream) Step(context.Context, chat.Step) error { return s.fail("step") }

func (s downStream) Stop(context.Context, string, chat.Message, string) error { return s.fail("stop") }

func (s downStream) fail(what string) error { return fmt.Errorf("live: stream %s: %w", what, s.err) }
