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
// chat.ErrConnectionDown, and Classify says that is transient, so the run treats the platform as down (a write is tried again
// every Config.Throttle, and not counted against Config.Attempts, while the outbox row of the reply waits for the connection, and the
// run looks the connection's platform up again after each failure: reresolve). It can do nothing a platform can (no streams, steps,
// edits or busy mark), so a run on it neither streams nor marks the thread busy, and it cuts no text: the message that goes through
// the outbox is cut by the platform that posts it, when the connection runs.
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

// reresolve looks the platform of the turn's connection up again, after a write failed because the platform the run holds is not the
// connection's now: nothing ran the connection when the turn started (the run holds down), the runtime it holds was retired, or its
// credential was rejected after a restart had halted it, and the platform it holds fails fast or is refused. A lookup that fails
// leaves the run as it is: the platform it holds fails the same way, and the next write looks again. One that works replaces the
// platform, what the run knows of its capabilities (the limits of the text) and the busy marking's. What the run decided at its start
// from the capabilities it had stays: a turn that began down has no stream, steps or busy mark, and goes on without them.
//
// A Ref made while the connection was down does not name the platform, and is made again by the platform that runs it. A stream that
// the earlier runtime started is not continued: the new platform's stream of the message is blind to what the old one sent (the
// adapter's StreamAt), so the turn does what it does for a stream it lost (takeOver): the new platform stops the stream, and the
// message is edited to the whole text, with the steps. The outbox row of the stream has the message's ts already. It is the goroutine's.
func (u *Run) reresolve(ctx context.Context, ref *chat.Ref) {
	p, err := u.cfg.Platforms.Platform(u.turn.Connection)
	if err != nil {
		return
	}
	u.platform, u.caps = p, p.Capabilities()
	u.busy.follow(p)
	if ref.ID != "" && ref.Platform == "" {
		*ref = p.RefAt(ref.Channel, ref.Thread, ref.ID)
	}
	if u.streamOpen() {
		*ref = u.stream.Ref()
		u.streaming, u.adopted, u.adoptStop, u.sent = false, true, true, ""
		u.cfg.Logger.WarnContext(ctx, "live: the stream was started on a runtime of the connection that is gone; it is stopped and the message edited to the whole text", "connection", u.turn.Connection, "channel", u.turn.Channel)
		return
	}
	u.cfg.Logger.InfoContext(ctx, "live: the turn's connection runs; the turn goes on through its platform", "connection", u.turn.Connection, "channel", u.turn.Channel)
}
