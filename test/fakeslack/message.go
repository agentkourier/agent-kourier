package fakeslack

import (
	"encoding/json"
	"fmt"
	"slices"
)

// Metadata is Slack message metadata (https://docs.slack.dev/messaging/message-metadata/).
type Metadata struct {
	EventType    string         `json:"event_type"`
	EventPayload map[string]any `json:"event_payload"`
}

// parseMetadata decodes a metadata form field. ok is false when it is not a
// JSON object with an event_type.
//
// UNCONFIRMED(WP-04): that rule and the invalid_metadata_format code.
func parseMetadata(raw string) (m *Metadata, ok bool) {
	m = &Metadata{}
	return m, json.Unmarshal([]byte(raw), m) == nil && m.EventType != ""
}

// Edited marks a message that chat.update changed.
type Edited struct {
	User string `json:"user"`
	TS   string `json:"ts"`
}

// Message is a stored message in the shape Slack returns it.
type Message struct {
	Type     string          `json:"type"`
	User     string          `json:"user"`
	Text     string          `json:"text"`
	TS       string          `json:"ts"`
	ThreadTS string          `json:"thread_ts,omitempty"`
	Blocks   json.RawMessage `json:"blocks,omitempty"`
	Metadata *Metadata       `json:"metadata,omitempty"`
	Edited   *Edited         `json:"edited,omitempty"`

	// ReplyCount and LatestReply are filled in on thread parents by
	// conversations.history; the store leaves them zero.
	ReplyCount  int    `json:"reply_count,omitempty"`
	LatestReply string `json:"latest_reply,omitempty"`

	// Streaming is true from chat.startStream until chat.stopStream, and Chunks
	// holds every chunk the streaming calls sent, as sent. Both are for test
	// assertions and are not part of Slack's message shape.
	Streaming bool              `json:"-"`
	Chunks    []json.RawMessage `json:"-"`
}

// clone returns a deep copy of m, so callers can neither see later changes to
// the store nor change it.
func (m *Message) clone() Message {
	c := *m
	c.Blocks = slices.Clone(m.Blocks)
	if m.Metadata != nil {
		md := *m.Metadata
		if m.Metadata.EventPayload != nil {
			md.EventPayload = cloneValue(m.Metadata.EventPayload).(map[string]any)
		}
		c.Metadata = &md
	}
	if m.Edited != nil {
		e := *m.Edited
		c.Edited = &e
	}
	if m.Chunks != nil {
		c.Chunks = make([]json.RawMessage, len(m.Chunks))
		for i, ch := range m.Chunks {
			c.Chunks[i] = slices.Clone(ch)
		}
	}
	return c
}

// cloneValue deep-copies the maps and slices of a decoded JSON value.
func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		c := make(map[string]any, len(t))
		for k, x := range t {
			c[k] = cloneValue(x)
		}
		return c
	case []any:
		c := make([]any, len(t))
		for i, x := range t {
			c[i] = cloneValue(x)
		}
		return c
	}
	return v
}

const botUser = "UFAKEBOT"

// nextTSLocked issues the next message ts: the clock's time in microseconds,
// or one microsecond past the last ts if the clock has not moved on. Callers
// hold s.mu.
func (s *Server) nextTSLocked() string {
	s.last = max(s.now().UnixMicro(), s.last+1)
	return fmt.Sprintf("%d.%06d", s.last/1_000_000, s.last%1_000_000)
}

// Messages returns every message in channel, oldest first, thread replies
// included, with metadata. It is for test assertions, and the messages are
// copies.
func (s *Server) Messages(channel string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, 0, len(s.channels[channel]))
	for _, m := range s.channels[channel] {
		out = append(out, m.clone())
	}
	return out
}

// Thread returns the thread parent with the given ts followed by its replies,
// oldest first, or nil if the channel has no top-level message with that ts.
// The messages are copies.
func (s *Server) Thread(channel, ts string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadLocked(channel, ts)
}

// threadLocked is Thread for callers holding s.mu. Messages are stored in ts
// order, so the parent precedes its replies.
func (s *Server) threadLocked(channel, ts string) []Message {
	var out []Message
	for _, m := range s.channels[channel] {
		if (m.TS == ts && (m.ThreadTS == "" || m.ThreadTS == ts)) || (m.ThreadTS == ts && m.TS != ts) {
			out = append(out, m.clone())
		}
	}
	if len(out) == 0 || out[0].TS != ts {
		return nil
	}
	return out
}
