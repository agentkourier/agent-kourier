package stubagent

import (
	"context"
	"time"
)

// Step is one thing an agent does during a turn. A turn is a list of steps, played in order;
// it ends with the task COMPLETED unless a step pauses it or the task is cancelled.
type Step interface {
	// play runs the step and reports whether the turn goes on.
	play(r *run) bool
}

// Text streams text as kagent does: one artifact, its first chunk without append, the others
// with append: true, then a lastChunk event holding the full text with no append. Consecutive
// Text steps feed the same artifact. With WithTick the server pauses between chunks.
func Text(chunks ...string) Step { return textStep(chunks) }

type textStep []string

func (ts textStep) play(r *run) bool {
	for _, c := range ts {
		if !r.chunk(c) {
			return false
		}
	}
	return true
}

// Tool records a tool call and its result as kagent does: each is its own data-part artifact,
// a single lastChunk event, apart from the text. It ends the open text artifact.
func Tool(name string, args, result map[string]any) Step { return toolStep{name, args, result} }

type toolStep struct {
	name         string
	args, result obj
}

func (ts toolStep) play(r *run) bool {
	id := r.s.newID("call")
	return r.endText() &&
		r.data(obj{"id": id, "name": ts.name, "args": ts.args}, "function_call") &&
		r.data(obj{"id": id, "name": ts.name, "response": ts.result}, "function_response")
}

// data emits a one-part data artifact, of kind function_call or function_response.
func (r *run) data(data obj, kind string) bool {
	return r.emit(func(t *task) (obj, bool) {
		part := obj{"data": data, "metadata": obj{"kagent.dev/a2a/part-type": kind}}
		a := &artifact{id: r.s.newID("artifact"), stamp: r.s.stamp(), parts: []obj{part}}
		t.arts = append(t.arts, a)
		return t.artifactEvent(a, a.parts, false, true), false
	})
}

// Hold makes the turn wait at a gate until Server.Release opens it. It lets a test freeze a
// task mid-turn, with the text so far, and inspect it.
func Hold(gate string) Step { return holdStep(gate) }

type holdStep string

func (h holdStep) play(r *run) bool {
	r.s.mu.Lock()
	gate := r.s.gateLocked(string(h))
	r.s.mu.Unlock()
	select {
	case <-gate:
		return true
	case <-r.ctx.Done():
		return false
	}
}

// run is one playing turn of a task.
type run struct {
	s    *Server
	t    *task
	ctx  context.Context
	open *artifact // the text artifact being streamed, if any
	text string    // what it holds so far
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

// chunk streams one text chunk, after the tick pause unless it is the first.
func (r *run) chunk(text string) bool {
	if r.open != nil && !r.sleep(r.s.tick) {
		return false
	}
	return r.emit(func(t *task) (obj, bool) {
		appendTo := r.open != nil
		if !appendTo {
			r.open, r.text = &artifact{id: r.s.newID("artifact"), stamp: r.s.stamp()}, ""
			t.arts = append(t.arts, r.open)
		}
		r.text += text
		r.open.parts = textParts(r.text)
		return t.artifactEvent(r.open, textParts(text), appendTo, false), false
	})
}

// endText closes the open text artifact with its lastChunk event.
func (r *run) endText() bool {
	if r.open == nil {
		return true
	}
	a := r.open
	r.open = nil
	return r.emit(func(t *task) (obj, bool) { return t.artifactEvent(a, a.parts, false, true), false })
}

// pause ends the turn with the task INPUT_REQUIRED, the snapshot as the last event.
func (r *run) pause(p *pause) bool {
	return r.emit(func(t *task) (obj, bool) {
		p.stamp = r.s.stamp()
		t.pause = p
		t.setState(inputRequired, p.stamp)
		return obj{"task": t.snapshot()}, true
	})
}

// finish ends the turn in a final state, with the task snapshot as the last event.
func (r *run) finish(state string) {
	if !r.endText() {
		return
	}
	r.emit(func(t *task) (obj, bool) {
		t.setState(state, r.s.stamp())
		return obj{"task": t.snapshot()}, true
	})
}

// sleep waits d, or until the turn is stopped. It reports whether the turn goes on.
func (r *run) sleep(d time.Duration) bool {
	if d <= 0 {
		return r.ctx.Err() == nil
	}
	select {
	case <-time.After(d):
		return true
	case <-r.ctx.Done():
		return false
	}
}
