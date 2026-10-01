// Package interact turns an agent's pause into a question in the thread (spec 8.4). The intake
// records a pause as an interaction and posts its prompt through the outbox, and when the same pause
// comes again after a crash it finishes what the crash left undone.
package interact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// ErrDisallowed is returned for a tool approval on a Binding that allows them, which Phase 5 renders.
var ErrDisallowed = errors.New("interact: the Binding does not allow that kind of pause")

// Binding is what the intake needs of a thread's Binding: which pauses it allows, and how long one waits.
type Binding struct {
	Dialect                dialect.Dialect
	Identity               string // the Binding's service identity, for the audit rows
	AskUser, ToolApprovals bool
	Timeout                time.Duration
}

// Lookup returns the Binding of a name; the broker implements it (WP-35h).
type Lookup interface {
	Binding(name string) (Binding, error)
}

// Prompt is what a renderer turns into the message that asks. ID is what a press carries back.
type Prompt struct {
	ID          string
	Interaction dialect.Interaction
	Deadline    time.Time
}

// PromptRenderer renders a prompt (WP-33b1); what the agent wrote is untrusted, and the renderer's to escape.
type PromptRenderer interface {
	Prompt(p Prompt) (slack.Content, error)
}

// Sender is *outbox.Outbox: Send posts at once, Enqueue only writes the row for the drain to post.
type Sender interface {
	Send(ctx context.Context, p outbox.Post) (string, error)
	Enqueue(ctx context.Context, p outbox.Post) (string, error)
}

// Config configures an Intake. Store, Lookup, Prompts, Outbox, Session and Audit are required.
type Config struct {
	Store   store.Interactions
	Lookup  Lookup
	Prompts PromptRenderer
	Outbox  Sender
	Session Session
	Audit   Auditor
	// OnApprovalOutcome is called once for each pause a Binding's policy turned away, for the
	// approval-outcome metric (WP-35f2). The outcomes are a closed set. Optional.
	OnApprovalOutcome func(binding string, outcome Outcome)
	Now               func() time.Time // time.Now if nil
	Logger            *slog.Logger     // slog.Default() if nil
}

// Intake records pauses as interactions and posts their prompts. It is safe for concurrent use.
type Intake struct{ cfg Config }

// New returns an Intake over cfg.
func New(cfg Config) (*Intake, error) {
	if cfg.Store == nil || cfg.Lookup == nil || cfg.Prompts == nil || cfg.Outbox == nil || cfg.Session == nil || cfg.Audit == nil {
		return nil, errors.New("interact: Store, Lookup, Prompts, Outbox, Session and Audit are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Intake{cfg}, nil
}

// Pause is a task paused on input, as the turn's FinalEvent carried it; reconciliation re-emits it after a restart.
type Pause struct {
	Key     store.ConversationKey
	Binding string
	Task    a2a.Task
}

// InteractionID is the ID a pause becomes: a hash of its kind, task, request ID and tool IDs, so the
// same pause always has the same ID and agent text never reaches one.
func InteractionID(in dialect.Interaction) string {
	tools := make([]string, len(in.Tools))
	for i, t := range in.Tools {
		tools[i] = t.ID
	}
	slices.Sort(tools)
	var b strings.Builder
	for _, f := range append([]string{string(in.Kind), string(in.TaskID), in.RequestID}, tools...) {
		fmt.Fprintf(&b, "%d:%s;", len(f), f) // length-prefixed, so no two field lists hash alike
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "ix-" + hex.EncodeToString(sum[:])[:32]
}

// Paused takes up a pause. Every step is idempotent, so a crash between two leaves the next call to
// finish the work and never repeat it: it creates the interaction, posts the prompt if that is not
// done (the outbox adopts a post made and not recorded), and records the message. It does I/O, so a
// caller on the agent's stream goroutine runs it elsewhere.
func (i *Intake) Paused(ctx context.Context, p Pause) error {
	b, err := i.cfg.Lookup.Binding(p.Binding)
	if err != nil {
		return fmt.Errorf("interact: Binding %q: %w", p.Binding, err)
	}
	in, err := b.Dialect.ParseInteraction(p.Task)
	switch {
	case errors.Is(err, dialect.ErrUnusablePause):
		return i.refuseUnusable(ctx, p, b, err)
	case err != nil:
		return fmt.Errorf("interact: parse the pause of task %s: %w", p.Task.ID, err)
	case in == nil:
		return nil // nothing structured to answer: the thread's own replies carry on
	}
	switch {
	case in.Kind == dialect.ToolApproval && !b.ToolApprovals:
		return i.autoDeny(ctx, p, b, *in)
	case in.Kind == dialect.ToolApproval: // allowed, but Phase 5 renders it
		return ErrDisallowed
	case !b.AskUser:
		return i.refuseAskUser(ctx, p, b, *in)
	}
	row, err := i.record(ctx, p, b, *in)
	if errors.Is(err, dialect.ErrUnusablePause) { // a second question under a seen ID
		return i.refuseUnusable(ctx, p, b, err)
	} else if err != nil {
		return err
	}
	switch row.State {
	case store.InteractionPending:
		if row.MessageRef == "" {
			return i.post(ctx, p, row, *in)
		}
	case store.InteractionAnswered: // the pause is back, so the answer did not arrive
		return i.resend(ctx, p, b, row)
	case store.InteractionTimedOut, store.InteractionCancelled: // the cancel may not have been made
		err := i.cfg.Session.Cancel(ctx, p.Key, a2a.TaskID(row.TaskID), "the interaction timed out")
		if err != nil && !errors.Is(err, session.ErrNotPaused) {
			return fmt.Errorf("interact: cancel the task of %s again: %w", row.ID, err)
		}
	}
	return nil
}

// record creates the interaction for in, or finds the one that exists. The deadline is the task's own
// timestamp, or courier's clock if that is earlier, plus the Binding's timeout, so a pause seen again
// after a restart keeps its deadline and kagent's clock cannot push it out (the clock is UNCONFIRMED(WP-02)).
//
// The ID names a pause by its task and request ID, which is UNCONFIRMED(WP-02) to be unique: kagent's
// ID is the model's tool call ID, which a model may repeat. So the stored request must be the one just
// parsed; if it is not, this is another question under a seen ID, and it is refused as unusable.
func (i *Intake) record(ctx context.Context, p Pause, b Binding, in dialect.Interaction) (store.Interaction, error) {
	request, err := json.Marshal(in) // the dialect reads it back unchanged to build the answer
	if err != nil {
		return store.Interaction{}, fmt.Errorf("interact: encode the pause: %w", err)
	}
	at := i.cfg.Now()
	if ts := p.Task.Status.Timestamp; ts != nil && ts.Before(at) {
		at = *ts
	}
	kind := store.InteractionAskUser
	switch {
	case in.Nested != nil:
		kind = store.InteractionNested
	case in.Kind == dialect.ToolApproval:
		kind = store.InteractionToolApproval
	}
	id := InteractionID(in)
	row := store.Interaction{ID: id, Key: p.Key, TaskID: string(in.TaskID), Kind: kind, Request: request, CreatedAt: at, Deadline: at.Add(b.Timeout)}
	if _, err := i.cfg.Store.CreateInteraction(ctx, row); err != nil {
		return store.Interaction{}, fmt.Errorf("interact: record %s: %w", id, err)
	}
	if row, err = i.cfg.Store.GetInteraction(ctx, id); err != nil {
		return store.Interaction{}, fmt.Errorf("interact: read %s: %w", id, err)
	}
	if !bytes.Equal(row.Request, request) {
		return store.Interaction{}, fmt.Errorf("%w: a second, different pause under the ID of %s", dialect.ErrUnusablePause, id)
	}
	return row, nil
}

// post sends the prompt through the outbox, role reply, source the interaction ID (spec 8.6 step 5).
func (i *Intake) post(ctx context.Context, p Pause, row store.Interaction, in dialect.Interaction) error {
	content, err := i.cfg.Prompts.Prompt(Prompt{ID: row.ID, Interaction: in, Deadline: row.Deadline})
	if err != nil {
		return fmt.Errorf("interact: render the prompt of %s: %w", row.ID, err)
	}
	ref, err := i.cfg.Outbox.Send(ctx, outbox.Post{Source: row.ID, Role: store.OutboxReply, Channel: p.Key.Channel, ThreadTS: p.Key.ThreadTS, Content: content})
	if err != nil {
		return fmt.Errorf("interact: post the prompt of %s: %w", row.ID, err)
	}
	if err := i.cfg.Store.SetInteractionMessageRef(ctx, row.ID, ref); err != nil {
		return fmt.Errorf("interact: record the prompt of %s: %w", row.ID, err)
	}
	return nil
}
