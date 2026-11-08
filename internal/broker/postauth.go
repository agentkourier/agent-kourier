package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// authRejected is the error of a run that ended because the platform rejected the credentials for good. It is ErrSlackAuth for errors.Is, and
// it keeps the platform's code (never a token), which the supervisor logs for a team connection that stops.
type authRejected struct {
	code string
	// via says what saw the rejection when it was not the connection's own run: "reported by a call".
	via string
}

func (e *authRejected) Error() string {
	if e.via == "" {
		return ErrSlackAuth.Error() + ": " + e.code
	}
	return ErrSlackAuth.Error() + ": " + e.code + " (" + e.via + ")"
}

// Is makes the error ErrSlackAuth.
func (e *authRejected) Is(target error) bool { return target == ErrSlackAuth }

// authWatch is a chat.Platform that reports a fatal auth error of the Platform it wraps.
type authWatch struct {
	chat.Platform
	report func(code string)
}

// WatchAuth returns p with one addition: when a call of it fails with an error that the platform classifies as a credential it
// rejects (chat.FatalAuth), report is called with the platform's code. The error and the result are returned unchanged. The calls that
// make it are every one that reaches the platform's API: posts, lookups, edits, streams (and the calls on the streams it hands out),
// ephemerals, reactions, busy marks, thread reads, and the reads of membership, groups, channels and users. Run is not one of them: it
// ends by itself, and RunPlatform reads the verdict. The callers treat such an error as transient, so a bot token revoked while the
// connection is up would otherwise leave Agent Kourier running and failing every call; the report is how the call side tells the run policy
// (see WithAuthFailure).
func WatchAuth(p chat.Platform, report func(code string)) chat.Platform {
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

func (w authWatch) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	err := w.Platform.Update(ctx, r, o)
	w.check(chat.OpPost, err)
	return err
}

func (w authWatch) Stream(ctx context.Context, t chat.Target, to, markdown string) (chat.Stream, error) {
	s, err := w.Platform.Stream(ctx, t, to, markdown)
	w.check(chat.OpPost, err)
	if err != nil {
		return nil, err
	}
	return watchedStream{Stream: s, w: w}, nil
}

// StreamAt makes no call; the stream it returns is watched, as the one Stream returns.
func (w authWatch) StreamAt(r chat.Ref) chat.Stream {
	return watchedStream{Stream: w.Platform.StreamAt(r), w: w}
}

func (w authWatch) Ephemeral(ctx context.Context, t chat.Target, user string, m chat.Message) error {
	err := w.Platform.Ephemeral(ctx, t, user, m)
	w.check(chat.OpPost, err)
	return err
}

func (w authWatch) React(ctx context.Context, r chat.Ref, name string) error {
	err := w.Platform.React(ctx, r, name)
	w.check(chat.OpReact, err)
	return err
}

func (w authWatch) Unreact(ctx context.Context, r chat.Ref, name string) error {
	err := w.Platform.Unreact(ctx, r, name)
	w.check(chat.OpReact, err)
	return err
}

func (w authWatch) IsMember(ctx context.Context, channel, user string) (bool, error) {
	ok, err := w.Platform.IsMember(ctx, channel, user)
	w.check(chat.OpOther, err)
	return ok, err
}

func (w authWatch) IsInAnyGroup(ctx context.Context, groups []string, user string) (bool, error) {
	ok, err := w.Platform.IsInAnyGroup(ctx, groups, user)
	w.check(chat.OpOther, err)
	return ok, err
}

func (w authWatch) GroupsReadable(ctx context.Context, groups []string) error {
	err := w.Platform.GroupsReadable(ctx, groups)
	w.check(chat.OpOther, err)
	return err
}

func (w authWatch) ChannelAccess(ctx context.Context, channel string) (chat.Access, error) {
	a, err := w.Platform.ChannelAccess(ctx, channel)
	w.check(chat.OpOther, err)
	return a, err
}

func (w authWatch) IsBot(ctx context.Context, user string) (bool, error) {
	ok, err := w.Platform.IsBot(ctx, user)
	w.check(chat.OpOther, err)
	return ok, err
}

func (w authWatch) ResolveUser(ctx context.Context, id string) (chat.User, error) {
	u, err := w.Platform.ResolveUser(ctx, id)
	w.check(chat.OpOther, err)
	return u, err
}

func (w authWatch) MarkBusy(ctx context.Context, t chat.Target, busy bool) error {
	err := w.Platform.MarkBusy(ctx, t, busy)
	w.check(chat.OpOther, err)
	return err
}

func (w authWatch) ThreadBetween(ctx context.Context, t chat.Target, after, before string, limit int) ([]chat.HistoryMessage, int, error) {
	msgs, dropped, err := w.Platform.ThreadBetween(ctx, t, after, before, limit)
	w.check(chat.OpOther, err)
	return msgs, dropped, err
}

func (w authWatch) check(op chat.Op, err error) {
	if err == nil {
		return
	}
	if v := w.Classify(op, err); v.Class == chat.FatalAuth {
		w.report(v.Code)
	}
}

// watchedStream is a chat.Stream whose calls are watched as the platform's are.
type watchedStream struct {
	chat.Stream
	w authWatch
}

func (s watchedStream) Append(ctx context.Context, markdown string) error {
	err := s.Stream.Append(ctx, markdown)
	s.w.check(chat.OpPost, err)
	return err
}

func (s watchedStream) Step(ctx context.Context, st chat.Step) error {
	err := s.Stream.Step(ctx, st)
	s.w.check(chat.OpPost, err)
	return err
}

func (s watchedStream) Stop(ctx context.Context, markdown string, final chat.Message, tag string) error {
	err := s.Stream.Stop(ctx, markdown, final, tag)
	s.w.check(chat.OpPost, err)
	return err
}

// stopped is the error of a Slack run whose context has ended: the context's cause if a call
// reported a rejection (WithAuthFailure), which is a failure, and otherwise a shutdown, wrapping
// the context's error.
func stopped(ctx context.Context) error {
	if cause := context.Cause(ctx); errors.Is(cause, ErrSlackAuth) {
		return cause
	}
	return fmt.Errorf("slack run stopped: %w", ctx.Err())
}

// WithAuthFailure returns a context for RunPlatform that ends, with ErrSlackAuth as its cause, when
// report is called with a fatal code. RunPlatform then returns that cause, an error that wraps ErrSlackAuth and keeps the code, so that
// the run ends as for a rejected connection, and not as for a shutdown: the own connection's ends the process, and a team connection's
// ends that connection. stop releases the context. report may be called from any goroutine, and only the first call counts.
func WithAuthFailure(parent context.Context) (ctx context.Context, report func(code string), stop context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	return ctx, func(code string) {
		cancel(&authRejected{code: code, via: "reported by a call"})
	}, func() { cancel(nil) }
}
