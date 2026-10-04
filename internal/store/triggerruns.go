package store

import (
	"context"
	"time"
)

// TriggerLimit names the limit that stopped a start (spec 8.2). It is the limit label of
// courier_trigger_limited_total.
type TriggerLimit string

// The limits of a trigger. When more than one is reached the daily cap is reported first, then the rate, then the
// concurrency: the one that lasts longest says most about why nothing starts.
const (
	// LimitDaily: the trigger started its maxRunsPerDay in the last 24 hours.
	LimitDaily TriggerLimit = "daily"
	// LimitRate: the trigger started its rateLimit.maxRuns in the last rateLimit.per.
	LimitRate TriggerLimit = "rate"
	// LimitConcurrent: rateLimit.maxConcurrent of the trigger's investigations are running.
	LimitConcurrent TriggerLimit = "concurrent"
)

// Valid reports whether l is one of the defined limits.
func (l TriggerLimit) Valid() bool {
	switch l {
	case LimitDaily, LimitRate, LimitConcurrent:
		return true
	}
	return false
}

// TriggerRun is one alert message that a botMessage trigger decided to start an investigation for. The alert's own
// message is the thread of the session it starts, so Key names both.
type TriggerRun struct {
	// Binding is the namespace/name of the Binding, and Trigger the trigger's place in its chat.triggers.
	Binding string
	Trigger int
	// Key is the alert's message: its connection, channel and ts as the thread.
	Key ConversationKey
	// At is when the start was decided. The windows of the limits end here.
	At time.Time
}

// TriggerRunLimits are the limits to check a start against. A zero field is not checked.
type TriggerRunLimits struct {
	// MaxRuns starts are allowed in any Per, and MaxConcurrent investigations running at once.
	MaxRuns       int
	Per           time.Duration
	MaxConcurrent int
	// MaxPerDay starts are allowed in any 24 hours.
	MaxPerDay int
	// DigestWindow is how long the digest line of a start that is stopped covers.
	DigestWindow time.Duration
}

// TriggerAdmission is the answer to a start: whether it may go ahead, and if not the limit that stopped it.
type TriggerAdmission struct {
	Admitted bool
	// Limit is the limit that stopped the start, empty if it was admitted.
	Limit TriggerLimit
}

// TriggerDigest is one window of starts that limits stopped, and the count of them by limit: what a digest line says
// (spec 8.1 step 4). It names no alert.
type TriggerDigest struct {
	Binding string
	Trigger int
	// Connection and Channel are where the digest is posted: the channel the alerts were posted to.
	Connection, Channel string
	// Start and End bound the window. It opens with the first stopped start after the last digest and lasts the
	// trigger's rate window, so one burst is not cut in two by a boundary.
	Start, End time.Time
	// Counts is the number of stopped starts in the window, by limit.
	Counts map[TriggerLimit]int
}

// TriggerRuns persists the starts of the botMessage triggers, so that their limits (spec 8.2, D26, D31) hold across
// a restart and are counted durably, and the digest of the starts that were stopped.
type TriggerRuns interface {
	// AdmitTriggerRun decides whether the start run may go ahead, and records it, in one atomic step: it checks the
	// limits against the starts the trigger made before, and if none is reached records the start, and otherwise
	// records that it was stopped, for the digest. A start is one row per message, so asking again for a message
	// that was admitted already is admitted again without being counted twice. A message that was stopped is checked
	// afresh: capacity may have come back. A stopped message already recorded keeps its row and window. Binding,
	// Key and At are required, and At after the Unix epoch (ErrInvalid otherwise).
	//
	// The limits count the starts of the Binding's trigger. Rate and daily count the starts in the window before
	// run.At: they slide. Concurrent counts the admitted starts whose session has an active task or an undelivered
	// turn, which is how a start that was queued and has not begun is a running investigation too.
	AdmitTriggerRun(ctx context.Context, run TriggerRun, limits TriggerRunLimits) (TriggerAdmission, error)
	// ListTriggerDigests returns the windows of stopped starts that ended at or before now and have not been
	// digested, oldest first, with their counts by limit.
	ListTriggerDigests(ctx context.Context, now time.Time) ([]TriggerDigest, error)
	// MarkTriggerDigested records that the digest of the window that began at start was queued, so that
	// ListTriggerDigests no longer returns it. It does nothing for a window with no undigested rows.
	MarkTriggerDigested(ctx context.Context, binding string, trigger int, start time.Time) error
	// PurgeTriggerRuns deletes the rows from before the cutoff, and returns how many. Pass a cutoff 24 hours before
	// now, the longest window any limit counts.
	PurgeTriggerRuns(ctx context.Context, before time.Time) (int, error)
}
