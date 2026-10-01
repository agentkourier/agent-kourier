// Package outbox posts to Slack once, even across a crash (spec 8.6). A post is written to the
// store before it is sent, tagged so it can be found in Slack's history, and its message ref is
// recorded after. A row with no ref may have been posted, so it is looked up before it is posted
// again.
//
// Where a caller can wait for Slack, such as the renderer's own goroutine, it uses Send and gets the
// message's ts. Where it cannot, such as the session's Notice, which runs on Slack's acknowledgement
// path, it uses Enqueue, a local write, and Run delivers the row. A notice's source ID must come from
// where it is, not from the event that carried it: the Expired and BindingGone notices for a mention
// take the Slack message's (channel, ts), because a mention arrives as two events and both reach
// Submit, and one ID makes them one row and one post.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Poster is the Slack side of the outbox, which *slack.Client satisfies.
type Poster interface {
	// Post posts content tagged with outboxID and returns its ts. Its error does not say whether
	// the message was posted.
	Post(ctx context.Context, channel, threadTS string, content slack.Content, outboxID string) (string, error)
	// Find returns the ts of the message tagged with outboxID, posted no earlier than about since.
	Find(ctx context.Context, channel, threadTS, outboxID string, since time.Time) (ts string, found bool, err error)
}

// Config configures an Outbox. Store and Poster are required.
type Config struct {
	Store  store.Outbox
	Poster Poster
	Now    func() time.Time // time.Now if nil
	Logger *slog.Logger     // slog.Default() if nil
	// CallTimeout bounds each Post and Find (default 30 seconds), because the delivery lock is
	// held across them and an unbounded Slack call would hold it for good.
	CallTimeout time.Duration
	// Interval is how often Run drains with no signal (default 30 seconds).
	Interval time.Duration
	// RetryBase is the delay before a row whose delivery failed is tried again (default 5 seconds).
	// It doubles with each failure up to RetryMax (default 5 minutes).
	RetryBase, RetryMax time.Duration
}

// Post is one message to deliver. Source and Role give its ID: Source is the event, turn or
// interaction it belongs to, and must differ for each message of one source and role.
type Post struct {
	Source            string
	Role              store.OutboxRole
	Channel, ThreadTS string // ThreadTS is empty for a message in the channel
	Content           slack.Content
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
	wake  chan struct{}         // Enqueue's signal to Run; one slot, since a drain takes every row
}

// retryState is how often a row's delivery has failed in a row, and when it may be tried again.
type retryState struct {
	fails int
	next  time.Time
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
	return &Outbox{cfg: cfg, wake: make(chan struct{}, 1), retry: map[string]retryState{}}
}

// Send writes the row, posts the message and records its ts, and returns the ts. A message that
// is already recorded is returned without a call to Slack. A row that exists without a ref, left
// by a failed post or a crash, is looked up first and posted only if Slack has no such message.
// A new row is posted at once. If the lookup fails nothing is posted, and the error is returned.
func (o *Outbox) Send(ctx context.Context, p Post) (string, error) {
	if p.Content.Text == "" && len(p.Content.Blocks) == 0 {
		return "", fmt.Errorf("send %s: no text or blocks: %w", ID(p.Source, p.Role), store.ErrInvalid)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	row, err := o.row(p)
	if err != nil {
		return "", err
	}
	created, err := o.cfg.Store.CreateOutbox(ctx, row)
	if err != nil {
		return "", err
	}
	ts, err := o.deliver(ctx, row.ID, !created)
	if err == nil {
		o.settle(row.ID, nil) // a row Run was backing off is delivered; forget it
	}
	return ts, err
}

// row returns the outbox row for p, with the message as its payload. A post with no content has no
// payload, which marks a stream: Reconcile and Run never deliver it.
func (o *Outbox) row(p Post) (store.OutboxEntry, error) {
	row := store.OutboxEntry{ID: ID(p.Source, p.Role), EventID: p.Source, Role: p.Role, Channel: p.Channel, ThreadTS: p.ThreadTS, CreatedAt: o.cfg.Now()}
	if p.Content.Text != "" || len(p.Content.Blocks) > 0 {
		payload, err := json.Marshal(p.Content)
		if err != nil {
			return store.OutboxEntry{}, fmt.Errorf("outbox row %s: %w", row.ID, err)
		}
		row.Payload = string(payload)
	}
	return row, nil
}

// Enqueue writes the row for p and returns its ID, and does nothing else: no Slack call, so it is
// safe where Slack waits for an answer, such as the session's Notice. Run delivers the row. The
// same ID enqueued twice is one row and one post. A post with no content is a stream, which the
// renderer records itself and Run leaves alone.
func (o *Outbox) Enqueue(ctx context.Context, p Post) (string, error) {
	row, err := o.row(p)
	if err != nil {
		return "", err
	}
	if _, err := o.cfg.Store.CreateOutbox(ctx, row); err != nil {
		return "", err
	}
	select {
	case o.wake <- struct{}{}:
	default: // a signal is already waiting, and the drain it starts will take this row too
	}
	return row.ID, nil
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
	if err == nil {
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
// pass of Run. A row with no payload is a stream, whose renderer records the ref itself, and is
// left alone, as is a row still waiting out its retry delay. It goes on past a row it cannot
// reconcile, and returns every such error joined.
func (o *Outbox) Reconcile(ctx context.Context) error {
	rows, err := o.cfg.Store.ListUnposted(ctx)
	if err != nil {
		return fmt.Errorf("reconcile outbox: %w", err)
	}
	var errs []error
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
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// deliver posts the row id unless it already has a ref, which it returns. With lookup it first
// asks Slack for the message, and adopts it if it is there. The caller holds o.mu. The row is read
// again here, so a delivery that finished while the caller waited for the lock is seen.
func (o *Outbox) deliver(ctx context.Context, id string, lookup bool) (string, error) {
	row, err := o.cfg.Store.GetOutbox(ctx, id)
	if err != nil {
		return "", err
	}
	if row.MessageRef != "" {
		return row.MessageRef, nil
	}
	var content slack.Content
	if row.Payload == "" {
		return "", fmt.Errorf("deliver %s: the row has no payload: %w", id, store.ErrInvalid)
	}
	if err := json.Unmarshal([]byte(row.Payload), &content); err != nil {
		return "", fmt.Errorf("deliver %s: payload: %w", id, err)
	}
	if lookup {
		ts, found, err := o.find(ctx, row)
		if err != nil {
			return "", fmt.Errorf("deliver %s: looking for the message before posting it: %w", id, err)
		}
		if found {
			o.cfg.Logger.InfoContext(ctx, "outbox: adopted a message posted before a crash", "outbox_id", id, "channel", row.Channel, "ts", ts)
			return o.record(ctx, id, ts)
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, o.cfg.CallTimeout)
	defer cancel()
	ts, err := o.cfg.Poster.Post(callCtx, row.Channel, row.ThreadTS, content, id)
	if err != nil {
		return "", fmt.Errorf("deliver %s: post: %w", id, err)
	}
	return o.record(ctx, id, ts)
}

// find looks for the row's message, within the call timeout.
func (o *Outbox) find(ctx context.Context, row store.OutboxEntry) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, o.cfg.CallTimeout)
	defer cancel()
	return o.cfg.Poster.Find(ctx, row.Channel, row.ThreadTS, row.ID, row.CreatedAt)
}

// record stores the message's ts on the row and returns it. The message is already in Slack, so it
// is recorded even if ctx has ended (a shutdown right after the post), within the call timeout; a
// failure here leaves it for the next lookup to adopt.
func (o *Outbox) record(ctx context.Context, id, ts string) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.cfg.CallTimeout)
	defer cancel()
	if err := o.cfg.Store.RecordMessageRef(ctx, id, ts); err != nil {
		return "", fmt.Errorf("deliver %s: record the message ref: %w", id, err)
	}
	return ts, nil
}
