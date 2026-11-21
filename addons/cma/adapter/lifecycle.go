package adapter

import (
	"context"
	"fmt"
	"time"
)

// reapInterval is how often Run looks for idle sessions. A session can outlive IdleTTL by up to this much.
const reapInterval = time.Minute

// Restore rebuilds the context-to-session map from the sessions this adapter made, so a restart continues each
// context in its own session instead of leaving that session behind. When two sessions name one context, which only
// a race between two adapters can cause, the one active last is kept and the others are archived. A context the map
// already has is left alone. Call it before serving.
func (e *Executor) Restore(ctx context.Context) error {
	infos, err := e.Backend.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	latest := map[string]SessionInfo{}
	var superseded []string
	for _, info := range infos {
		cur, ok := latest[info.ContextID]
		switch {
		case !ok:
			latest[info.ContextID] = info
		case info.LastActive.After(cur.LastActive):
			superseded = append(superseded, cur.ID)
			latest[info.ContextID] = info
		default:
			superseded = append(superseded, info.ID)
		}
	}
	e.mu.Lock()
	if e.sessions == nil {
		e.sessions = map[string]*contextSession{}
	}
	restored, now := 0, e.clock()
	for contextID, info := range latest {
		if _, ok := e.sessions[contextID]; !ok {
			last := info.LastActive
			if last.IsZero() { // no activity time: the idle clock starts now rather than archiving at once
				last = now
			}
			e.sessions[contextID] = &contextSession{id: info.ID, lastActive: last}
			restored++
		}
	}
	e.mu.Unlock()
	for _, id := range superseded {
		if err := e.Backend.Archive(ctx, id); err != nil {
			e.logger().Warn("archive a superseded session", "session", id, "error", err)
		}
	}
	e.logger().Info("restored sessions", "contexts", restored, "superseded", len(superseded))
	return nil
}

// Run archives the sessions of contexts idle past IdleTTL until ctx ends. It returns at once when IdleTTL is zero.
func (e *Executor) Run(ctx context.Context) {
	if e.IdleTTL <= 0 {
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
		}
	}
}

// reap archives each session whose context has had no turn running for longer than IdleTTL, and forgets it. A
// session it fails to archive goes back in the map, unless its context has started a new one, for the next reap.
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
	}
}
