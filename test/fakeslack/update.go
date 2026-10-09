package fakeslack

import (
	"net/url"
)

// updateMessage implements chat.update
// (https://docs.slack.dev/reference/methods/chat.update/).
//
// Error codes from the docs: channel_not_found, message_not_found, no_text,
// invalid_blocks_format, msg_too_long. The request itself must carry text or at
// least one block, so a metadata-only update, or blocks=[] without text, is
// no_text.
//
// UNCONFIRMED(WP-04): invalid_arguments for a missing ts; fields the request
// omits are kept, though real Slack may clear omitted blocks, so Agent Kourier should
// always send text and blocks together; blocks=[] alongside text clears the
// message's blocks, as slack-go v0.29.0's MsgOptionBlocks() with no arguments
// intends; a message that is still streaming cannot be updated
// (cant_update_message), so the renderer's two paths (spec 8.3) cannot collide
// on one message, though real Slack's behavior and code for it are unknown.
func (s *Server) updateMessage(form url.Values) (response, string) {
	channel, ts := form.Get("channel"), form.Get("ts")
	switch {
	case channel == "":
		return nil, "channel_not_found"
	case ts == "":
		return nil, "invalid_arguments"
	}
	if code := validateContent(form.Get("text"), form.Get("blocks"), true); code != "" {
		return nil, code
	}
	var meta *Metadata
	if raw := form.Get("metadata"); raw != "" {
		var ok bool
		if meta, ok = parseMetadata(raw); !ok {
			return nil, "invalid_metadata_format"
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.channels[channel] {
		if m.TS != ts {
			continue
		}
		if m.Streaming {
			return nil, "cant_update_message"
		}
		prev := m.clone()
		if form.Get("text") != "" {
			m.Text = form.Get("text")
		}
		if form.Has("blocks") {
			m.Blocks = blocksOrNil(form.Get("blocks"))
		}
		if meta != nil {
			m.Metadata = meta
		}
		m.Edited = &Edited{User: s.bot.user, TS: s.nextTSLocked()}
		s.queueOwnLocked(channel, &prev, m)
		echo := m.clone()
		echo.Metadata = nil // as for chat.postMessage
		return response{"channel": channel, "ts": ts, "text": m.Text, "message": echo}, ""
	}
	return nil, "message_not_found"
}
