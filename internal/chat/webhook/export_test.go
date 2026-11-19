package webhook

import "time"

// The bounds of the platform, for the external tests.
const (
	AckWait            = ackWait
	BusyRetryAfter     = busyRetryAfter
	MaxIDValue         = maxIDValue
	MinTokenLen        = minTokenLen
	UnverifiedLogEvery = unverifiedLogEvery
)

// SetAckWait sets how long the handler waits for the sink, for a test that cannot wait for the default. It is set before the handler
// is first called.
func (p *Platform) SetAckWait(d time.Duration) { p.ackWait = d }

// Running says whether a Run takes requests.
func (p *Platform) Running() bool { return p.run.Load() != nil }

// SetAfterLoad sets what the handler calls once it has loaded the Run to hand an event to. It is set before the handler is first
// called.
func (p *Platform) SetAfterLoad(f func()) { p.afterLoad = f }

// Series is every outcome and reason the platform counts, as "outcome reason".
func Series() []string {
	var out []string
	for _, s := range requestSeries {
		out = append(out, s.outcome+" "+s.reason)
	}
	return out
}
