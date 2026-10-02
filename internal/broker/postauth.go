package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
)

// authWatch is an outbox.Platform that reports a fatal auth error of the Platform it wraps.
type authWatch struct {
	outbox.Platform
	report func(code string)
}

// WatchAuth returns p with one addition: when a Post, a Find or a React fails with an error that the platform
// classifies as a credential it rejects (chat.FatalAuth), report is called with the platform's code. The error and
// the result are returned unchanged. The outbox treats such an error as transient, so a bot token revoked while
// the connection is up would otherwise leave courier running and posting nothing; the report is how the post side
// tells the run policy (see WithAuthFailure). The edits, the stream calls and the ephemerals are not watched yet
// (hardening).
func WatchAuth(p outbox.Platform, report func(code string)) outbox.Platform {
	return authWatch{p, report}
}

func (w authWatch) Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error) {
	ref, err := w.Platform.Post(ctx, t, o, tag)
	w.check(chat.OpPost, err)
	return ref, err
}

func (w authWatch) Find(ctx context.Context, t chat.Target, tag string, since time.Time) (chat.Ref, bool, error) {
	ref, found, err := w.Platform.Find(ctx, t, tag, since)
	w.check(chat.OpPost, err)
	return ref, found, err
}

func (w authWatch) React(ctx context.Context, r chat.Ref, name string) error {
	err := w.Platform.React(ctx, r, name)
	w.check(chat.OpReact, err)
	return err
}

func (w authWatch) check(op chat.Op, err error) {
	if err == nil {
		return
	}
	if v := w.Classify(op, err); v.Class == chat.FatalAuth {
		w.report(v.Code)
	}
}

// stopped is the error of a Slack run whose context has ended: the context's cause if a post
// reported a rejection (WithAuthFailure), which is a failure, and otherwise a shutdown, wrapping
// the context's error.
func stopped(ctx context.Context) error {
	if cause := context.Cause(ctx); errors.Is(cause, ErrSlackAuth) {
		return cause
	}
	return fmt.Errorf("slack run stopped: %w", ctx.Err())
}

// WithAuthFailure returns a context for RunSlack that ends, with ErrSlackAuth as its cause, when
// report is called with a fatal code. RunSlack then returns that cause, so the process exits as for
// a rejected connection, and not as for a shutdown. stop releases the context. report may be called
// from any goroutine, and only the first call counts.
func WithAuthFailure(parent context.Context) (ctx context.Context, report func(code string), stop context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	return ctx, func(code string) {
		cancel(fmt.Errorf("%w: %s (reported by a post)", ErrSlackAuth, code))
	}, func() { cancel(nil) }
}
