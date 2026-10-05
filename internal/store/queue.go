package store

import (
	"context"
	"time"
)

// Queue is the worker-facing side of the durable event queue (spec 6.1, D15).
//
// Delivery is at-least-once: a claimed event whose lease expires without
// CompleteEvent or FailEvent becomes claimable again. The store applies no
// retry policy; the worker chooses lease, retry time, and attempt limit.
//
// Each claim yields a fencing token, the event's Attempts. Only the current
// lease holder may change the event: CompleteEvent and FailEvent take the
// token and return ErrConflict if the event has been reclaimed since. A lease
// that expired but was never reclaimed is still held, so a late holder can
// still finish.
//
// NextAttemptAt is the retry time while an event is pending and the lease
// deadline while it is processing. It means nothing for done and dead events.
type Queue interface {
	// ClaimEvent atomically claims the oldest claimable event: pending with
	// NextAttemptAt <= now, or processing with an expired lease
	// (NextAttemptAt <= now). Of those, the one due earliest is taken: by NextAttemptAt, then by
	// ReceivedAt. The event becomes processing, its Attempts increases by one, and NextAttemptAt becomes now+lease, the lease
	// deadline. The returned Event reflects the claim. It returns ErrNotFound
	// when nothing is claimable and ErrInvalid for a zero now or a
	// non-positive lease.
	//
	// The store does not enforce an attempt limit, so after crashes a claim
	// can return an event with Attempts above the worker's maximum. The
	// caller must dead-letter such an event with FailEvent before running a
	// handler.
	ClaimEvent(ctx context.Context, now time.Time, lease time.Duration) (Event, error)
	// CompleteEvent marks done the processing event claimed as attempt. It
	// returns ErrNotFound if the event does not exist and ErrConflict if it
	// is not processing at that attempt.
	CompleteEvent(ctx context.Context, id string, attempt int) error
	// FailEvent records errText as the last error of the processing event
	// claimed as attempt. If attempt has reached maxAttempts the event becomes
	// dead; otherwise it becomes pending with NextAttemptAt = retryAt. Errors
	// are as for CompleteEvent, plus ErrInvalid for an attempt or maxAttempts
	// below one or a zero retryAt.
	FailEvent(ctx context.Context, id string, attempt int, retryAt time.Time, maxAttempts int, errText string) error
}
