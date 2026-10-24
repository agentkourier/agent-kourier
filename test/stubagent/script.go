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
// Text steps feed the same artifact, and an empty chunk is skipped. With WithTick the server
// pauses between chunks.
func Text(chunks ...string) Step { return textStep(chunks) }

type textStep []string

func (ts textStep) play(r *run) bool {
	for _, c := range ts {
		if c != "" && !r.chunk(c) {
			return false
		}
	}
	return true
}

// Tool records a tool call and its result as kagent does: each is its own data-part artifact,
// a single lastChunk event, apart from the text. It ends the open text artifact.
// UNCONFIRMED: that text followed by a tool call is closed with its lastChunk event before the
// call; the fixtures have no text ahead of a tool call.
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

// RefuseAnswer refuses the answer instead of taking it, when it is among the steps that the then of Ask or
// RequestApproval returns: the answer's send waits at the gate until Server.Release opens it, holding no lock, so
// the task can be read and other calls served meanwhile, and then answers -32004 with msg, as kagent answers a send
// that its session cannot take. No step of the continuation runs, the answer is not in the task's history, and the
// task still waits for input with the same request. A client that leaves before the gate opens gets nothing. The
// steps of a continuation run only once the answer is taken, so Hold cannot delay the send's answer; this step
// does. In a turn that no answer resumed it has nothing to refuse, and it ends the task FAILED, so the mistake shows.
func RefuseAnswer(gate, msg string) Step { return refuseStep{gate, msg} }

type refuseStep struct{ gate, msg string }

func (refuseStep) play(r *run) bool {
	r.finish(failed, "stubagent: RefuseAnswer outside the steps that an answer resumes")
	return false
}

// refusal returns the first RefuseAnswer of steps, if there is one.
func refusal(steps []Step) (refuseStep, bool) {
	for _, st := range steps {
		if rf, ok := st.(refuseStep); ok {
			return rf, true
		}
	}
	return refuseStep{}, false
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
			r.open, r.text = &artifact{id: r.s.newID("artifact")}, ""
			t.arts = append(t.arts, r.open)
		}
		r.text += text
		r.open.parts, r.open.stamp = textParts(r.text), r.s.stamp() // each event has its own position
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
	return r.emit(func(t *task) (obj, bool) {
		a.stamp = r.s.stamp()
		return t.artifactEvent(a, a.parts, false, true), false
	})
}

// pause ends the turn with the task INPUT_REQUIRED, the snapshot as the last event.
func (r *run) pause(p *pause) bool {
	return r.emit(func(t *task) (obj, bool) {
		p.stamp, p.ext = r.s.stamp(), t.ext
		t.pause = p
		t.setState(inputRequired, p.stamp)
		return obj{"task": t.snapshot()}, true
	})
}

// Fail ends the turn with the task FAILED and reason as its status message, as kagent's does when the
// model call fails. The text said before it stays in the task's output, and steps after it do not run.
func Fail(reason string) Step { return endStep{failed, reason} }

// Reject ends the turn with the task REJECTED and reason as its status message. The text said before it
// stays in the task's output, and steps after it do not run.
func Reject(reason string) Step { return endStep{rejected, reason} }

type endStep struct{ state, reason string }

func (e endStep) play(r *run) bool {
	r.finish(e.state, e.reason)
	return false
}

// finish ends the turn in a final state, with the task snapshot as the last event. reason, if any, is
// the status message of the task.
func (r *run) finish(state, reason string) {
	if !r.endText() {
		return
	}
	r.emit(func(t *task) (obj, bool) {
		t.setState(state, r.s.stamp())
		t.note = reason
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
