package interact

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// BackstopConfig configures a Backstop. Store, Sessions and Outbox are required.
type BackstopConfig struct {
	Store    store.Interactions
	Sessions SessionReader
	Outbox   Enqueuer
	Now      func() time.Time // time.Now if nil
	Logger   *slog.Logger     // slog.Default() if nil
	// Horizon is how far back it looks for answers (default RepairHorizon), Grace how long an answer is
	// given to arrive before it is taken for lost (default 2 minutes), and Interval how often Run looks
	// (default 1 minute).
	Horizon, Grace, Interval time.Duration
}

// Backstop tells a thread about an answer that never reached the agent. A person's answer is recorded
// before it is sent, and the session takes it only while its task is paused. If the session has not
// parked on the task yet when the answer is sent, and does not by the time it is sent again, the
// decider can do no more, and says nothing, since the first send may have got through before a crash.
// What decides is the mark the decider and the recovery set when the session takes an answer
// (store.Interaction.DeliveredAt): an answered interaction without it, whose task is no longer the
// session's, has an answer that was never taken and never will be, and the thread is told.
//
// It is the repair loop's twin, over the same listing of decided interactions in a horizon, and not part
// of it, since it needs the sessions and the outbox and the repair needs Slack's edits; both are run at
// start, so what a crash left is done then. The notice's ID is the one the decider uses for an answer
// that arrives too late, so that however many looked, and whichever came first, it is one post. It is safe
// for concurrent use.
type Backstop struct {
	cfg  BackstopConfig
	mu   sync.Mutex
	told map[string]struct{} // told by this process, to leave the outbox alone; forgotten past the horizon
}

// NewBackstop returns a Backstop over cfg.
func NewBackstop(cfg BackstopConfig) (*Backstop, error) {
	if cfg.Store == nil || cfg.Sessions == nil || cfg.Outbox == nil {
		return nil, errors.New("interact: Store, Sessions and Outbox are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Horizon <= 0 {
		cfg.Horizon = RepairHorizon
	}
	if cfg.Grace <= 0 {
		cfg.Grace = 2 * time.Minute
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	return &Backstop{cfg: cfg, told: map[string]struct{}{}}, nil
}

// Run checks at once, and then every Interval, until ctx ends, and returns its error.
func (b *Backstop) Run(ctx context.Context) error {
	tick := time.NewTicker(b.cfg.Interval)
	defer tick.Stop()
	for {
		if err := b.Check(ctx); err != nil && ctx.Err() == nil {
			b.cfg.Logger.WarnContext(ctx, "interact: answers that may not have been delivered could not all be checked; they will be tried again", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Check looks at the interactions answered within the horizon and tells the thread of each whose answer was
// never delivered, and whose task is over. It goes on past one it cannot read, and returns every such
// error joined.
func (b *Backstop) Check(ctx context.Context) error {
	now := b.cfg.Now()
	rows, err := b.cfg.Store.ListDecidedInteractions(ctx, now.Add(-b.cfg.Horizon))
	if err != nil {
		return fmt.Errorf("interact: list the interactions that were answered: %w", err)
	}
	listed := make(map[string]struct{}, len(rows))
	var errs []error
	for _, row := range rows {
		listed[row.ID] = struct{}{}
		if row.State != store.InteractionAnswered || !row.DeliveredAt.IsZero() || now.Sub(row.DecidedAt) < b.cfg.Grace || b.isTold(row.ID) {
			continue
		}
		if err := b.check(ctx, row, now); err != nil {
			errs = append(errs, err)
		}
	}
	b.mu.Lock()
	maps.DeleteFunc(b.told, func(id string, _ struct{}) bool { _, ok := listed[id]; return !ok }) // past the horizon
	b.mu.Unlock()
	return errors.Join(errs...)
}

func (b *Backstop) isTold(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.told[id]
	return ok
}

// check tells the thread of row if the task it was for is over.
func (b *Backstop) check(ctx context.Context, row store.Interaction, now time.Time) error {
	sess, err := b.cfg.Sessions.GetSession(ctx, row.Key, readAll, now)
	switch {
	case err == nil && sess.TaskID == row.TaskID:
		return nil // still the session's task: it may yet take the answer
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("interact: read the session of %s: %w", row.ID, err)
	}
	if _, err := b.cfg.Outbox.Enqueue(ctx, outbox.Post{Source: row.ID + "/answer", Role: store.OutboxNotice, Channel: row.Key.Channel,
		Thread: row.Key.Thread, Out: chat.Say(render.Notice(textClosedBefore, ""))}); err != nil {
		return fmt.Errorf("interact: tell the thread that the answer to %s never arrived: %w", row.ID, err)
	}
	b.mu.Lock()
	b.told[row.ID] = struct{}{}
	b.mu.Unlock()
	b.cfg.Logger.InfoContext(ctx, "interact: told a thread that an answer never reached its task", "interaction", row.ID, "task", row.TaskID)
	return nil
}
