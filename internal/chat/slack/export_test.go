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

// PendingOn is the events the lane of channel holds in the Run that is going: the one being handled and those waiting behind
// it. It is zero when the channel has no lane, or before Run has begun.
func (c *Client) PendingOn(channel string) int {
	l := c.lanes.Load()
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if ln := l.byChannel[channel]; ln != nil {
		return ln.held
	}
	return 0
}
