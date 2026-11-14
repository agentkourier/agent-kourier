package chat

// HistoryMessage is a message from a channel's history, as the platform's adapter would have seen it arrive
// live. The dry run (trigger.Replay) classifies it.
type HistoryMessage struct {
	// Message is what the adapter would hand the Sink. For an Ignored message only At is set: where it was,
	// so that a listing can show it.
	Message Inbound
	// Own says Agent Kourier itself posted it. The adapter never forwards such a message, whatever a trigger names.
	Own bool
	// Ignored says the adapter would drop it before the Sink: an edit, a deletion or any other subtype that
	// starts nothing, and a bot's message with no content.
	Ignored bool
}
