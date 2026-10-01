package e2e

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// WaitThread waits up to timeout for pred to hold of the thread rooted at ts in channel, root first,
// polling until then, and returns the thread it last read, which on a timeout is what the thread
// looked like. pred is not called for a thread that cannot be read: one that is not there yet, a
// root nobody has posted, is waited for. Like Wait it ends at once with ctx's error if ctx ends.
func (s *Slack) WaitThread(ctx context.Context, timeout time.Duration, channel, ts string, pred func([]fakeslack.MessageView) bool) ([]fakeslack.MessageView, error) {
	var last []fakeslack.MessageView
	err := Wait(ctx, timeout, "thread "+ts+" in "+channel+" to match", func(ctx context.Context) (bool, error) {
		thread, err := s.Thread(ctx, channel, ts)
		if err != nil {
			return false, err
		}
		last = thread
		return pred(thread), nil
	})
	return last, err
}

// Ephemerals returns the ephemeral messages courier sent in channel, oldest first. The fake shows
// them through this call alone, for e2e tests: no user, history or thread read does, as in Slack,
// where only the recipient sees one.
func (s *Slack) Ephemerals(ctx context.Context, channel string) ([]fakeslack.Ephemeral, error) {
	var out []fakeslack.Ephemeral
	err := s.c.do(ctx, http.MethodGet, "/_control/ephemerals?"+url.Values{"channel": {channel}}.Encode(), nil, &out)
	return out, err
}

// Reactions returns the reactions on the message ts in channel, in the order they were added, and
// an empty list if there are none. A message the fake does not hold is a 404.
func (s *Slack) Reactions(ctx context.Context, channel, ts string) ([]fakeslack.Reaction, error) {
	var out []fakeslack.Reaction
	err := s.c.do(ctx, http.MethodGet, "/_control/reactions?"+url.Values{"channel": {channel}, "ts": {ts}}.Encode(), nil, &out)
	return out, err
}
