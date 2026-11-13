package live

import (
	"slices"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/render"
)

// stepRec is a tool call of a stream, which is a step: the call, how it ended, and where in the text it was made.
type stepRec struct {
	call  agent.ToolCallEvent
	state chat.StepState
	after int // how many text artifacts there were when the call was made: the step follows their text
	kind  stepKind
	// ending is how a card that stands for time ended; NotEnded while it is under way. Only a card under way can be taken over.
	ending render.Ending
	// An activity card is ended with what it counted by then, and when.
	counts  render.Counts
	endedAt time.Time
}

// stepKind is what a step stands for. The zero value is a tool call.
type stepKind int

const (
	// callStep is a tool call: the call, and how it ended.
	callStep stepKind = iota
	// workingStep is a Working… card for a gap. The next tool call takes it over and it becomes that call's step; text, or the
	// end of the turn, ends it under a title of its own (render.WorkingEnded), which a tool call after the text changes.
	workingStep
	// activityStep is the one card of the activity mode, which shows counts and no call; it ends at the first text or the end
	// of the turn, with what it counted then.
	activityStep
)

// noteCall makes e the latest call, under way: the update path's status line is about it. A call that is reported
// again after its result keeps the outcome it has, so that the line does not go back to a call under way that nothing
// would end. The caller holds u.mu.
func (u *Run) noteCall(e agent.ToolCallEvent) {
	if u.tool != nil && e.CallID != "" && u.tool.CallID == e.CallID {
		u.tool = &e
		return
	}
	u.tool, u.toolState = &e, chat.ToolCalling
}

// beginStep records a tool call as a step, once: a call that is reported again is the same step, and
// a call with no ID, which no result can name, is a step of its own. A call that comes while the last step is a
// Working… card that nothing has claimed takes that card over, so a gap and the call that ended it are one card; it
// costs no step beyond the card. The activity mode shows no step for a call. The caller holds u.mu.
func (u *Run) beginStep(e agent.ToolCallEvent) {
	if !u.eligible || u.progress == v1alpha1.ProgressActivity || (e.CallID != "" && (u.stepIndex(e.CallID) >= 0 || u.gone[e.CallID])) {
		return
	}
	if n := len(u.steps); n > 0 && u.steps[n-1].open() { // a Working… card that is still unclaimed: the call is that card
		u.steps[n-1].call, u.steps[n-1].kind = e, callStep
		return
	}
	u.addStep(stepRec{call: e, after: len(u.order)})
}

// addStep records r as the latest step. A stream shows the first maxSteps and no more. A listed turn keeps the latest
// maxListed: the oldest makes way, and is counted (dropped), and its call is remembered (gone). The caller holds u.mu.
func (u *Run) addStep(r stepRec) {
	switch {
	case !u.listed && len(u.steps) >= maxSteps:
		return
	case u.listed && len(u.steps) >= maxListed:
		if id := u.steps[0].call.CallID; id != "" {
			u.gone[id] = true
		}
		u.steps, u.dropped = u.steps[1:], u.dropped+1
	}
	u.steps = append(u.steps, r)
}

// endStep finishes the step of a tool call that has its result: done, or failed if the response says so (render.Failed).
// A result of a call that is no step, or of one that has its result already, changes nothing. The caller holds u.mu.
func (u *Run) endStep(e agent.ToolResultEvent) {
	if i := u.stepIndex(e.CallID); i >= 0 && u.steps[i].state == chat.StepRunning {
		u.steps[i].state = chat.StepDone
		if render.Failed(e.Response) {
			u.steps[i].state = chat.StepFailed
		}
	}
}

// callMade notes a tool call for the activity indicators: a call with an ID is under way until its result, and a call under
// way is no gap. A call with no ID can never have a result, so it is under way until the next event and is counted as it
// arrives. A call that is reported again, with an ID the run has, changes nothing. In the activity mode the first tool event
// begins it. The caller holds u.mu.
func (u *Run) callMade(e agent.ToolCallEvent, now time.Time) {
	if e.CallID == "" {
		u.anon = true
		u.finished++
	} else {
		if _, known := u.calls[e.CallID]; known {
			return
		}
		u.calls[e.CallID] = false
		u.inflight++
		u.anon = false
	}
	u.idle = false // the tool's own line takes the place of Working…
	u.followText()
	u.beginActivity()
	u.reopenGap(now)
}

// resultIn counts the result of a tool call the run knows, once, and ends the call's time as a call under way: a new gap
// begins if no other call is. The caller holds u.mu.
func (u *Run) resultIn(e agent.ToolResultEvent, now time.Time) {
	u.anon = false
	if done, known := u.calls[e.CallID]; known && !done {
		u.calls[e.CallID] = true
		u.inflight--
		u.finished++
		if render.Failed(e.Response) {
			u.failed++
		}
	}
	u.beginActivity()
	u.reopenGap(now)
}

// progressIn takes a working update (spec D45 (1)), which is activity whether or not anything in it is understood; line is
// its progress line (render.ProgressLine), in steps-and-activity only. Being the next event, any update ends a call with no ID,
// which was under way until then, and begins the gap that such a call held off. Otherwise an update leaves the gap running: were
// it to begin the gap again, an agent that sends updates more often than the delay would never show that it is working. It
// begins no card and no line itself either: a card stays once sent, and a server may send an update or two just before the text
// that would end such a card, as kagent 1.0 does before an approved turn's answer and kagent 0.9.x with the answer itself. The
// activity mode counts the update, and shows none of its text, since that mode names nothing the agent does. In
// steps-and-activity an update with a line, where Working… may show (no call with an ID under way), stands for Working…
// until the next event: it titles the open Working… card, or the card the delay begins, and on a message that is updated it
// is the line at once (idle) while no card is open, which the next write can take back. Which of the two the run shows is the
// path's, so this holds for a turn that moved from the stream to the update path as well. The caller holds u.mu.
func (u *Run) progressIn(line string, now time.Time) {
	if u.anon {
		u.anon = false
		u.reopenGap(now)
	}
	if u.progress == v1alpha1.ProgressActivity {
		u.updates++
		return
	}
	if line == "" || u.final || u.inflight > 0 {
		return
	}
	u.note, u.idle = line, true
}

// reopenGap begins a gap at now, in which the run waits the delay for something new before it shows that it is working. No
// gap runs while a call is under way (its own card, or line, is the sign), once the turn is final, or, in the activity
// mode, once the indicator has begun or the first text has come: the indicator is for the time before the text, it
// begins once, and it is rewritten by events and never by the clock. The caller holds u.mu.
func (u *Run) reopenGap(now time.Time) {
	over := u.progress == v1alpha1.ProgressActivity && (u.texted || u.begun)
	u.gapOpen = !u.final && u.inflight == 0 && !u.anon && !over
	if u.gapOpen {
		u.gapAt = now
		u.gapSeq++
	}
}

// beginActivity begins the activity mode's indicator, once, if no text has come: the one card of a stream, or the line of a
// message that is updated. The caller holds u.mu.
func (u *Run) beginActivity() {
	if u.progress != v1alpha1.ProgressActivity || u.texted || u.begun {
		return
	}
	u.begun = true
	if u.eligible {
		u.addStep(stepRec{kind: activityStep, after: len(u.order)})
	}
}

// openIdle is the delay timer firing for the gap seq: if that gap is still the gap and has run the delay, the run shows
// that it is working, which is a Working… card unless the platform has no cards, and the line of a message that is
// updated. It is the one thing a clock writes; the gap is then used, and a new one begins with the next event. A gap that
// events have ended since the timer was set shows nothing. now is read by the caller.
func (u *Run) openIdle(seq int, now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.gapOpen || u.gapSeq != seq || now.Before(u.gapAt.Add(u.cfg.ActivityDelay)) {
		return
	}
	u.gapOpen = false
	if u.progress == v1alpha1.ProgressActivity {
		u.beginActivity()
		return
	}
	u.idle = true
	if n := len(u.steps); u.eligible && (n == 0 || !u.steps[n-1].open()) {
		u.addStep(stepRec{kind: workingStep, after: len(u.order)})
	}
}

// endWorking ends what stands for time, as e says, when text comes or the turn ends: a Working… card that no call claimed,
// and the activity card, with the counts and the time it stands at. A card ends once. Only text that came while a Working…
// card was open says that it wrote the answer, until a tool call follows that text (followText); a final that ends it says how
// the task ended. The caller holds u.mu.
func (u *Run) endWorking(now time.Time, e render.Ending) {
	n := len(u.steps)
	if n == 0 || u.steps[n-1].ending != render.NotEnded {
		return
	}
	switch r := &u.steps[n-1]; r.kind {
	case workingStep:
		r.ending = e
	case activityStep:
		r.ending, r.counts, r.endedAt = e, u.counts(), now
	}
}

// counts is what the activity mode shows it counted: the tool calls that have their result and the failures among them, and
// the working updates until the first of those, so that what it shows never counts backwards. The caller holds u.mu.
func (u *Run) counts() render.Counts {
	c := render.Counts{Steps: u.finished, Failed: u.failed}
	if u.finished == 0 {
		c.Updates = u.updates
	}
	return c
}

// followText makes the text that ended a Working… card an update, now that a tool call follows it: the card said that it
// wrote the answer while nothing but text came after (render.WroteTitle), and the answer is what comes after this call. The
// caller holds u.mu.
func (u *Run) followText() {
	for i := range u.steps {
		if r := &u.steps[i]; r.kind == workingStep && r.ending == render.EndedByText {
			r.ending = render.EndedByUpdate
		}
	}
}

// arm returns the timer for the open gap, the one it was given if that is still the gap's, and nil if no gap is open. seq
// is the gap the timer is for. It is the run's goroutine's alone.
func (u *Run) arm(timer <-chan time.Time, seq int) (<-chan time.Time, int) {
	u.mu.Lock()
	open, at, cur := u.gapOpen, u.gapAt, u.gapSeq
	u.mu.Unlock()
	switch {
	case !open:
		return nil, seq
	case timer != nil && cur == seq:
		return timer, seq
	}
	return u.cfg.After(max(at.Add(u.cfg.ActivityDelay).Sub(u.cfg.Now()), 0)), cur
}

// open says the step is a Working… card that nothing has claimed or ended.
func (r stepRec) open() bool { return r.kind == workingStep && r.ending == render.NotEnded }

// stepIndex is the place of the step of the call callID, or -1. The caller holds u.mu.
func (u *Run) stepIndex(callID string) int {
	if callID == "" {
		return -1
	}
	return slices.IndexFunc(u.steps, func(s stepRec) bool { return s.call.CallID == callID })
}
