package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
)

// authWatch is an outbox.Poster that reports a fatal auth error of the Poster it wraps.
type authWatch struct {
	outbox.Poster
	report func(code string)
}

// WatchPosts returns p with one addition: when a Post or a Find fails with an error that says the
// tokens will never work (slack.FatalAuthCode), report is called with its code. The error and the
// result are returned unchanged. The outbox treats such an error as transient, so a bot token
// revoked while the connection is up would otherwise leave courier running and posting nothing; the
// report is how the post side tells the run policy (see WithAuthFailure).
func WatchPosts(p outbox.Poster, report func(code string)) outbox.Poster {
	return authWatch{p, report}
}

func (w authWatch) Post(ctx context.Context, channel, threadTS string, content slack.Content, outboxID string) (string, error) {
	ts, err := w.Poster.Post(ctx, channel, threadTS, content, outboxID)
	w.check(err)
	return ts, err
}

func (w authWatch) Find(ctx context.Context, channel, threadTS, outboxID string, since time.Time) (string, bool, error) {
	ts, found, err := w.Poster.Find(ctx, channel, threadTS, outboxID, since)
	w.check(err)
	return ts, found, err
}

func (w authWatch) check(err error) {
	if code := slack.FatalAuthCode(err); code != "" {
		w.report(code)
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
