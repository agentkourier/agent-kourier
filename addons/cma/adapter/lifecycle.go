package adapter

import (
	"context"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	kitstore "github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

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
// A context the executor already has is left alone. The runtime calls it when this replica takes over as the leader,
// before it fails the turns a restart cut off and serves (runtime.Restorer); it fails, and stops the takeover, when the
// store or CMA cannot be read, or a change cannot be stored.
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
			e.sessions[contextID] = &contextSession{id: s.SessionID}
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
		if err := e.Store.ForgetSession(ctx, contextID, s.SessionID, now); err != nil {
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

// Interrupted sends user.interrupt to the session of each context whose task the runtime's settlement failed, so that
// a session the cut-off turn left running stops (runtime.Interrupter). It is best effort: a failure is logged, and the
// takeover goes on.
func (e *Executor) Interrupted(ctx context.Context, settled kitstore.Settlement) {
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
}

// ReleaseContext archives the session of a context the runtime's upkeep found idle past SESSION_IDLE_TTL, with no task
// running (runtime.ContextReleaser); the upkeep then forgets the context with its tasks from before. The session is the
// stored one, c.Ref, or, for a context whose session the store missed (a failed PutSession), the one in the map. A
// context with no session has nothing to archive, and is forgotten. A context whose session a turn here holds, or is
// creating, since the store listed it, is refused with runtime.ErrContextBusy. A session that fails to archive goes back
// in the map, unless the context has started a new one, for the next sweep.
func (e *Executor) ReleaseContext(ctx context.Context, c kitstore.Context) error {
	e.mu.Lock()
	if _, creating := e.creating[c.ContextID]; creating {
		e.mu.Unlock()
		return fmt.Errorf("context %s: a session is being created: %w", c.ContextID, runtime.ErrContextBusy)
	}
	id := c.Ref
	cs, mapped := e.sessions[c.ContextID]
	if id == "" && mapped {
		id = cs.id
	}
	if mapped && cs.id != id {
		mapped = false // the map's session is newer than the stored one, which is the one to archive
	}
	if mapped {
		if cs.turns > 0 {
			e.mu.Unlock()
			return fmt.Errorf("context %s: a turn runs in session %s: %w", c.ContextID, id, runtime.ErrContextBusy)
		}
		delete(e.sessions, c.ContextID)
	}
	e.mu.Unlock()
	if id == "" {
		return nil
	}
	if err := e.Backend.Archive(ctx, id); err != nil {
		if mapped {
			e.mu.Lock()
			if _, ok := e.sessions[c.ContextID]; !ok {
				e.sessions[c.ContextID] = cs
			}
			e.mu.Unlock()
		}
		return fmt.Errorf("archive session %s: %w", id, err)
	}
	e.logger().Info("archived an idle session", "session", id, "context", c.ContextID, "idle_ttl", e.IdleTTL)
	e.Metrics.SessionArchived(ArchiveIdle)
	return nil
}

// ValidateResume refuses an answer to a task paused on tool approvals that does not decide each of its calls once, in
// a tool_approval_response, with a2a.ErrInvalidParams, before the turn runs (runtime.ResumeValidator): Execute's error
// would fail the task, and the session would wait on the calls for good. Plain text never decides a call.
func (e *Executor) ValidateResume(_ context.Context, paused *a2a.Task, msg *a2a.Message) error {
	_, _, err := e.outbound(&a2asrv.ExecutorContext{Message: msg, StoredTask: paused})
	return err
}

// CanceledAfterFailedCancel accepts a cancel that failed when the task has ended canceled (runtime.CancelSettler). That
// happens when the session's idle after the interrupt reaches the turn first: the turn ends canceled (turnState), and
// the request handler, which hands the cancel to the running turn, then finds that turn gone.
func (e *Executor) CanceledAfterFailedCancel(_ context.Context, task *a2a.Task) bool {
	return task.Status.State == a2a.TaskStateCanceled
}

// FailedOutcome names a turn that failed waiting for a sandbox tool's result, whose status message carries the code
// timeout, turnTimeout (runtime.TurnClassifier). Any other failed turn is the runtime's failed.
func (e *Executor) FailedOutcome(status a2a.TaskStatus) telemetry.TurnOutcome {
	if status.Message != nil && status.Message.Metadata["code"] == "timeout" {
		return turnTimeout
	}
	return ""
}
