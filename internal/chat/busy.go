package chat

import (
	"context"
	"errors"
)

// ErrNoBusy is returned by BusyMarker.MarkBusy of a platform that cannot show a thread as busy, or will not for the rest
// of the process (Slack: it answers with a code that says the app, token or workspace cannot use the method). No retry mends it: a caller
// stops asking, and the platform has reported it already, so the caller says nothing more of it.
var ErrNoBusy = errors.New("chat: the platform cannot show a thread as busy")

// BusyMarker is an optional interface of a Platform: it shows a thread as busy, a sign that Agent Kourier is working in it
// that needs no message of its own, for a thread where the turn has posted nothing yet, such as an alert's. Caps.Busy says
// whether the platform does, so a caller asks first and a platform that shows none need not implement it.
//
// It is a courtesy and best effort: a platform may show the mark only to clients that are open when it is set,
// and may drop it when a message is posted in the thread. So a caller sets it when its work begins, sets it again after
// each write that it makes anyway (never on a timer), and clears it when the work ends, whatever the outcome.
type BusyMarker interface {
	// MarkBusy shows the thread t as busy if busy is set, and takes the mark off if not. Setting a mark that is there is
	// success, and so is clearing one that is not. The text of the mark is the platform's own, never the agent's. A target
	// with no thread is an error wrapping ErrInvalid. A platform that cannot says ErrNoBusy, every time from then on, and
	// reports why on its own. Any other error may pass on another try, and never ends a turn.
	MarkBusy(ctx context.Context, t Target, busy bool) error
}
