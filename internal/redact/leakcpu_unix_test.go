//go:build unix

package redact

import (
	"syscall"
	"time"
)

// cpuTime is the CPU time the process has used, user and system: a run that the machine makes wait, because others are using it,
// takes no more of it, so the time of a run is as steady on a loaded runner as on an idle one.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return time.Duration(time.Now().UnixNano())
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
