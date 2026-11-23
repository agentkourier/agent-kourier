package cluster

import (
	"errors"
	"log/slog"
	"sync"
)

// ExitLeadershipLost is the exit code of a replica that led and no longer does: its lease could not be renewed within
// the renew deadline, or a write or a renewal found that another replica has taken it since. The process ends at once,
// without a drain, and Kubernetes restarts it as a standby. It is the broker's code for the same end.
const ExitLeadershipLost = 5

// ErrLeadershipLost is what Run returns when this replica stops leading: the caller ends the process with
// ExitLeadershipLost (FailStop.LeaseLost).
var ErrLeadershipLost = errors.New("leadership lost: the lease was taken or could not be renewed")

// FailStop ends the process at once, once, with a line that says why: the answer to a lost lease and to a write the
// store refused as deposed. A drain would let a replica that no longer leads keep running turns. Both fields are
// required.
type FailStop struct {
	// Log receives the line.
	Log *slog.Logger
	// Exit ends the process with its code: os.Exit, or a test's.
	Exit func(code int)

	once sync.Once
}

// Deposed stops the process after a write found that another replica has taken the lease: the store's OnDeposed.
func (f *FailStop) Deposed() { f.now("a write found that another replica has taken the lease") }

// LeaseLost stops the process after Elector.Run returned ErrLeadershipLost.
func (f *FailStop) LeaseLost() { f.now("the lease was lost or could not be renewed") }

func (f *FailStop) now(reason string) {
	f.once.Do(func() {
		f.Log.Error("leadership lost: stopping at once, without a drain", "reason", reason)
		f.Exit(ExitLeadershipLost)
	})
}
