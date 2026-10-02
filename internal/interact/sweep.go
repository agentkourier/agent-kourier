package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Updater edits a message: chat.Platform.
type Updater interface {
	Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error
	// RefAt is the Ref of the message recorded by its ID alone, as an interaction records its prompt.
	RefAt(channel, thread, id string) chat.Ref
}

// Enqueuer writes a post for the outbox to deliver: *outbox.Outbox.
type Enqueuer interface {
	Enqueue(ctx context.Context, p outbox.Post) (string, error)
}

// SessionReader reads a conversation's session, to name its Binding: the session store.
type SessionReader interface {
	GetSession(ctx context.Context, key store.ConversationKey, ttl time.Duration, now time.Time) (store.Session, error)
}

// PressQueue knows the presses that are recorded and not yet decided (WP-33d1 implements it).
type PressQueue interface {
	// PendingPresses reports whether the interaction has a press, recorded at or before the time
	// given, that has not been decided.
	PendingPresses(ctx context.Context, interactionID string, before time.Time) (bool, error)
}

// timedOutNotice is the fixed text of the notice in the thread. It names no agent text, and does not
// say that the task was cancelled, which it may not have been: it had moved on, or the cancel failed.
const timedOutNotice = "No one answered this question in time, so it was closed."

// maxAttempts is how many sweeps may fail at the edit or the notice of a timeout before the step is
// given up on, so that one prompt that cannot be edited does not hold its thread. The cancel is
// never given up on: a task that is still paused holds its thread and the replies queued behind it,
// and only this sweep would end it.
const maxAttempts = 5

// readAll is a thread TTL no session outlives, to read a session whatever its age.
const readAll = 100 * 365 * 24 * time.Hour

// SweepConfig configures a Sweeper. Everything but Now, Logger, Interval, Batch, Grace and
// OnTimeout is required.
type SweepConfig struct {
	Store    store.Interactions
	Sessions SessionReader
	Session  Session
	Updater  Updater
	Outbox   Enqueuer
	Auditor  Auditor
	// Presses is asked before an interaction is timed out: one with a press recorded by its
	// deadline and not yet decided is left, because deciding a press can take longer than the
	// grace, and cancelling the task under it would drop an answer that came in time.
	Presses PressQueue
	Now     func() time.Time // time.Now if nil
	Logger  *slog.Logger     // slog.Default() if nil
	// Interval is how often Run sweeps (default 10 seconds).
	Interval time.Duration
	// Batch is how many interactions one sweep times out at most (default 10), so that a backlog
	// after an outage reaches Slack a few edits at a time.
	Batch int
	// Grace is how long past its deadline an interaction waits before it is swept (default 5
	// seconds), so that a press that was being decided at the deadline has been recorded first.
	Grace time.Duration
	// OnTimeout, if set, is called once for each interaction that timed out, with its ID, for a metric.
	OnTimeout func(id string)
}

// Sweeper times out the interactions nobody answered (spec 8.4 step 6).
//
// UNCONFIRMED: a task that resumes outside courier (from kagent's own UI, say) and pauses again is a
// new pause, and becomes a new interaction while the old one is still pending. The old one is swept
// when it is due, and its cancel names the old pause's task, which is the task that is paused now. Every step before the
// timeout is recorded can be repeated, so a sweep that dies halfway is finished by the next, and
// only the sweep that records the timeout audits and counts it.
type Sweeper struct {
	cfg SweepConfig
	mu  sync.Mutex     // one sweep at a time
	try map[string]int // sweeps that have failed at a step, by interaction; guarded by mu
}

// NewSweeper returns a Sweeper over cfg.
func NewSweeper(cfg SweepConfig) (*Sweeper, error) {
	if cfg.Store == nil || cfg.Sessions == nil || cfg.Session == nil || cfg.Updater == nil || cfg.Outbox == nil || cfg.Auditor == nil || cfg.Presses == nil {
		return nil, errors.New("interact: Store, Sessions, Session, Updater, Outbox, Auditor and Presses are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 10
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 5 * time.Second
	}
	return &Sweeper{cfg: cfg, try: map[string]int{}}, nil
}

// Run sweeps at once and then every Interval until ctx ends, and returns its error.
func (s *Sweeper) Run(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()
	for {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.WarnContext(ctx, "interact: some interactions were not timed out; they will be tried again", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Sweep times out up to Batch of the interactions that are past their deadline and grace, oldest
// first, except those with a press waiting to be decided (see Presses). For each it cancels the paused task (a task that has moved on is fine), replaces the
// prompt, enqueues the notice, records the timeout, and then audits and counts it. A step that
// fails leaves the interaction pending for the next sweep. It goes on past one it cannot finish,
// and returns every such error joined.
func (s *Sweeper) Sweep(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	due, err := s.cfg.Store.ListDueInteractions(ctx, now.Add(-s.cfg.Grace))
	if err != nil {
		return fmt.Errorf("interact: list the interactions that are due: %w", err)
	}
	var errs []error
	done := 0
	for _, row := range due {
		if done == s.cfg.Batch {
			break
		}
		switch waiting, err := s.cfg.Presses.PendingPresses(ctx, row.ID, row.Deadline); { // if it cannot be known, wait
		case err != nil:
			errs = append(errs, fmt.Errorf("interact: presses of %s: %w", row.ID, err))
			continue
		case waiting:
			continue // not counted against the batch: it is not work
		}
		done++
		if err := s.timeOut(ctx, row, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// timeOut does the steps for one interaction, then records the timeout.
func (s *Sweeper) timeOut(ctx context.Context, row store.Interaction, now time.Time) error {
	s.try[row.ID]++
	final := s.try[row.ID] >= maxAttempts // the sweep on which the edit and the notice are given up on
	// The order matters: the cancel first, so that nothing is said or edited for a task that is still
	// paused, and the timeout recorded last, so that a sweep that dies on the way is finished by the next.
	for _, step := range []struct {
		name      string
		essential bool // never given up on
		edit      bool // giving it up leaves the prompt with its controls
		do        func() error
	}{
		{"cancel the task", true, false, func() error { return s.cancel(ctx, row) }},
		{"replace the prompt", false, true, func() error { return s.replace(ctx, row) }},
		{"enqueue the notice", false, false, func() error { return s.notify(ctx, row) }},
	} {
		err := step.do()
		switch {
		case err == nil:
		case step.essential || !final:
			return fmt.Errorf("interact: time out %s: %s: %w", row.ID, step.name, err)
		case step.edit:
			// The prompt keeps its buttons although the question timed out: a press on one is refused,
			// but nothing yet edits it again. WP-33d3 repairs it, and finds it by this line.
			s.cfg.Logger.WarnContext(ctx, "interact: the prompt of a timed-out interaction keeps its controls: the edit was given up on", "interaction", row.ID, "err", err)
		default:
			s.cfg.Logger.ErrorContext(ctx, "interact: giving up on a step of a timeout", "interaction", row.ID, "step", step.name, "err", err)
		}
	}
	err := s.cfg.Store.ResolveInteraction(ctx, row.ID, store.InteractionTimedOut, now)
	switch {
	case errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound): // decided in the meantime
		delete(s.try, row.ID)
		return nil
	case err != nil:
		return fmt.Errorf("interact: record the timeout of %s: %w", row.ID, err)
	}
	delete(s.try, row.ID)
	s.report(ctx, row, now)
	return nil
}

// cancel cancels the task of the question. It names that task, so a newer one the thread has moved
// on to is not touched: the session answers ErrNotPaused for one that is not the paused task.
func (s *Sweeper) cancel(ctx context.Context, row store.Interaction) error {
	err := s.cfg.Session.Cancel(ctx, row.Key, a2a.TaskID(row.TaskID), "the interaction timed out")
	if err != nil && !errors.Is(err, session.ErrNotPaused) {
		return err
	}
	return nil
}

// replace edits the prompt, if it was posted, into the timed-out message.
func (s *Sweeper) replace(ctx context.Context, row store.Interaction) error {
	if row.MessageRef == "" {
		return nil
	}
	var in dialect.Interaction // zero if the stored request cannot be read: the ending still says that it timed out
	if err := json.Unmarshal(row.Request, &in); err != nil {
		s.cfg.Logger.WarnContext(ctx, "interact: the stored request cannot be read", "interaction", row.ID)
	}
	return s.cfg.Updater.Update(ctx, s.cfg.Updater.RefAt(row.Key.Channel, row.Key.Thread, row.MessageRef), chat.End(endingOf(chat.TimedOut, "", row.Deadline, in, nil)))
}

// notify enqueues the notice. Its ID comes from the interaction, so the same one is never posted twice.
func (s *Sweeper) notify(ctx context.Context, row store.Interaction) error {
	_, err := s.cfg.Outbox.Enqueue(ctx, outbox.Post{Source: row.ID, Role: store.OutboxNotice, Channel: row.Key.Channel,
		Thread: row.Key.Thread, Out: chat.Say(render.Notice(timedOutNotice, ""))})
	return err
}

// report audits a timeout this sweep recorded and counts it. The timeout is done, so a failure to
// audit is logged and never undoes it (spec 8.6 step 7); the writer meters the failure itself.
func (s *Sweeper) report(ctx context.Context, row store.Interaction, now time.Time) {
	var actor audit.Actor // no person decided it
	if sess, err := s.cfg.Sessions.GetSession(ctx, row.Key, readAll, now); err == nil {
		actor.Binding = sess.Binding
	}
	d := audit.Decision{InteractionID: row.ID, TaskID: a2a.TaskID(row.TaskID), Result: audit.DecisionTimedOut, At: now}
	if err := s.cfg.Auditor.Decision(ctx, actor, d); err != nil {
		s.cfg.Logger.ErrorContext(ctx, "interact: a timeout was not audited", "interaction", row.ID, "err", err)
	}
	if s.cfg.OnTimeout != nil {
		s.cfg.OnTimeout(row.ID)
	}
}
