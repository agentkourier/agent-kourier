package broker

import (
	"context"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/session"
)

// unacknowledge is the session manager's Unacknowledge: it queues the removal n with queue (live.Sink.QueueNotice) and,
// once that is written, settles the source reply with settle (store.AcknowledgmentLister), so that retention does not
// list the reply and queue its removal again. A removal that was not queued is not settled: retention still finds it. Its
// error says which of the two failed, for the manager logs it as it is.
func unacknowledge(queue func(context.Context, session.Key, session.Notice) error, settle func(context.Context, []string) error) func(context.Context, session.Key, string, session.Notice) error {
	return func(ctx context.Context, key session.Key, source string, n session.Notice) error {
		if err := queue(ctx, key, n); err != nil {
			return fmt.Errorf("queue the acknowledgment removal of %s: %w", source, err)
		}
		if err := settle(ctx, []string{source}); err != nil {
			return fmt.Errorf("settle %s, whose acknowledgment removal is queued: %w", source, err)
		}
		return nil
	}
}
