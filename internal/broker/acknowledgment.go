package broker

import (
	"context"

	"github.com/agentkourier/agent-kourier/internal/session"
)

// unacknowledge is the session manager's Unacknowledge: it queues the removal n with queue (live.Sink.QueueNotice) and,
// once that is written, settles the source reply with settle (store.AcknowledgmentLister), so that retention does not
// list the reply and queue its removal again. A removal that was not queued is not settled: retention still finds it.
func unacknowledge(queue func(context.Context, session.Key, session.Notice) error, settle func(context.Context, []string) error) func(context.Context, session.Key, string, session.Notice) error {
	return func(ctx context.Context, key session.Key, source string, n session.Notice) error {
		if err := queue(ctx, key, n); err != nil {
			return err
		}
		return settle(ctx, []string{source})
	}
}
