package slack

import (
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/lanes"
)

// MaxChannelBacklog is the events a channel's lane holds, for the external tests that fill one.
const MaxChannelBacklog = lanes.PerChannel

// MaxPendingEvents is the events all lanes hold.
const MaxPendingEvents = lanes.Total

// HTTPAckWait is how long the handler of a request in HTTP mode waits for the sink by default.
const HTTPAckWait = httpAckWait

// SetHTTPAckWait sets how long the handler of a request in HTTP mode waits for the sink, for a test that cannot wait for the default.
// It is set before the handler is first called.
func (c *Client) SetHTTPAckWait(d time.Duration) { c.ackWait = d }

// PendingEvents is the events the lanes of the Run that is going hold: the one being handled and those waiting. It is zero
// before Run has begun.
func (c *Client) PendingEvents() int {
	if l := c.lanes.Load(); l != nil {
		return l.Held()
	}
	return 0
}

// PendingOn is the events the lane of channel holds in the Run that is going: the one being handled and those waiting behind
// it. It is zero when the channel has no lane, or before Run has begun.
func (c *Client) PendingOn(channel string) int {
	if l := c.lanes.Load(); l != nil {
		return l.HeldOn(channel)
	}
	return 0
}
