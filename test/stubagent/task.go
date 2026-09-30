package stubagent

// A2A 1.0 task states.
const (
	submitted = "TASK_STATE_SUBMITTED"
	working   = "TASK_STATE_WORKING"
	completed = "TASK_STATE_COMPLETED"
)

// adkExtension is the marker ADK's A2A server puts in the metadata of tasks and updates.
const adkExtension = "https://google.github.io/adk-docs/a2a/a2a-extension/"

// session is one contextId. A message with a new contextId, or none, opens one.
type session struct {
	id string
}

// task is one A2A task. Everything but the id and session is guarded by Server.mu.
//
// Maps and slices held here are replaced, never mutated, so a snapshot or an event that
// shares them stays valid while the task moves on.
type task struct {
	id      string
	sess    *session
	created string
	updated string
	state   string
	history []obj
	log     []event       // every stream event so far, in order; followers read from an index
	changed chan struct{} // closed and replaced whenever log grows
}

// event is one stream result. final marks the event that ends a stream: a task snapshot in a
// state where the agent is no longer running.
type event struct {
	res   obj
	final bool
}

// push appends an event and wakes the followers. The caller holds Server.mu.
func (t *task) push(res obj, final bool) {
	t.log = append(t.log, event{res, final})
	close(t.changed)
	t.changed = make(chan struct{})
}

// setState records a state change at time stamp. The caller holds Server.mu.
func (t *task) setState(state, stamp string) {
	t.state, t.updated = state, stamp
}

// snapshot is the task as GetTask returns it.
func (t *task) snapshot() obj {
	return obj{
		"id":        t.id,
		"contextId": t.sess.id,
		"status":    obj{"state": t.state, "timestamp": t.updated},
		"history":   t.history,
		"metadata":  obj{adkExtension: true, "kagent.dev/a2a/task-created-at": t.created},
	}
}

// statusEvent is a statusUpdate result for the task's current state.
func (t *task) statusEvent() obj {
	return obj{"statusUpdate": obj{
		"contextId": t.sess.id,
		"taskId":    t.id,
		"status":    obj{"state": t.state, "timestamp": t.updated},
		"metadata":  obj{adkExtension: true},
	}}
}
