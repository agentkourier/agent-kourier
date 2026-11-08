package live

import (
	"context"
	"fmt"
	"math"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// runPlatform is what a run calls of the platform of its turn's connection: a chat.Platform has more, and a call of one of those
// does not compile here, which is what keeps a run from reaching a method that the platform of a connection that is down does not
// have. Capabilities is asked when the run starts, of the platform itself.
type runPlatform interface {
	chat.BusyMarker
	Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error
	Stream(ctx context.Context, t chat.Target, to, markdown string) (chat.Stream, error)
	StreamAt(r chat.Ref) chat.Stream
	RefAt(channel, thread, id string) chat.Ref
	Classify(op chat.Op, err error) chat.Verdict
}

// down is the platform of a turn whose connection no platform runs: every write fails with err, which wraps
// chat.ErrConnectionDown, and Classify says that is transient, so the run treats the platform as down (a write is tried again,
// and given up on after Config.Attempts, as for a platform that does not answer) while the outbox row of the reply waits for
// the connection. It can do nothing a platform can (no streams, steps, edits or busy mark), so a run on it neither streams nor
// marks the thread busy, and it cuts no text: the message that goes through the outbox is cut by the platform that posts it,
// when the connection runs.
type down struct {
	conn string
	err  error
}

var _ runPlatform = down{}

// Capabilities says what down can do: nothing, and no text is cut.
func (d down) Capabilities() chat.Caps { return chat.Caps{MaxText: math.MaxInt32} }

func (d down) Classify(chat.Op, error) chat.Verdict { return chat.Verdict{Class: chat.Transient} }

func (d down) Update(context.Context, chat.Ref, chat.Outgoing) error { return d.fail("update") }

func (d down) Stream(context.Context, chat.Target, string, string) (chat.Stream, error) {
	return nil, d.fail("stream")
}

func (d down) StreamAt(r chat.Ref) chat.Stream { return downStream{r, d.err} }

func (d down) MarkBusy(context.Context, chat.Target, bool) error { return d.fail("mark busy") }

// RefAt is the Ref a platform that was up would have made, as far as it can be known without one: the adapter's own Refs name
// the platform too ("slack"), which down does not know.
func (d down) RefAt(channel, thread, id string) chat.Ref {
	return chat.Ref{Conn: d.conn, Channel: channel, Thread: thread, ID: id}
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
