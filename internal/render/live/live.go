// Package live renders one turn's output into a single Slack message, which it keeps up to date
// with chat.update while the agent works (spec 8.3). Each turn has one goroutine that holds the
// latest state; the handler it gives out only stores and signals, so it never blocks the stream
// reader. Writes are throttled per message and coalesced: whatever the state is when the window
// opens is what is sent. The first post goes through the outbox, so a crash cannot post it twice
// (spec 8.6); later changes are updates, which are idempotent.
//
// This is the chat.update path. It never updates a message that is still streaming: it never
// starts a stream. Text is rendered at the last step, from the raw events (package render), and
// every send carries the Slack client's inert options.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// NoOutput is what a message says when its turn ended with nothing to show, so that a tool status
// line is not left standing as if the agent were still at work.
const NoOutput = "(no output)"

// fallbackLimit is the length of a message's plain text, which Slack shows where it cannot show
// the blocks, such as in a notification.
const fallbackLimit = 1000

// Sender posts through the outbox: *outbox.Outbox.
type Sender interface {
	Send(ctx context.Context, p outbox.Post) (string, error)
}

// Updater edits a message: *slack.Client.
type Updater interface {
	Update(ctx context.Context, channel, ts string, c slack.Content) error
}

// Config configures a Renderer. Sender and Updater are required.
type Config struct {
	Sender  Sender
	Updater Updater
	// Throttle is the least time between two writes to one message (default 2 seconds).
	Throttle time.Duration
	// Attempts is how many times in a row a write may fail before it is given up (default 5). A
	// rate limit is not an attempt: Slack says when to come back, and the write waits for it.
	Attempts int
	// FlushTimeout bounds the one write that settles a turn whose context ended after its final
	// event, which runs on a context that is no longer cancelled (default 10 seconds).
	FlushTimeout time.Duration
	// Now and After are the clock, which a test injects: time.Now and time.After by default.
	Now    func() time.Time
	After  func(time.Duration) <-chan time.Time
	Logger *slog.Logger // slog.Default() if nil
}

// Renderer starts the rendering of turns. It is safe for concurrent use.
type Renderer struct{ cfg Config }

// New returns a Renderer over cfg.
func New(cfg Config) *Renderer {
	if cfg.Throttle <= 0 {
		cfg.Throttle = 2 * time.Second
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = 5
	}
	if cfg.FlushTimeout <= 0 {
		cfg.FlushTimeout = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Renderer{cfg}
}

// Turn is where one turn's output goes.
type Turn struct {
	Source            string // the turn's ID, from which the outbox row of its reply is named
	Channel, ThreadTS string
	// Mode is live, which updates the message as the turn runs, or final, which posts it once
	// when the turn ends.
	Mode v1alpha1.OutputMode
	// Recovered says the turn was found again after a restart: its message may exist already, and
	// is updated to the whole text.
	Recovered bool
}

// Run is the rendering of one turn.
type Run struct {
	cfg  Config
	turn Turn
	wake chan struct{} // a slot: the state changed
	done chan struct{}

	mu    sync.Mutex // guards the state below
	texts map[a2a.ArtifactID]string
	order []a2a.ArtifactID
	tool  *agent.ToolCallEvent // the call being made, if any
	final bool

	// Below: the goroutine's alone.
	sends int  // Sends tried
	stale bool // the message may hold older content than the last Send was given
}

// Start begins rendering a turn and returns it. Its goroutine ends once the final text is
// written, or given up on, or when ctx ends.
func (r *Renderer) Start(ctx context.Context, t Turn) *Run {
	u := &Run{cfg: r.cfg, turn: t, wake: make(chan struct{}, 1), done: make(chan struct{}), texts: map[a2a.ArtifactID]string{}}
	go u.run(ctx)
	return u
}

// Done is closed when the goroutine has ended.
func (u *Run) Done() <-chan struct{} { return u.done }

// Handle takes an event of the turn. It is the agent.Handler: it stores the event and signals,
// and never blocks, so it may be called from the stream's reader. After the end it does nothing.
func (u *Run) Handle(ev agent.Event) {
	select {
	case <-u.done:
		return
	default:
	}
	u.mu.Lock()
	switch e := ev.(type) {
	case agent.TextEvent:
		if _, seen := u.texts[e.ArtifactID]; !seen {
			u.order = append(u.order, e.ArtifactID)
		}
		u.texts[e.ArtifactID] = e.Text
	case agent.ToolCallEvent:
		u.tool = &e
	case agent.ToolResultEvent:
		if u.tool != nil && u.tool.CallID == e.CallID {
			u.tool = nil
		}
	case agent.FinalEvent:
		u.final = true
	}
	u.mu.Unlock()
	select {
	case u.wake <- struct{}{}:
	default: // a signal is waiting already
	}
}

// run writes the state each time its window opens, until the final text is out.
func (u *Run) run(ctx context.Context) {
	defer close(u.done)
	var ts, sent string
	var next time.Time
	fails := 0
	for {
		select {
		case <-ctx.Done():
			u.flush(ctx, &ts, sent)
			return
		case <-u.wake:
		}
		for {
			if d := next.Sub(u.cfg.Now()); d > 0 {
				select {
				case <-ctx.Done():
					u.flush(ctx, &ts, sent)
					return
				case <-u.cfg.After(d):
				}
			}
			c, final, ok := u.snapshot() // after the wait, so that what is sent is the latest
			if !ok && final {            // the turn ended with nothing to show
				if ts == "" {
					return
				}
				c, ok = placeholder(), true // but a message stands: do not leave its tool line up
			}
			if !ok || (u.turn.Mode == v1alpha1.OutputFinal && !final) {
				break
			}
			if key := c.Text + string(c.Blocks); key == sent {
				if final {
					return
				}
				break
			}
			err := u.write(ctx, &ts, c)
			next = u.cfg.Now().Add(u.cfg.Throttle)
			if err != nil {
				var limited *slackgo.RateLimitedError
				switch {
				case errors.Is(err, store.ErrInvalid), errors.Is(err, outbox.ErrAbandoned): // retrying cannot mend them
					u.cfg.Logger.ErrorContext(ctx, "live: the message cannot be sent; giving up", "channel", u.turn.Channel, "err", err)
					return
				case errors.As(err, &limited):
					u.cfg.Logger.WarnContext(ctx, "live: Slack rate-limited the message; waiting as it asked", "channel", u.turn.Channel, "retry_after", limited.RetryAfter)
					next = u.cfg.Now().Add(max(u.cfg.Throttle, limited.RetryAfter))
					continue
				}
				u.cfg.Logger.WarnContext(ctx, "live: writing the message failed; it will be tried again", "channel", u.turn.Channel, "err", err)
				if fails++; fails >= u.cfg.Attempts {
					u.cfg.Logger.ErrorContext(ctx, "live: giving up on a message", "channel", u.turn.Channel)
					if final {
						return
					}
					fails = 0
					break
				}
				continue
			}
			fails = 0
			sent = c.Text + string(c.Blocks)
			if u.stale {
				u.stale, sent = false, "" // the post may be the old message: write once more
				continue
			}
			if final {
				return
			}
			break
		}
	}
}

// placeholder is the message of a turn that ended with nothing to show.
func placeholder() slack.Content {
	blocks, _ := json.Marshal([]map[string]any{{"type": "markdown", "text": NoOutput}})
	return slack.Content{Text: NoOutput, Blocks: blocks}
}

// flush settles a turn whose context ended after its final event: the task is over, nothing will
// send the final text again, so it is written once, now, on a context that is not cancelled and is
// bounded by FlushTimeout. A turn that is not final owes nothing, and is left.
func (u *Run) flush(ctx context.Context, ts *string, sent string) {
	c, final, ok := u.snapshot()
	if !final {
		return
	}
	if !ok {
		if *ts == "" {
			return
		}
		c = placeholder()
	}
	if c.Text+string(c.Blocks) == sent {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.cfg.FlushTimeout)
	defer cancel()
	err := u.write(ctx, ts, c)
	if err == nil && u.stale { // the post may be the old message, not this text
		u.stale = false
		err = u.write(ctx, ts, c)
	}
	if err != nil {
		u.cfg.Logger.ErrorContext(ctx, "live: the final text was not written at shutdown", "channel", u.turn.Channel, "err", err)
	}
}

// write posts the message the first time, through the outbox, and updates it after.
func (u *Run) write(ctx context.Context, ts *string, c slack.Content) error {
	if *ts != "" {
		return u.cfg.Updater.Update(ctx, u.turn.Channel, *ts, c)
	}
	u.sends++
	got, err := u.cfg.Sender.Send(ctx, outbox.Post{Source: u.turn.Source, Role: store.OutboxReply, Channel: u.turn.Channel, ThreadTS: u.turn.ThreadTS, Content: c})
	if err == nil {
		*ts = got
		// A Send that follows a failed one, or a recovered turn's, returns the message that is
		// there already, or posts the payload stored first, not necessarily this content.
		u.stale = u.sends > 1 || u.turn.Recovered
	}
	return err
}

// snapshot renders the state as a message: a markdown block of the text so far and, while a tool
// call is under way, a context block with its status line. Once the turn is final the line is
// gone. ok is false if there is nothing to show yet.
func (u *Run) snapshot() (c slack.Content, final, ok bool) {
	u.mu.Lock()
	events := make([]agent.TextEvent, 0, len(u.order))
	for _, id := range u.order {
		events = append(events, agent.TextEvent{ArtifactID: id, Text: u.texts[id]})
	}
	tool, final := u.tool, u.final
	u.mu.Unlock()
	c.Text = render.Body(events, fallbackLimit, render.FlavourText)
	var blocks []map[string]any
	if body := render.Body(events, render.MaxBlock, render.FlavourMarkdown); body != "" {
		blocks = append(blocks, map[string]any{"type": "markdown", "text": body})
	}
	if tool != nil && !final {
		// The line sits in a mrkdwn context block, where "[text](url)" is no link and entities are
		// what render's text flavour escapes; it must not go inside the markdown block.
		line := render.ToolLine(tool.Name, tool.Args, render.FlavourText)
		blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": line}}})
		if c.Text == "" {
			c.Text = line
		}
	}
	if len(blocks) == 0 {
		return c, final, false
	}
	c.Blocks, _ = json.Marshal(blocks)
	return c, final, true
}
