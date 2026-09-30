package stubagent

// Step is one thing an agent does during a turn. A turn is a list of steps, played in order;
// it ends with the task COMPLETED unless a step pauses it or the task is cancelled.
type Step interface {
	// play runs the step and reports whether the turn goes on.
	play(r *run) bool
}

// run is one playing turn of a task.
type run struct {
	s *Server
	t *task
}

// emit changes the task and logs an event, under the lock. It returns false, doing nothing,
// once the task has stopped working (it was cancelled), which ends the turn.
func (r *run) emit(fn func(t *task) (res obj, final bool)) bool {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.t.state != working {
		return false
	}
	res, final := fn(r.t)
	r.t.push(res, final)
	return true
}

// finish ends the turn in a final state, with the task snapshot as the last event.
func (r *run) finish(state string) {
	r.emit(func(t *task) (obj, bool) {
		t.setState(state, r.s.stamp())
		return obj{"task": t.snapshot()}, true
	})
}
