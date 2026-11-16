package web

import "time"

// The bounds of the platform, for the external tests and the guards of the limits page.
const (
	AckWait       = ackWait
	ReplyTimeout  = replyTimeout
	MaxRetryAfter = maxRetryAfter
)

// SetAckWait sets how long the handler waits for the sink, for a test that cannot wait for the default. It is set before the handler
// is first called.
func (p *Platform) SetAckWait(d time.Duration) { p.ackWait = d }

// Running says whether a Run takes requests.
func (p *Platform) Running() bool { return p.run.Load() != nil }

// ClientTimeout is the bound of the client that sends the replies.
func (p *Platform) ClientTimeout() time.Duration { return p.client.Timeout }
