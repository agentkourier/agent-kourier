package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// MaxLimit is the largest batch size (the limit argument of PurgeAudit, PurgeExpiredSessions and ListStaleTasks) the
// store accepts. A larger one is ErrInvalid, whichever store holds the data: Postgres binds a limit as 32 bits, and a
// batch of two billion rows is a bug in the caller, not a request. ListAudit has a bound of its own, 1 to 1000.
const MaxLimit = math.MaxInt32

// Adapter is what a store backend implements: the whole of Store. An adapter renders operations as SQL and runs them in
// its transactions; it can rely on every argument it is given being legal, and it records no telemetry, because Guard
// does both once, for every backend.
type Adapter = Store

// Guarded is the store seam: a Store that checks every argument and records every operation's telemetry once, over
// an Adapter that does the storing. The checks run before the adapter is called, so an invalid call never reaches it;
// the span and the duration observation of an operation cover the checks and the adapter's work, and end with the
// bounded outcome that its error gives (see finish). Guarded opens no transaction: it sits outside the adapter's, so the adapter's
// own rules (Postgres's epoch fence, SKIP LOCKED) run unchanged inside what it calls.
//
// A backend's Open returns a type that embeds *Guarded, so that what it returns is always the guarded store and the
// bare adapter is out of reach. Close and ClaimReplyInteraction are not operations of the store's telemetry (neither
// has ever had a span): Close passes straight through, and the claim is checked and scrubbed like the rest.
type Guarded struct {
	a      Adapter
	scrub  func(error) error
	noRows []error
}

var _ Store = (*Guarded)(nil)

// GuardOption changes how Guard sets up a Guarded.
type GuardOption func(*Guarded)

// WithScrub sets a function that every error from the adapter passes through before it is recorded as an outcome and
// returned: the Postgres store uses it to take its DSN's secrets out of an error's text. It must keep errors.Is and
// errors.As working, so that the outcome can still be classified.
func WithScrub(f func(error) error) GuardOption {
	return func(g *Guarded) {
		if f != nil {
			g.scrub = f
		}
	}
}

// WithNoRows names an error that the backend's driver gives for a missing row, such as sql.ErrNoRows: it counts as
// ErrNotFound when an operation's outcome is classified. The adapters turn a missing row into ErrNotFound themselves;
// this keeps a driver error that slips past one from being counted as a failure.
func WithNoRows(errs ...error) GuardOption {
	return func(g *Guarded) { g.noRows = append(g.noRows, errs...) }
}

// Guard returns the guarded store over a.
func Guard(a Adapter, opts ...GuardOption) *Guarded {
	g := &Guarded{a: a, scrub: func(err error) error { return err }}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Close closes the adapter. It may be called more than once, as Store promises.
func (g *Guarded) Close() error { return g.a.Close() }

// ClaimReplyInteraction implements ReplyInteractionClaimer. It checks its arguments and passes the error through
// scrub, and records no telemetry, as it never has.
func (g *Guarded) ClaimReplyInteraction(ctx context.Context, id, interaction string) error {
	if id == "" || interaction == "" {
		return g.scrub(fmt.Errorf("claim reply interaction: an ID and an interaction are required: %w", ErrInvalid))
	}
	return g.scrub(g.a.ClaimReplyInteraction(ctx, id, interaction))
}

// run runs fn as the operation name: a span and a duration observation that end with the outcome of the error fn
// returns, after scrub. fn receives the context of the span and does its argument checks first, so that a refused call
// is an operation with the outcome "invalid", as it always has been.
func (g *Guarded) run(ctx context.Context, name string, fn func(ctx context.Context) error) (err error) {
	ctx, op := telemetry.Start(ctx, name)
	defer func() {
		err = g.scrub(err)
		g.finish(op, err)
	}()
	return fn(ctx)
}

// finish ends op with a bounded outcome: the store's errors name theirs, and any other error is classified as every
// operation's is.
func (g *Guarded) finish(op *telemetry.Operation, err error) {
	switch {
	case errors.Is(err, ErrNotFound), g.isNoRows(err):
		op.FinishOutcome(nil, "not_found")
	case errors.Is(err, ErrConflict):
		op.FinishOutcome(nil, "conflict")
	case errors.Is(err, ErrExpired):
		op.FinishOutcome(nil, "expired")
	case errors.Is(err, ErrLimit):
		op.FinishOutcome(nil, "limited")
	case errors.Is(err, ErrInvalid):
		op.FinishOutcome(nil, "invalid")
	default:
		op.Finish(err)
	}
}

func (g *Guarded) isNoRows(err error) bool {
	for _, e := range g.noRows {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// stamp gives a row the trace of the call, unless the caller supplied one: a row that carries TraceParent keeps it.
func stamp(ctx context.Context, parent, state *string) {
	if *parent == "" {
		*parent, *state = telemetry.Capture(ctx)
	}
}

// validKey reports whether k names a conversation: all three parts are set.
func validKey(k ConversationKey) bool {
	return k.Connection != "" && k.Channel != "" && k.Thread != ""
}

// validTime reports whether t is a time that both stores can keep as one that is set: after the Unix epoch and no later
// than time.Unix(0, math.MaxInt64), April 2262. The lower bound is because SQLite stores 0 for a time never set, so the
// zero time and anything at or before the epoch cannot be told from it. The upper bound is because SQLite keeps a time
// as nanoseconds in 64 bits: a later time wraps and reads back as an earlier one (2300-01-01 as 1715), though Postgres
// could hold it. Both stores refuse what only one can keep, where a row's "unset" reads as 0 (the times of outbox
// abandonment, trigger runs and threads, and votes).
func validTime(t time.Time) bool {
	return t.After(time.Unix(0, 0)) && !t.After(time.Unix(0, math.MaxInt64))
}

// validLimit reports whether n is a batch size the store accepts: 1 to MaxLimit.
func validLimit(n int) bool { return n >= 1 && n <= MaxLimit }
