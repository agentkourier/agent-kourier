//go:build !unix

package redact

import "time"

// cpuTime is the wall clock where the process's CPU time is not at hand.
func cpuTime() time.Duration { return time.Duration(time.Now().UnixNano()) }
