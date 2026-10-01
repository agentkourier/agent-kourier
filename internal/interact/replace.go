package interact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// maxEditFailures is how many failed edits of a prompt the repair makes before it leaves the prompt, so
// that one whose message is gone is not tried for ever.
const maxEditFailures = 10

// ReplacerConfig configures a Replacer. Store and Updater are required.
type ReplacerConfig struct {
	Store   store.Interactions
	Updater Updater
	Now     func() time.Time // time.Now if nil
	Logger  *slog.Logger     // slog.Default() if nil
	// Horizon is how far back the repair looks for interactions that ended (default 24 hours): the
	// prompt of an older one that was never replaced stays as it is.
	Horizon time.Duration
	// Interval is how often Run repairs (default 10 seconds), Batch how many prompts a repair edits at
	// most (default 10), and ListEvery how often it reads the store for interactions that ended since
	// (default 5 minutes). Together they pace the edits to Slack, after a restart above all.
	Interval, ListEvery time.Duration
	Batch               int
}

// Replacer takes the controls off the prompt of an interaction that has ended, and says how it ended
// in their place (spec 8.4 step 5). Replace does it for one, and Repair and Run for every one that ended
// within the horizon and has not been done by this process: the ones whose edit failed, and the ones
// that ended while no process was there to edit them, or whose edit the sweep gave up on. There is no
// record of what was replaced, since an edit is the same however often it is made: a restarted
// process replaces each recent prompt once more. It is safe for concurrent use.
type Replacer struct {
	cfg ReplacerConfig
	run sync.Mutex // one repair at a time

	mu        sync.Mutex
	done      map[string]struct{} // replaced, or given up on, by this process
	failures  map[string]int
	todo      []string  // the interactions the last listing found
	listed    time.Time // when it did
	waitUntil time.Time // Slack asked for no edits before this
}

// NewReplacer returns a Replacer over cfg.
func NewReplacer(cfg ReplacerConfig) (*Replacer, error) {
	if cfg.Store == nil || cfg.Updater == nil {
		return nil, errors.New("interact: Store and Updater are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Horizon <= 0 {
		cfg.Horizon = 24 * time.Hour
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.ListEvery <= 0 {
		cfg.ListEvery = 5 * time.Minute
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 10
	}
	return &Replacer{cfg: cfg, done: map[string]struct{}{}, failures: map[string]int{}}, nil
}

// Replace edits the prompt of the interaction id into its replacement, built from the row: who
// decided and when, how it ended, and a summary of the answer, with no input or button. It does
// nothing, and returns nil, for an interaction that is pending, that has no prompt posted, or that is
// gone. The edit is the same however often it is made, so a failed one, or one repeated after a crash,
// is tried again. The error of a failed edit is returned.
func (r *Replacer) Replace(ctx context.Context, id string) error {
	row, err := r.cfg.Store.GetInteraction(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("interact: read %s: %w", id, err)
	case row.State == store.InteractionPending || row.MessageRef == "":
		return nil
	}
	content, err := ReplacementContent(row)
	if err != nil {
		return fmt.Errorf("interact: render the replacement of %s: %w", id, err)
	}
	if err := r.cfg.Updater.Update(ctx, row.Key.Channel, row.MessageRef, content); err != nil {
		return fmt.Errorf("interact: replace the prompt of %s: %w", id, err)
	}
	r.mu.Lock()
	r.done[id] = struct{}{}
	r.mu.Unlock()
	return nil
}

// Run repairs at once, and then every Interval, until ctx ends, and returns its error.
func (r *Replacer) Run(ctx context.Context) error {
	tick := time.NewTicker(r.cfg.Interval)
	defer tick.Stop()
	for {
		if err := r.Repair(ctx); err != nil && ctx.Err() == nil {
			r.cfg.Logger.WarnContext(ctx, "interact: some prompts could not be replaced; they will be tried again", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Repair replaces the prompts of up to Batch of the interactions that ended within the Horizon and that
// this process has not replaced. The store is read when none has been or ListEvery has passed since. A
// failed edit is counted and tried again by the next repair; after maxEditFailures it is given up on,
// and logged at Error. A rate limit is none of that: Slack's Retry-After is waited out, and the edit is
// not counted. It goes on past a prompt it cannot edit, and returns every such error joined.
func (r *Replacer) Repair(ctx context.Context) error {
	r.run.Lock()
	defer r.run.Unlock()
	now := r.cfg.Now()
	r.mu.Lock()
	waiting, relist := now.Before(r.waitUntil), r.listed.IsZero() || now.Sub(r.listed) >= r.cfg.ListEvery
	r.mu.Unlock()
	if waiting {
		return nil
	}
	if relist {
		rows, err := r.cfg.Store.ListDecidedInteractions(ctx, now.Add(-r.cfg.Horizon))
		if err != nil {
			return fmt.Errorf("interact: list the interactions that ended: %w", err)
		}
		r.mu.Lock()
		r.todo, r.listed = nil, now
		listed := make(map[string]struct{}, len(rows))
		for _, row := range rows {
			listed[row.ID] = struct{}{}
			if row.MessageRef != "" {
				r.todo = append(r.todo, row.ID)
			}
		}
		// What is no longer listed is past the horizon and will not be looked at again, so what is kept of
		// it is dropped, or both maps would grow by one entry per interaction for as long as the process runs.
		maps.DeleteFunc(r.done, func(id string, _ struct{}) bool { _, ok := listed[id]; return !ok })
		maps.DeleteFunc(r.failures, func(id string, _ int) bool { _, ok := listed[id]; return !ok })
		r.mu.Unlock()
	}
	r.mu.Lock()
	todo := slices.Clone(r.todo)
	r.mu.Unlock()
	var errs []error
	tried := 0
	for _, id := range todo {
		if tried == r.cfg.Batch {
			break
		}
		if r.isDone(id) {
			continue
		}
		tried++
		err := r.Replace(ctx, id)
		var limited *slackgo.RateLimitedError
		switch {
		case err == nil:
		case errors.As(err, &limited):
			r.mu.Lock()
			r.waitUntil = now.Add(max(limited.RetryAfter, time.Second))
			r.mu.Unlock()
			return errors.Join(errs...)
		default:
			errs = append(errs, err)
			r.fail(ctx, id, err)
		}
	}
	return errors.Join(errs...)
}

func (r *Replacer) isDone(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.done[id]
	return ok
}

// fail counts a failed edit of the prompt of id, and leaves the prompt once it has failed too often.
func (r *Replacer) fail(ctx context.Context, id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures[id]++; r.failures[id] >= maxEditFailures {
		r.done[id] = struct{}{}
		r.cfg.Logger.ErrorContext(ctx, "interact: giving up on replacing a prompt: it keeps its controls", "interaction", id, "failures", r.failures[id], "err", err)
	}
}

// ReplacementContent is the message that takes the place of the prompt of a decided interaction. It is
// built from the row alone: the outcome from its state, who from its decider and when from its decision
// time, and the questions, hint and sub-agent from its stored request. An answer is shown only as its
// chosen choices, and never as free text (the owner's decision); that is render's rule, and the answer
// is not otherwise used here.
func ReplacementContent(row store.Interaction) (slack.Content, error) {
	e := render.Ending{By: row.Decider, At: row.DecidedAt}
	switch row.State {
	case store.InteractionAnswered:
		e.Outcome = render.Answered
	case store.InteractionTimedOut:
		e.Outcome = render.TimedOut
	case store.InteractionCancelled:
		e.Outcome = render.Cancelled
	default:
		return slack.Content{}, fmt.Errorf("an interaction that is %q has no replacement", row.State)
	}
	var in dialect.Interaction
	if err := json.Unmarshal(row.Request, &in); err != nil {
		return slack.Content{}, fmt.Errorf("the stored request: %w", err)
	}
	e.Hint, e.Questions = in.Hint, in.Questions
	if in.Nested != nil {
		e.Subagent = in.Nested.Subagent
	}
	if e.Outcome == render.Answered {
		var a dialect.Answer
		if err := json.Unmarshal(row.Answer, &a); err != nil {
			return slack.Content{}, fmt.Errorf("the stored answer: %w", err)
		}
		e.Answers = a.Answers
	}
	text, blocks := render.Replacement(e)
	raw, err := json.Marshal(blocks)
	if err != nil {
		return slack.Content{}, err
	}
	return slack.Content{Text: text, Blocks: raw}, nil
}
