package fakeweb

// Message is the one schema a kourier-v1 sender POSTs, version 1, as the profile's contract gives it.
type Message struct {
	// Channel is the conversation; Thread the ID of the message that opened the thread, or empty for a message that opens one; ID the
	// message's own, the same on every delivery of it; User who wrote it, and Text what they wrote.
	Channel, Thread, ID, User, Text string
	// Mention says a reply in a thread is addressed to Agent Kourier.
	Mention bool
}

// Body is the message's JSON.
func (m Message) Body() string {
	v := map[string]any{"v": 1, "channel": m.Channel, "id": m.ID, "user": m.User, "text": m.Text}
	if m.Thread != "" {
		v["thread"] = m.Thread
	}
	if m.Mention {
		v["mention"] = true
	}
	return marshal(v)
}
