package slack

// MaxChannelBacklog is the events a channel's lane holds, for the external tests that fill one.
const MaxChannelBacklog = maxChannelBacklog

// MaxPendingEvents is the events all lanes hold.
const MaxPendingEvents = maxPendingEvents

// PendingEvents is the events the lanes of the Run that is going hold: the one being handled and those waiting. It is zero
// before Run has begun.
func (c *Client) PendingEvents() int {
	if l := c.lanes.Load(); l != nil {
		return l.held()
	}
	return 0
}
