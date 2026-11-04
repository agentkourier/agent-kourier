package crsource

import "time"

// SetClockForTest replaces the clock that dates a failed list or watch, which Stale measures against.
func (s *Source) SetClockForTest(now func() time.Time) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	s.now = now
}

// SetProbeForTest sets the pace and the timeout of the probe, before Start.
func (s *Source) SetProbeForTest(every, timeout time.Duration) {
	s.probeEvery, s.probeTimeout = every, timeout
}
