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

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// RepairHorizon is how far back the repair of ended prompts, and the backstop of undelivered answers, look for
// interactions that ended (their default Horizon). Retention keeps the request and answer of a settled interaction
// for longer than this (RetentionConfig.ClearGrace), since the repair builds its edit from them.
const RepairHorizon = 24 * time.Hour

// maxEditFailures is how many failed edits of a prompt the repair makes before it leaves the prompt, so
// that one whose message is gone is not tried for ever.
const maxEditFailures = 10

// Editor edits a message and says what an error of an edit means: chat.Platform.
type Editor interface {
	Updater
	Classify(op chat.Op, err error) chat.Verdict
}

// ReplacerConfig configures a Replacer. Store and Updater are required.
type ReplacerConfig struct {
	Store   store.Interactions
	Updater Editor
	Now     func() time.Time // time.Now if nil
	Logger  *slog.Logger     // slog.Default() if nil
	// Horizon is how far back the repair looks for interactions that ended (default RepairHorizon): the
	// prompt of an older one that was never replaced stays as it is.
	Horizon time.Duration
	// EditTimeout bounds each edit (default 30 seconds), so that one that never answers does not hold up
	// the press that is being decided, or the repair, which edits one prompt at a time.
	EditTimeout time.Duration
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
		cfg.Horizon = RepairHorizon
	}
	if cfg.EditTimeout <= 0 {
		cfg.EditTimeout = 30 * time.Second
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
	case len(row.Request) == 0: // cleared by retention, which waits out RepairHorizon: the ending cannot be built, and is no longer wanted
		return nil
	}
	out, err := ReplacementOutgoing(row)
	if err != nil {
		return fmt.Errorf("interact: render the replacement of %s: %w", id, err)
	}
	editCtx, cancel := context.WithTimeout(ctx, r.cfg.EditTimeout)
	defer cancel()
	if err := r.cfg.Updater.Update(editCtx, r.cfg.Updater.RefAt(row.Key.Channel, row.Key.Thread, row.MessageRef), out); err != nil {
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
		if err == nil {
			continue
		}
		if retry := r.cfg.Updater.Classify(chat.OpPost, err).RetryAfter; retry > 0 { // the platform said when to come back
			r.mu.Lock()
			r.waitUntil = r.cfg.Now().Add(max(retry, time.Second)) // from when it arrived: the pass may have been long
			r.mu.Unlock()
			return errors.Join(errs...)
		}
		errs = append(errs, err)
		r.fail(ctx, id, err)
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

// ReplacementOutgoing is the ending that takes the place of the prompt of a decided interaction. It is
// built from the row alone: the outcome from its state, who from its decider and when from its decision
// time, and the questions, hint and sub-agent from its stored request. An answer is shown only as its
// chosen choices, and never as free text (the owner's decision): endingOf says only that a free-text
// question was answered. A tool approval that timed out or was cancelled has an ending of its tools as stored; an
// answered one adds the verdict of each tool from the stored answer (approvalEnding), and is an error if that answer does not
// decide exactly the tools of the stored request.
func ReplacementOutgoing(row store.Interaction) (chat.Outgoing, error) {
	var outcome chat.Outcome
	switch row.State {
	case store.InteractionAnswered:
		outcome = chat.Answered
	case store.InteractionTimedOut:
		outcome = chat.TimedOut
	case store.InteractionCancelled:
		outcome = chat.Cancelled
	default:
		return chat.Outgoing{}, fmt.Errorf("an interaction that is %q has no replacement", row.State)
	}
	var in dialect.Interaction
	if err := json.Unmarshal(row.Request, &in); err != nil {
		return chat.Outgoing{}, fmt.Errorf("the stored request: %w", err)
	}
	var answers [][]string
	if outcome == chat.Answered {
		var a dialect.Answer
		if err := json.Unmarshal(row.Answer, &a); err != nil {
			return chat.Outgoing{}, fmt.Errorf("the stored answer: %w", err)
		}
		if in.Kind == dialect.ToolApproval { // by the request's kind, not the row's: a sub-agent's approval is stored as "nested"
			e, err := approvalEnding(row.Decider, row.DecidedAt, in, a.Decisions)
			if err != nil {
				return chat.Outgoing{}, err
			}
			return chat.End(e), nil
		}
		answers = a.Answers
	}
	e, err := endingOf(outcome, row.Decider, row.DecidedAt, in, answers)
	if err != nil {
		return chat.Outgoing{}, err
	}
	return chat.End(e), nil
}
