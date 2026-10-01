// Package outbox posts to Slack once, even across a crash (spec 8.6). A post is written to the
// store before it is sent, tagged so it can be found in Slack's history, and its message ref is
// recorded after. A row with no ref may have been posted, so it is looked up before it is posted
// again.
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
	mu sync.Mutex
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
	return &Outbox{cfg: cfg}
}

// Send writes the row, posts the message and records its ts, and returns the ts. A message that
// is already recorded is returned without a call to Slack. A row that exists without a ref, left
// by a failed post or a crash, is looked up first and posted only if Slack has no such message.
// A new row is posted at once. If the lookup fails nothing is posted, and the error is returned.
func (o *Outbox) Send(ctx context.Context, p Post) (string, error) {
	if p.Content.Text == "" && len(p.Content.Blocks) == 0 {
		return "", fmt.Errorf("send %s: no text or blocks: %w", ID(p.Source, p.Role), store.ErrInvalid)
	}
	payload, err := json.Marshal(p.Content)
	if err != nil {
		return "", fmt.Errorf("send %s: %w", ID(p.Source, p.Role), err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	row := store.OutboxEntry{ID: ID(p.Source, p.Role), EventID: p.Source, Role: p.Role, Channel: p.Channel, ThreadTS: p.ThreadTS,
		CreatedAt: o.cfg.Now(), Payload: string(payload)}
	created, err := o.cfg.Store.CreateOutbox(ctx, row)
	if err != nil {
		return "", err
	}
	return o.deliver(ctx, row.ID, !created)
}

// Reconcile delivers every row that has a payload and no ref, oldest first, looking each up
// before it posts: it is for startup, after a crash may have left posts unrecorded. A row with no
// payload is a stream, whose renderer records the ref itself, and is left alone. It goes on past
// a row it cannot reconcile, and returns every such error joined.
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
		_, err := o.deliver(ctx, r.ID, true)
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

// record stores the message's ts on the row and returns it. The message is already in Slack, so a
// failure here leaves it for the next lookup to adopt.
func (o *Outbox) record(ctx context.Context, id, ts string) (string, error) {
	if err := o.cfg.Store.RecordMessageRef(ctx, id, ts); err != nil {
		return "", fmt.Errorf("deliver %s: record the message ref: %w", id, err)
	}
	return ts, nil
}
