package interact

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// answerSuffix ends the ID of the message that sends an interaction's answer (AnswerMessage), so that the interaction is read
// off the message.
const answerSuffix = "/answer"

// reopened is a pause that the session opened again because the denial sent to it was not delivered.
type reopened struct {
	task a2a.TaskID
}

// Reopened is the session's watcher of answers (session.AnswerWatcher) for the denial of a timed-out approval. The session
// takes an answer and sends it afterwards, so the sweep that sent the denial is never told that it failed; a send that fails
// reopens the pause, and Reopened is told then. It keeps what it is told, for the Sweeper (SweepConfig.Reopened) to take up on
// its next sweep and cancel the task, which is how a denial that cannot be delivered ends in a cancel and not in a task paused
// behind a closed interaction until a restart. It is told only of answers that were not delivered, so a denial that was
// delivered is followed by nothing.
//
// A person's answer is left to the session alone: the interaction it answers is answered, not pending or timed out, and
// Reopened neither keeps it nor says that it tells the thread. The ownership is read from the interaction (a tool approval that
// is pending or timed out can only have been answered by the broker), so a restart loses nothing: the pause of a timed-out
// approval that comes again is denied again (Intake.Paused), and Reopened is told if that fails. It is safe for concurrent use.
type Reopened struct {
	st store.Interactions
	mu sync.Mutex
	// got is what it was told and the Sweeper has not taken, by interaction ID.
	got map[string]reopened
}

var _ session.AnswerWatcher = (*Reopened)(nil)

// NewReopened returns a Reopened that reads interactions from st.
func NewReopened(st store.Interactions) *Reopened {
	return &Reopened{st: st, got: map[string]reopened{}}
}

// AnswerReopened implements session.AnswerWatcher. It reports that it tells the thread (the Sweeper does, when it cancels the
// task) for the denial of an approval that is pending or timed out, and keeps the pause for the Sweeper. If the interaction
// cannot be read it keeps the pause too, to be judged on the Sweeper's own read, and says nothing is told, so that the
// session's notice stands. Any other answer is left alone. The stored interaction supplies the conversation key
// when the sweeper cancels; the watcher retains only the reported task ID to check against that row.
func (r *Reopened) AnswerReopened(ctx context.Context, _ session.Key, taskID a2a.TaskID, answerID string) bool {
	id, ok := strings.CutSuffix(answerID, answerSuffix)
	if !ok {
		return false
	}
	row, err := r.st.GetInteraction(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !denial(row)) {
		return false
	}
	r.mu.Lock()
	r.got[id] = reopened{task: taskID}
	r.mu.Unlock()
	return err == nil
}

// denial reports whether row is an interaction the broker answers by denial: a tool approval (the decoded request's Kind, never
// the row's) that is pending or timed out. A person's answer is recorded on the row before it is sent, which makes it
// answered.
func denial(row store.Interaction) bool {
	_, approval := toolApprovalOf(row)
	return approval && (row.State == store.InteractionPending || row.State == store.InteractionTimedOut)
}

// take hands over what it was told since the last call, by interaction ID.
func (r *Reopened) take() map[string]reopened {
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.got
	r.got = map[string]reopened{}
	return got
}
