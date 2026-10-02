// Package outbox posts to a chat platform once, even across a crash (spec 8.6). A post is written to the
// store before it is sent, as a message in the neutral model (chat.Outgoing, versioned), tagged so that the
// platform can find it again, and its message ref is recorded after. A row with no ref may have been posted, so
// it is looked up before it is posted again. The platform renders the message at delivery, and the outbox knows
// nothing of its markup.
//
// Where a caller can wait for the platform, such as the renderer's own goroutine, it uses Send and gets the
// message's Ref. Where it cannot, such as the session's Notice, which runs on the chat platform's
// acknowledgement path, it uses Enqueue, a local write, and Run delivers the row. A notice's source ID must come
// from where it is, not from the event that carried it: the Expired and BindingGone notices for a mention take
// the message's (channel, ts), because a mention arrives as two events and both reach Submit, and one ID makes
// them one row and one post.
//
// A reaction (EnqueueReaction) is a row too, with the role reaction: durable and made once per ID, with no lookup,
// since making it twice is harmless (a reaction that is there already is success) and a reaction has no message of
// its own to find. Its ref is the ID of the message it was made on.
//
// A post that can never succeed is abandoned, not retried for ever: what the platform says is permanent (Classify:
// a channel that is gone, text too long, content it refuses), a message that is not valid or a stored payload that
// is not one of this version (a legacy Slack payload, which is not migrated), and a lookup that has timed out five
// times in a row. The row keeps its place and loses its text, Config.OnAbandon is called once with the reason, one
// of the neutral reasons (chat.Reason) or an Abandon constant, and one warning is logged, with the platform's own
// code; the store keeps neither. A row still unposted after Config.Retention loses its text the same way. Send of
// an abandoned row returns ErrAbandoned and never posts it again; a producer that wants the same words posted anew
// needs a new source ID.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// ErrAbandoned is returned for a post that courier has given up on, and wraps its cause.
var ErrAbandoned = errors.New("outbox: the post was abandoned")

// The reasons OnAbandon is given besides the neutral reasons of chat.Reason.
const (
	AbandonInvalid       = "invalid"        // the message cannot be posted at all, or the stored payload cannot be read
	AbandonLookupTimeout = "lookup_timeout" // looking for it keeps timing out, with the platform otherwise working
	AbandonExpired       = "expired"        // it was still unposted after Config.Retention
)

// storeWriteTimeout bounds the store writes that must outlive a cancelled context (recording a ref,
// abandoning a row). It is not CallTimeout, which is for calls to the platform: a test that sets that to
// a few milliseconds must not make a local write time out under load.
const storeWriteTimeout = 10 * time.Second

// lookupTimeouts is how many lookups of a row in a row must time out, over at least
// Config.LookupTimeoutFloor and with some other platform call working in between, before it is
// abandoned.
const lookupTimeouts = 5

// Platform is what the outbox asks of the chat platform: chat.Platform, or a wrapper of it.
type Platform interface {
	// Post posts o tagged with tag and returns where it landed. Its error does not say whether the message
	// was posted.
	Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error)
	// Find returns the message tagged with tag, posted no earlier than about since.
	Find(ctx context.Context, t chat.Target, tag string, since time.Time) (chat.Ref, bool, error)
	// React puts a reaction on a message. One that is there already is success.
	React(ctx context.Context, r chat.Ref, name string) error
	// RefAt is the Ref of the message a row recorded by its ID alone.
	RefAt(channel, thread, id string) chat.Ref
	// Fits says whether a prompt can be shown in one message, which is checked before it is stored.
	Fits(p chat.Prompt) error
	// Classify says what an error of a call means: permanent errors abandon the row.
	Classify(op chat.Op, err error) chat.Verdict
}

// Config configures an Outbox. Store and Platform are required.
type Config struct {
	Store    store.Outbox
	Platform Platform
	Now      func() time.Time // time.Now if nil
	Logger   *slog.Logger     // slog.Default() if nil
	// CallTimeout bounds each Post, Find and React (default 30 seconds), because the delivery lock is
	// held across them and an unbounded call would hold it for good.
	CallTimeout time.Duration
	// Interval is how often Run drains with no signal (default 30 seconds).
	Interval time.Duration
	// RetryBase is the delay before a row whose delivery failed is tried again (default 5 seconds).
	// It doubles with each failure up to RetryMax (default 5 minutes).
	RetryBase, RetryMax time.Duration
	// Retention is how long a post may stay unposted before its text is dropped and it is
	// abandoned (default 7 days).
	Retention time.Duration
	// LookupTimeoutFloor is how long the lookups of a row must have been timing out, from the first
	// of five in a row, before it is abandoned for them (default 1 hour). The rule is for a lookup
	// that can never finish, not for an outage that hangs every request: so it also needs some
	// other platform call to have worked since the first timeout.
	LookupTimeoutFloor time.Duration
	// PurgeInterval is how often Reconcile drops the text of expired rows (default 5 minutes).
	PurgeInterval time.Duration
	// OnAbandon, if set, is called once for each row that is abandoned, with its ID and the reason:
	// a neutral reason (chat.Reason) or AbandonInvalid, AbandonLookupTimeout or AbandonExpired. It is for a
	// metric, and runs under the delivery lock, so it must not block.
	OnAbandon func(id, reason string)
}

// Post is one message to deliver. Source and Role give its ID: Source is the event, turn or
// interaction it belongs to, and must differ for each message of one source and role.
type Post struct {
	Source            string
	Role              store.OutboxRole
	Channel, ThreadTS string // ThreadTS is empty for a message in the channel
	// Out is what to say, in the neutral model. It is stored as it is, so it must be valid (Validate), and a
	// prompt must fit (Platform.Fits): one that does not is store.ErrInvalid, which no retry mends.
	Out chat.Outgoing
}

// ID returns the outbox ID of a message: deterministic, so a retry of the same event names the
// same row. An interaction prompt passes its interaction ID as source, so it cannot collide with
// an agent reply (spec 8.6 step 5).
func ID(source string, role store.OutboxRole) string { return source + "/" + string(role) }

// Outbox delivers posts. It is safe for concurrent use.
type Outbox struct {
	cfg Config
	// mu allows one delivery at a time. A post's outcome is unknown until its ref is recorded, so
	// two deliveries of one row could post it twice.
	mu    sync.Mutex
	retry map[string]retryState // rows whose delivery failed; guarded by mu
	// platformOK is when a call to the platform last worked, and lastPurge when the retention purge last ran.
	// Both are guarded by mu.
	platformOK, lastPurge time.Time
	wake                  chan struct{} // Enqueue's signal to Run; one slot, since a drain takes every row
}

// retryState is how often a row's delivery has failed in a row, and when it may be tried again.
type retryState struct {
	fails    int
	timeouts int // lookups in a row that timed out
	// firstTimeout is when the first of them did.
	firstTimeout time.Time
	next         time.Time
}

// New returns an Outbox over cfg.
func New(cfg Config) *Outbox {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 30 * time.Second
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 5 * time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 5 * time.Minute
	}
	if cfg.LookupTimeoutFloor <= 0 {
		cfg.LookupTimeoutFloor = time.Hour
	}
	if cfg.PurgeInterval <= 0 {
		cfg.PurgeInterval = 5 * time.Minute
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 7 * 24 * time.Hour
	}
	return &Outbox{cfg: cfg, wake: make(chan struct{}, 1), retry: map[string]retryState{}}
}

// Send writes the row, posts the message and records its ID, and returns where it is. A message that
// is already recorded is returned without a call to the platform. A row that exists without a ref, left
// by a failed post or a crash, is looked up first and posted only if the platform has no such message.
// A new row is posted at once. If the lookup fails nothing is posted, and the error is returned.
func (o *Outbox) Send(ctx context.Context, p Post) (chat.Ref, error) {
	row, err := o.row(p)
	if err != nil {
		return chat.Ref{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	created, err := o.cfg.Store.CreateOutbox(ctx, row)
	if err != nil {
		return chat.Ref{}, err
	}
	ref, err := o.deliver(ctx, row.ID, !created)
	if err == nil {
		o.settle(row.ID, nil) // a row Run was backing off is delivered; forget it
	}
	return ref, err
}

// row returns the outbox row for p, with the message as its payload. A message that is not valid, or a prompt that
// does not fit, is ErrInvalid: such a row would never be delivered, and would hold its ID for good.
func (o *Outbox) row(p Post) (store.OutboxEntry, error) {
	row := store.OutboxEntry{ID: ID(p.Source, p.Role), EventID: p.Source, Role: p.Role, Channel: p.Channel, ThreadTS: p.ThreadTS, CreatedAt: o.cfg.Now()}
	if p.Role == store.OutboxReaction {
		return store.OutboxEntry{}, fmt.Errorf("outbox post %s: a reaction is not a post, use EnqueueReaction: %w", row.ID, store.ErrInvalid)
	}
	if err := p.Out.Validate(); err != nil { // no retry mends a message that cannot be shown
		return store.OutboxEntry{}, fmt.Errorf("outbox post %s: %w: %w", row.ID, store.ErrInvalid, err)
	}
	if p.Out.Prompt != nil {
		if err := o.cfg.Platform.Fits(*p.Out.Prompt); err != nil {
			return store.OutboxEntry{}, fmt.Errorf("outbox post %s: %w: %w", row.ID, store.ErrInvalid, err)
		}
	}
	payload, err := json.Marshal(p.Out)
	if err != nil {
		return store.OutboxEntry{}, fmt.Errorf("outbox post %s: %w", row.ID, err)
	}
	row.Payload = string(payload)
	return row, nil
}

// Enqueue writes the row for p and returns its ID, and does nothing else: no platform call, so it is
// safe where the platform waits for an answer, such as the session's Notice. Run delivers the row. The
// same ID enqueued twice is one row and one post. A message that is not valid is ErrInvalid.
func (o *Outbox) Enqueue(ctx context.Context, p Post) (string, error) {
	row, err := o.row(p)
	if err != nil {
		return "", err
	}
	return o.enqueue(ctx, row)
}

// enqueue writes row and wakes Run.
func (o *Outbox) enqueue(ctx context.Context, row store.OutboxEntry) (string, error) {
	if _, err := o.cfg.Store.CreateOutbox(ctx, row); err != nil {
		return "", err
	}
	select {
	case o.wake <- struct{}{}:
	default: // a signal is already waiting, and the drain it starts will take this row too
	}
	return row.ID, nil
}

// Reaction is a reaction to make on the message TS in Channel: Name is the bare emoji name. Source
// gives its row's ID, which is ID(Source, store.OutboxReaction), and must differ for each reaction
// of one source.
type Reaction struct {
	Source, Channel, TS, Name string
}

// reactionPayload is a reaction row's payload.
type reactionPayload struct {
	TS   string `json:"ts"`
	Name string `json:"name"`
}

// EnqueueReaction writes the row for r and returns its ID, and does nothing else: no platform call, so
// it is safe where the platform waits for an answer. Run delivers the row. The same ID enqueued twice is
// one row and one reaction. An empty field is ErrInvalid.
func (o *Outbox) EnqueueReaction(ctx context.Context, r Reaction) (string, error) {
	id := ID(r.Source, store.OutboxReaction)
	if r.Source == "" || r.Channel == "" || r.TS == "" || r.Name == "" {
		return "", fmt.Errorf("outbox reaction %s: the source, channel, ts and name are required: %w", id, store.ErrInvalid)
	}
	payload, err := json.Marshal(reactionPayload{TS: r.TS, Name: r.Name})
	if err != nil {
		return "", fmt.Errorf("outbox reaction %s: %w", id, err)
	}
	return o.enqueue(ctx, store.OutboxEntry{ID: id, EventID: r.Source, Role: store.OutboxReaction, Channel: r.Channel, CreatedAt: o.cfg.Now(), Payload: string(payload)})
}

// Run drains the outbox until ctx ends, and returns its error. It drains at once, which recovers
// what a crash left, then on each Enqueue and every Interval. A row that fails is tried again after
// a delay that doubles from RetryBase up to RetryMax, so one that always fails does not spin. Call
// it once.
func (o *Outbox) Run(ctx context.Context) error {
	tick := time.NewTicker(o.cfg.Interval)
	defer tick.Stop()
	for {
		if err := o.Reconcile(ctx); err != nil && ctx.Err() == nil {
			o.cfg.Logger.WarnContext(ctx, "outbox: some posts were not delivered; they will be tried again", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-o.wake:
		case <-tick.C:
		}
	}
}

// settle records the outcome of delivering a row: a failure sets the delay before its next try, a
// success forgets the row. The caller holds o.mu.
func (o *Outbox) settle(id string, err error) {
	if err == nil || errors.Is(err, ErrAbandoned) {
		delete(o.retry, id)
		return
	}
	r := o.retry[id]
	r.fails++
	delay := o.cfg.RetryBase
	for i := 1; i < r.fails && delay < o.cfg.RetryMax; i++ {
		delay *= 2
	}
	r.next = o.cfg.Now().Add(min(delay, o.cfg.RetryMax))
	o.retry[id] = r
}

// Reconcile delivers every row that has a payload and no ref, oldest first, looking each up
// before it posts: it is for startup, after a crash may have left posts unrecorded, and is one
// pass of Run. A row with no payload is not one of Send's or Enqueue's (they refuse an invalid
// message): it is a stream's, whose renderer records the ref itself, and is left alone, as is a
// row still waiting out its retry delay. The retention purge runs in it at most every
// PurgeInterval. It goes on past a row it cannot reconcile, and returns every such error joined.
func (o *Outbox) Reconcile(ctx context.Context) error {
	var errs []error
	now := o.cfg.Now()
	// The purge comes before any lookup of the rows it takes, and under the lock, so that no row is
	// purged while its post is in flight.
	o.mu.Lock()
	if o.lastPurge.IsZero() || now.Sub(o.lastPurge) >= o.cfg.PurgeInterval { // a write, so not on every pass
		expired, err := o.cfg.Store.AbandonOutboxBefore(ctx, now.Add(-o.cfg.Retention), now)
		if err != nil {
			errs = append(errs, fmt.Errorf("drop expired outbox text: %w", err))
		} else {
			o.lastPurge = now
		}
		for _, id := range expired {
			delete(o.retry, id)
			o.reportAbandon(ctx, id, AbandonExpired, "", nil)
		}
	}
	o.mu.Unlock()
	rows, err := o.cfg.Store.ListUnposted(ctx)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("reconcile outbox: %w", err))...)
	}
	for _, r := range rows {
		if r.Payload == "" {
			continue
		}
		o.mu.Lock()
		if next, ok := o.retry[r.ID]; ok && o.cfg.Now().Before(next.next) {
			o.mu.Unlock()
			continue
		}
		_, err := o.deliver(ctx, r.ID, true)
		o.settle(r.ID, err)
		o.mu.Unlock()
		if !errors.Is(err, ErrAbandoned) { // an abandon is reported where it happens
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// target is where a row's message goes.
func target(row store.OutboxEntry) chat.Target {
	return chat.Target{Channel: row.Channel, Thread: row.ThreadTS}
}

// permanent says whether err, of a call of the kind op, is one that no retry mends, and if so why: the neutral
// reason and the platform's own code.
func (o *Outbox) permanent(op chat.Op, err error) (reason, code string, ok bool) {
	v := o.cfg.Platform.Classify(op, err)
	if v.Class != chat.Permanent {
		return "", "", false
	}
	return string(orOther(v.Reason)), v.Code, true
}

// orOther is r, or chat.ReasonOther for a permanent error that gave none.
func orOther(r chat.Reason) chat.Reason {
	if r == chat.ReasonNone {
		return chat.ReasonOther
	}
	return r
}

// decode reads the payload of a row as the message it holds. A payload that is not a message of this version is
// an error, such as a legacy Slack payload (Text and Blocks, with no version), which is not migrated.
func decode(payload string) (chat.Outgoing, error) {
	var out chat.Outgoing
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return chat.Outgoing{}, err
	}
	if out.V != chat.OutgoingVersion {
		return chat.Outgoing{}, fmt.Errorf("%w: the payload is of version %d, not %d: a legacy payload, which is not migrated", chat.ErrInvalid, out.V, chat.OutgoingVersion)
	}
	if err := out.Validate(); err != nil {
		return chat.Outgoing{}, err
	}
	return out, nil
}

// deliver posts the row id unless it already has a ref, which it returns. With lookup it first
// asks the platform for the message, and adopts it if it is there. The caller holds o.mu. The row is read
// again here, so a delivery that finished while the caller waited for the lock is seen.
func (o *Outbox) deliver(ctx context.Context, id string, lookup bool) (chat.Ref, error) {
	row, err := o.cfg.Store.GetOutbox(ctx, id)
	if err != nil {
		return chat.Ref{}, err
	}
	if row.MessageRef != "" {
		return o.cfg.Platform.RefAt(row.Channel, row.ThreadTS, row.MessageRef), nil
	}
	if !row.AbandonedAt.IsZero() {
		return chat.Ref{}, fmt.Errorf("deliver %s: %w", id, ErrAbandoned)
	}
	if row.Role == store.OutboxReaction {
		return o.react(ctx, row)
	}
	if row.Payload == "" {
		return chat.Ref{}, fmt.Errorf("deliver %s: the row has no payload: %w", id, store.ErrInvalid)
	}
	out, err := decode(row.Payload)
	if err != nil {
		return chat.Ref{}, o.abandon(ctx, row, AbandonInvalid, "", fmt.Errorf("deliver %s: payload: %w", id, err))
	}
	if lookup {
		ref, found, err := o.find(ctx, row)
		if err == nil {
			o.platformOK = o.cfg.Now()
		} else if reason, code, ok := o.permanent(chat.OpPost, err); ok { // as for a post: it will not pass
			return chat.Ref{}, o.abandon(ctx, row, reason, code, fmt.Errorf("deliver %s: looking for the message: %w", id, err))
		}
		timedOut := err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
		if o.lookupGivesUp(id, timedOut) {
			return chat.Ref{}, o.abandon(ctx, row, AbandonLookupTimeout, "", fmt.Errorf("deliver %s: looking for the message: %w", id, err))
		}
		if err != nil {
			return chat.Ref{}, fmt.Errorf("deliver %s: looking for the message before posting it: %w", id, err)
		}
		if found {
			o.cfg.Logger.InfoContext(ctx, "outbox: adopted a message posted before a crash", "outbox_id", id, "channel", row.Channel, "ts", ref.ID)
			return o.record(ctx, id, ref)
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, o.cfg.CallTimeout)
	defer cancel()
	ref, err := o.cfg.Platform.Post(callCtx, target(row), out, id)
	if err != nil {
		err = fmt.Errorf("deliver %s: post: %w", id, err)
		if reason, code, ok := o.permanent(chat.OpPost, err); ok {
			return chat.Ref{}, o.abandon(ctx, row, reason, code, err)
		}
		if errors.Is(err, store.ErrInvalid) || errors.Is(err, chat.ErrInvalid) {
			return chat.Ref{}, o.abandon(ctx, row, AbandonInvalid, "", err)
		}
		return chat.Ref{}, err
	}
	o.platformOK = o.cfg.Now()
	testhooks.AfterPost() // a no-op unless this is a test image, where it can crash the process here
	return o.record(ctx, id, ref)
}

// react makes the reaction of row and records the ID of its message as the ref. There is no lookup:
// a reaction made twice is made once, and one made before a crash is simply found there. The caller
// holds o.mu.
func (o *Outbox) react(ctx context.Context, row store.OutboxEntry) (chat.Ref, error) {
	var p reactionPayload
	if err := json.Unmarshal([]byte(row.Payload), &p); err != nil || p.TS == "" || p.Name == "" {
		return chat.Ref{}, o.abandon(ctx, row, AbandonInvalid, "", fmt.Errorf("deliver %s: the reaction's payload is not usable: %w", row.ID, store.ErrInvalid))
	}
	ref := o.cfg.Platform.RefAt(row.Channel, "", p.TS)
	callCtx, cancel := context.WithTimeout(ctx, o.cfg.CallTimeout)
	defer cancel()
	if err := o.cfg.Platform.React(callCtx, ref, p.Name); err != nil {
		err = fmt.Errorf("deliver %s: react: %w", row.ID, err)
		if reason, code, ok := o.permanent(chat.OpReact, err); ok {
			return chat.Ref{}, o.abandon(ctx, row, reason, code, err)
		}
		if errors.Is(err, store.ErrInvalid) || errors.Is(err, chat.ErrInvalid) {
			return chat.Ref{}, o.abandon(ctx, row, AbandonInvalid, "", err)
		}
		return chat.Ref{}, err
	}
	o.platformOK = o.cfg.Now()
	return o.record(ctx, row.ID, ref)
}

// lookupGivesUp notes how a lookup of id ended and reports whether the row should now be abandoned
// for its lookups: a timeout adds one to the count in a row, and any other end starts it again.
// It needs five, spread over LookupTimeoutFloor from the first, and some other platform call that has
// worked since that first one; otherwise it is the platform that is down, not this lookup that cannot
// finish. The caller holds o.mu.
func (o *Outbox) lookupGivesUp(id string, timedOut bool) bool {
	r, ok := o.retry[id]
	if !timedOut {
		if ok {
			r.timeouts, r.firstTimeout = 0, time.Time{}
			o.retry[id] = r
		}
		return false
	}
	now := o.cfg.Now()
	if r.timeouts == 0 {
		r.firstTimeout = now
	}
	r.timeouts++
	o.retry[id] = r
	return r.timeouts >= lookupTimeouts && now.Sub(r.firstTimeout) >= o.cfg.LookupTimeoutFloor && o.platformOK.After(r.firstTimeout)
}

// abandon gives up on the row, which will never post, and returns an error that wraps ErrAbandoned
// and cause. If the store cannot record it, the row is still work and that error is returned alone.
// code is the platform's own code for a permanent error, for the log and nothing else. The caller holds o.mu.
func (o *Outbox) abandon(ctx context.Context, row store.OutboxEntry, reason, code string, cause error) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()
	newly, err := o.cfg.Store.AbandonOutbox(wctx, row.ID, o.cfg.Now())
	if err != nil {
		return fmt.Errorf("abandon %s (%s): %w", row.ID, reason, err)
	}
	delete(o.retry, row.ID)
	if newly {
		o.reportAbandon(ctx, row.ID, reason, code, cause)
	}
	return fmt.Errorf("%w (%s): %w", ErrAbandoned, reason, cause)
}

// reportAbandon logs one warning, never the text, and calls OnAbandon. The platform's code is in the log and
// not in the call, so that a metric's labels are the closed set of reasons.
func (o *Outbox) reportAbandon(ctx context.Context, id, reason, code string, cause error) {
	attrs := []any{"outbox_id", id, "reason", reason}
	if code != "" {
		attrs = append(attrs, "code", code)
	}
	if cause != nil { // an expired row has none
		attrs = append(attrs, "err", cause)
	}
	o.cfg.Logger.WarnContext(ctx, "outbox: a post was abandoned and will not be retried", attrs...)
	if o.cfg.OnAbandon != nil {
		o.cfg.OnAbandon(id, reason)
	}
}

// find looks for the row's message, within the call timeout.
func (o *Outbox) find(ctx context.Context, row store.OutboxEntry) (chat.Ref, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, o.cfg.CallTimeout)
	defer cancel()
	return o.cfg.Platform.Find(ctx, target(row), row.ID, row.CreatedAt)
}

// record stores the message's ID on the row and returns its Ref. The message is already on the platform, so it
// is recorded even if ctx has ended (a shutdown right after the post), within storeWriteTimeout; a
// failure here leaves it for the next lookup to adopt.
func (o *Outbox) record(ctx context.Context, id string, ref chat.Ref) (chat.Ref, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()
	if err := o.cfg.Store.RecordMessageRef(ctx, id, ref.ID); err != nil {
		return chat.Ref{}, fmt.Errorf("deliver %s: record the message ref: %w", id, err)
	}
	return ref, nil
}
