package e2e

import (
	"encoding/json"
	"strings"
)

// leaseLostReason is the fail-stop reason an Agent Kourier gives when its lease lapsed or could not be renewed (internal/broker, run.go): the
// leader stops, but no write of it was refused.
const leaseLostReason = "the lease was lost or could not be renewed"

// ExitReasons returns the reasons of the "leadership lost" lines of an Agent Kourier's log, as "message: reason": why it stopped as the leader.
// Nothing else of the log is returned, and a line that is not JSON is skipped.
func ExitReasons(log string) []string {
	var out []string
	for _, line := range strings.Split(log, "\n") {
		var e struct{ Msg, Reason string }
		if json.Unmarshal([]byte(line), &e) == nil && strings.Contains(e.Msg, "leadership lost") {
			out = append(out, e.Msg+": "+e.Reason)
		}
	}
	return out
}

// StoppedByFence says whether a deposed leader stopped because a write of it was refused: some reason is a fence's (the store's stale
// epoch, or the chat write's once Agent Kourier fences those), and not only the loss of the lease, which stops a leader that nothing refused.
func StoppedByFence(reasons []string) bool {
	for _, r := range reasons {
		if !strings.Contains(r, leaseLostReason) {
			return true
		}
	}
	return false
}
