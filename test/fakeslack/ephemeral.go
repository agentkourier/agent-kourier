package fakeslack

import "net/url"

// Ephemeral is a message the bot sent with chat.postEphemeral: visible to one
// user only, and in no history. Message.User is the bot, as for any bot post.
type Ephemeral struct {
	Message
	Recipient string
}

// Ephemerals returns the ephemeral messages sent in channel, oldest first. They
// are for test assertions, and are copies.
func (s *Server) Ephemerals(channel string) []Ephemeral {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Ephemeral, 0, len(s.ephemerals[channel]))
	for _, e := range s.ephemerals[channel] {
		out = append(out, Ephemeral{e.clone(), e.Recipient})
	}
	return out
}

// postEphemeral implements chat.postEphemeral
// (https://docs.slack.dev/reference/methods/chat.postEphemeral/), which courier
// uses to tell one user, say, that an answer was incomplete. The message is
// kept apart from the channel's messages, since nobody else could read it.
//
// Error codes from the docs: channel_not_found, no_text, invalid_blocks,
// msg_too_long.
//
// UNCONFIRMED(WP-04): invalid_arguments for a missing user; that a thread_ts
// is taken as it is, without checking the thread exists or marking its parent;
// the message_ts in the response; and that ephemeral messages get no event,
// and cannot be updated.
func (s *Server) postEphemeral(form url.Values) (response, string) {
	channel := form.Get("channel")
	if channel == "" {
		return nil, "channel_not_found"
	}
	if form.Get("user") == "" {
		return nil, "invalid_arguments"
	}
	if code := validateContent(form.Get("text"), form.Get("blocks"), true); code != "" {
		return nil, code
	}
	m := s.botMessage(form.Get("text"), form.Get("thread_ts"))
	m.Blocks = blocksOrNil(form.Get("blocks"))

	s.mu.Lock()
	defer s.mu.Unlock()
	m.TS = s.nextTSLocked()
	s.ephemerals[channel] = append(s.ephemerals[channel], Ephemeral{*m, form.Get("user")})
	return response{"message_ts": m.TS}, ""
}
