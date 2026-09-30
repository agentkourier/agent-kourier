package fakeslack

import (
	"net/url"
)

// postMessage implements chat.postMessage
// (https://docs.slack.dev/reference/methods/chat.postMessage/).
//
// Error codes from the docs: channel_not_found, no_text, thread_not_found,
// invalid_blocks_format, msg_too_long. Any non-empty channel ID is accepted,
// since the fake has no channel registry. Content limits are in validate.go.
//
// UNCONFIRMED(WP-04): a reply must target a top-level message; the response
// echoes the message without its metadata, so courier must not depend on it
// there; blocks=[] with text stores no blocks.
func (s *Server) postMessage(form url.Values) (response, string) {
	channel := form.Get("channel")
	if channel == "" {
		return nil, "channel_not_found"
	}
	if code := validateContent(form.Get("text"), form.Get("blocks"), true); code != "" {
		return nil, code
	}
	m := &Message{
		Type: "message", User: botUser, Text: form.Get("text"),
		ThreadTS: form.Get("thread_ts"), Blocks: blocksOrNil(form.Get("blocks")),
	}
	if raw := form.Get("metadata"); raw != "" {
		var ok bool
		if m.Metadata, ok = parseMetadata(raw); !ok {
			return nil, "invalid_metadata_format"
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if code := s.addLocked(channel, m); code != "" {
		return nil, code
	}
	echo := m.clone()
	echo.Metadata = nil
	return response{"channel": channel, "ts": m.TS, "message": echo}, ""
}

// addLocked assigns m the next ts and stores it in channel. A reply (m.ThreadTS
// set) must target an existing thread parent.
func (s *Server) addLocked(channel string, m *Message) (code string) {
	if m.ThreadTS != "" {
		if s.threadLocked(channel, m.ThreadTS) == nil {
			return "thread_not_found"
		}
		s.markParentLocked(channel, m.ThreadTS)
	}
	m.TS = s.nextTSLocked()
	s.channels[channel] = append(s.channels[channel], m)
	return ""
}

// markParentLocked gives a message that has just gained a reply thread_ts equal
// to its own ts, as Slack reports thread parents.
func (s *Server) markParentLocked(channel, ts string) {
	for _, m := range s.channels[channel] {
		if m.TS == ts {
			m.ThreadTS = ts
		}
	}
}
