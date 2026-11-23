package adapter

import (
	"context"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
)

// reapInterval is how often Run looks for idle sessions and old tasks. A session can outlive IdleTTL, and a task
// TaskRetention, by up to this much.
const reapInterval = time.Minute

// InterruptedMessage is the agent message of a task that a restart failed.
const InterruptedMessage = "Interrupted by an adapter restart."

// Restore loads each context's session from the store and reconciles it with the sessions this adapter made in CMA,
// which carry their context ID in their metadata:
//
//   - The stored session stays while CMA still lists it unarchived. CMA's other sessions for the context, which only a
//     crash between creating a session and storing it can leave, are archived.
//   - A context the store lacks, whose session CMA lists, adopts it (the one active last, when there are several; the
//     others are archived), and the store records it.
//   - A stored session that CMA no longer lists (archived, terminated, or in another environment) is forgotten, with
//     the context's tasks: the context's next message starts a new session.
//
// A context the executor already has is left alone. Call it before serving; it fails when the store or CMA cannot be
// read, or a change cannot be stored.
func (e *Executor) Restore(ctx context.Context) error {
	stored, err := e.Store.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("read the stored sessions: %w", err)
	}
	infos, err := e.Backend.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	listed := map[string][]SessionInfo{}
	for _, info := range infos {
		listed[info.ContextID] = append(listed[info.ContextID], info)
	}
	known := map[string]store.Session{}
	for _, s := range stored {
		known[s.ContextID] = s
	}
	now := e.clock()
	keep := map[string]store.Session{}
	adopted := map[string]bool{}
	var superseded []string
	for contextID, list := range listed {
		chosen := -1
		if s, ok := known[contextID]; ok {
			for i, info := range list {
				if info.ID == s.SessionID {
					chosen = i
				}
			}
		}
		if chosen < 0 {
			adopted[contextID] = true
			chosen = 0
			for i, info := range list {
				if info.LastActive.After(list[chosen].LastActive) {
					chosen = i
				}
			}
		}
		for i, info := range list {
			if i != chosen {
				superseded = append(superseded, info.ID)
			}
		}
		last := list[chosen].LastActive
		if s, ok := known[contextID]; ok && !adopted[contextID] {
			last = s.LastActive
		}
		if last.IsZero() { // no activity time: the idle clock starts now rather than archiving at once
			last = now
		}
		keep[contextID] = store.Session{ContextID: contextID, SessionID: list[chosen].ID, LastActive: last}
	}

	e.mu.Lock()
	if e.sessions == nil {
		e.sessions = map[string]*contextSession{}
	}
	var add []store.Session
	for contextID, s := range keep {
		if _, ok := e.sessions[contextID]; !ok {
			e.sessions[contextID] = &contextSession{id: s.SessionID, lastActive: s.LastActive}
			add = append(add, s)
		}
	}
	e.mu.Unlock()

	adoptions := 0
	for _, s := range add {
		if !adopted[s.ContextID] {
			continue
		}
		if err := e.Store.PutSession(ctx, s); err != nil {
			return fmt.Errorf("store an adopted session: %w", err)
		}
		adoptions++
	}
	forgotten := 0
	for contextID, s := range known {
		if _, ok := keep[contextID]; ok {
			continue
		}
		e.logger().Info("forgetting a context whose session CMA no longer lists", "context", contextID, "session", s.SessionID)
		if err := e.Store.ForgetContext(ctx, contextID, s.SessionID, now); err != nil {
			return fmt.Errorf("forget context %s: %w", contextID, err)
		}
		e.Metrics.SessionArchived(ArchiveGone)
		forgotten++
	}
	for _, id := range superseded {
		if err := e.Backend.Archive(ctx, id); err != nil {
			e.logger().Warn("archive a superseded session", "session", id, "error", err)
			continue
		}
		e.Metrics.SessionArchived(ArchiveSuperseded)
	}
	e.logger().Info("restored sessions", "contexts", len(add), "adopted", adoptions, "forgotten", forgotten, "superseded", len(superseded))
	return nil
}

// Settle fails the tasks a restart left submitted or working, with the agent message InterruptedMessage, and sends
// their sessions an interrupt (best effort: a failure is logged). A task paused on an approval (input-required)
// survives, and its answer reaches the paused calls. Call it after Restore and before serving. The store fails each
// task in a write of its own: one whose record does not decode by its state alone, and one that changed since it was
// read it logs and leaves. Settle fails when the store cannot be read or written.
func (e *Executor) Settle(ctx context.Context) error {
	settled, err := e.Store.SettleInterrupted(ctx, InterruptedMessage)
	e.Telemetry.TasksInterrupted(settled.Interrupted())
	if err != nil {
		return err
	}
	contexts := map[string]bool{}
	for _, task := range settled.Failed {
		contexts[task.ContextID] = true
	}
	for _, task := range settled.Unreadable {
		contexts[task.ContextID] = true
	}
	for contextID := range contexts {
		e.mu.Lock()
		cs, ok := e.sessions[contextID]
		e.mu.Unlock()
		if !ok {
			continue
		}
		if err := e.Backend.Send(ctx, cs.id, Outbound{Interrupt: true}); err != nil {
			e.logger().Warn("interrupt the session of an interrupted task", "context", contextID, "session", cs.id, "error", err)
		}
	}
	if settled.Interrupted() > 0 || settled.Skipped > 0 {
		e.logger().Info("failed the tasks a restart interrupted", "tasks", settled.Interrupted(),
			"unreadable", len(settled.Unreadable), "skipped", settled.Skipped)
	}
	return nil
}

// Run archives the sessions of contexts idle past IdleTTL, and deletes the settled tasks older than TaskRetention,
// once a minute until ctx ends. It returns at once when both are zero.
func (e *Executor) Run(ctx context.Context) {
	if e.IdleTTL <= 0 && e.TaskRetention <= 0 {
		return
	}
	tick := time.NewTicker(reapInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.reap(ctx)
			e.prune(ctx)
		}
	}
}

// reap archives each session whose context has had no turn running for longer than IdleTTL, and forgets it with the
// context's tasks. A session it fails to archive goes back in the map, unless its context has started a new one, for
// the next reap.
func (e *Executor) reap(ctx context.Context) {
	if e.IdleTTL <= 0 {
		return
	}
	type expired struct{ contextID, session string }
	var due []expired
	now := e.clock()
	e.mu.Lock()
	for contextID, cs := range e.sessions {
		if cs.turns == 0 && now.Sub(cs.lastActive) > e.IdleTTL {
			due = append(due, expired{contextID, cs.id})
			delete(e.sessions, contextID)
		}
	}
	e.mu.Unlock()
	for _, d := range due {
		log := e.logger().With("session", d.session, "context", d.contextID)
		if err := e.Backend.Archive(ctx, d.session); err != nil {
			log.Warn("archive an idle session; will retry", "error", err)
			e.mu.Lock()
			if _, ok := e.sessions[d.contextID]; !ok {
				e.sessions[d.contextID] = &contextSession{id: d.session, lastActive: now.Add(-e.IdleTTL - time.Nanosecond)}
			}
			e.mu.Unlock()
			continue
		}
		log.Info("archived an idle session", "idle_ttl", e.IdleTTL)
		e.Metrics.SessionArchived(ArchiveIdle)
		// The tasks before now ran in the archived session and can never run again; a turn that started since runs in
		// a new session and is kept. Should this fail, Restore forgets the context at the next start.
		if err := e.Store.ForgetContext(ctx, d.contextID, d.session, now); err != nil {
			log.Warn("forget an archived session's context", "error", err)
		}
	}
}

// prune deletes the settled tasks last updated more than TaskRetention ago, with their message IDs.
func (e *Executor) prune(ctx context.Context) {
	if e.TaskRetention <= 0 {
		return
	}
	n, err := e.Store.DeleteSettledBefore(ctx, e.clock().Add(-e.TaskRetention))
	if err != nil {
		e.logger().Warn("delete old settled tasks", "error", err)
		return
	}
	if n > 0 {
		e.logger().Info("deleted old settled tasks", "tasks", n, "task_retention", e.TaskRetention)
	}
}
