package live

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/render"
)

// view is what the state shows at one moment.
type view struct {
	// m is the message of the update path: the text so far, and the latest tool call's status line as render.Turn shows it,
	// unless the updates carry steps, which say the same. post is m with the line: what the first post shows (posted).
	m, post chat.Message
	body    string // the text a stream carries
	// steps are the steps a stream shows, in the order of the calls, each in the state to show; and those of a turn that took
	// over a stream, or that is listed, which its updates carry (message). earlier is how many a listed turn shows no longer.
	steps   []chat.Step
	earlier int
	pos     []int // where in body each step follows: the text before that place was written before the call
	final   bool
	rated   bool // the final is the task's end, not a pause: the footer belongs on it
	ok      bool // there is something to show yet
}

// posted is the message the first post of v carries: post, since the outbox stores no steps, or "(no output)" for a final
// whose steps are all it shows, which the update that follows replaces with them.
func (v view) posted() chat.Message {
	if v.post.Fallback == "" && len(v.post.Parts) == 0 {
		return render.Nothing()
	}
	return v.post
}

// snapshot builds the state: the text so far as the body and, on the update path, the status line of the latest tool
// call, in the tense of how far it has got (render.Turn says when it is shown), and on the stream path the steps. A
// state with a status line is writable with no text, so a call that returned replaces the line that said it was under way.
// Once the turn is final the line is gone, and a step with no result is ended: the turn will not hear of it again. A turn
// that took over a stream, or that is listed, has the steps too, which its updates carry, and no status line in them, since
// its cards say the same (the first post, which carries no steps, keeps it):
// steps make it writable with no text, the final included: a pause, or an end, with no text shows the cards and nothing
// under them, as a stream that stops with steps and no text does. Only a turn with no text and no step is "(no output)".
//
// At a pause for a tool approval the step of each call it waits on is shown as waiting for the approval, done, whatever the
// call had reported (render.AwaitingApproval): kagent reports such a call with a "requires confirmation" error, and a step
// that is not done when the stream stops reads as failed. Such a step may go from failed to done, the one outcome that is
// changed once shown; the answer's message shows the call again, with how it ended.
func (u *Run) snapshot() view {
	carries := u.carries()
	u.mu.Lock()
	events := make([]agent.TextEvent, 0, len(u.order))
	for _, id := range u.order {
		events = append(events, agent.TextEvent{ArtifactID: id, Text: u.texts[id]})
	}
	tool, toolState, final, paused, awaiting := u.tool, u.toolState, u.final, u.paused, u.awaiting
	idle, begun, counts, note := u.idle, u.begun && !u.texted, u.counts(), u.note
	var recs []stepRec
	var dropped int
	if u.stepsOn() || carries {
		recs, dropped = slices.Clone(u.steps), u.dropped
	}
	u.mu.Unlock()
	var now time.Time
	if u.timed {
		now = u.cfg.Now()
	}
	v := view{final: final, rated: final && !paused && !u.cutShort, earlier: dropped}
	var shown *agent.ToolCallEvent
	var status string           // the update path's indicator that the agent is working
	if !final && !u.streaming { // on the stream path a tool call is a step, and has no status line
		shown = tool
		switch {
		case u.progress == v1alpha1.ProgressActivity: // names no tool
			shown = nil
			if begun {
				status = render.ActivityLine(now.Sub(u.started), counts)
			}
		case idle: // Working…, or the agent's progress line, takes the place of the line of a call that has returned
			shown, status = nil, cmp.Or(note, render.WorkingTitle)
		}
	}
	v.body = render.Body(events, u.caps.Limit(chat.WhereStream))
	v.m, v.ok = render.Turn(events, shown, toolState, u.caps)
	if status != "" {
		v.m, v.ok = render.WithStatus(v.m, status), true
	}
	if v.rated && v.body != "" && len(u.turn.Footer) > 0 { // a turn that ended with no text has no reply to rate, as a stream's stop says (stopFooter)
		v.m.Parts = append(slices.Clone(v.m.Parts), u.turn.Footer...)
	}
	v.post = v.m
	// The cards say what the line would, so the updates leave it out; but no card says what stands for Working… until one is
	// open, and a progress line begins none (progressIn): that line stays, under the cards, until the delay's card has it.
	if n := len(recs); carries && (shown != nil || status != "") && (!idle || (n > 0 && recs[n-1].open())) {
		v.m, v.ok = render.Turn(events, nil, toolState, u.caps)
	}
	for i, r := range recs {
		id := fmt.Sprintf("s%d", dropped+i+1) // a step keeps its ID when the ones before it are dropped
		if _, sent := u.sentOf(id); final && r.kind != callStep && v.body == "" && !sent {
			continue // a card for time that was never shown is not content of a turn with no text: no stream is made for it
		}
		state := r.state
		if final && state == chat.StepRunning {
			state = chat.StepEnded
		}
		before := render.Body(events[:min(r.after, len(events))], u.caps.Limit(chat.WhereStream))
		pos := len(v.body)
		if strings.HasPrefix(v.body, before) {
			pos = len(before)
		}
		if r.kind == callStep && r.call.CallID != "" && slices.Contains(awaiting, r.call.CallID) {
			v.steps = append(v.steps, render.AwaitingApproval(id, r.call))
		} else {
			v.steps = append(v.steps, u.stepOf(id, r, state, now, counts, note))
		}
		v.pos = append(v.pos, pos)
	}
	v.ok = v.ok || len(v.steps) > 0
	return v
}

// stepOf is the step a record shows in the state given: the call it is, or the card that stands for time. A call with no
// result at the final is ended with them (state); a card that stands for time has its own ending. A Working… card under way
// is titled with the agent's progress line, if there is one: only the last card can be under way.
func (u *Run) stepOf(id string, r stepRec, state chat.StepState, now time.Time, counts render.Counts, note string) chat.Step {
	switch r.kind {
	case workingStep:
		st := render.WorkingEnded(id, r.ending)
		if r.ending == render.NotEnded && note != "" {
			st.Title = note
		}
		return st
	case activityStep:
		if r.ending == render.NotEnded {
			return render.ActivityStep(id, counts, now.Sub(u.started), r.ending)
		}
		return render.ActivityStep(id, r.counts, r.endedAt.Sub(u.started), r.ending)
	}
	return render.Step(id, r.call, state)
}

// stepsOn says whether the stream shows steps: the turn is eligible for them (a stream, on a platform that has them),
// it is still the stream it began as, and the platform has not refused one.
func (u *Run) stepsOn() bool { return u.eligible && u.streaming && !u.stepsOff }

// carries says whether the turn's updates carry its steps: it is eligible for them, it is listed or it took over the stream
// of an earlier run, which is the message it updates (a platform such as Slack drops a stream's cards from a message it
// updates), and the platform has not refused a message with them.
func (u *Run) carries() bool {
	return u.eligible && (u.listed || u.adopted) && !u.streaming && !u.stepsOff
}
