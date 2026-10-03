package fakeslack

import (
	"encoding/json"
	"net/url"
	"strings"
)

// The streaming methods are built from Slack's method pages and slack-go
// v0.29.0's client code, with no captured traffic behind them. Each
// UNCONFIRMED(WP-04) tag below is for WP-04 to settle against a real workspace,
// along with Q2 (rate limits of chat.startStream and chat.update under many
// concurrent threads), which a fake cannot answer at all.

// startStream implements chat.startStream
// (https://docs.slack.dev/reference/methods/chat.startStream/). The streamed
// message is stored at once as a reply in the thread, with the text so far, and
// stays Streaming until chat.stopStream.
//
// Documented: thread_ts and, in channels, recipient_user_id are required;
// initial content may be markdown_text or chunks.
//
// UNCONFIRMED(WP-04): recipient_team_id is also required in channels (recalled
// from the docs and Slack's Bolt helpers, which take both; slack-go only says
// the option "sets the recipient team ID"); the error codes invalid_arguments
// (no thread_ts), missing_recipient_user_id and missing_recipient_team_id; that
// a stream is visible to reads while running; treating channel IDs that start
// with "D" as direct messages, which need no recipient; and that a metadata
// parameter, which this method is not known to take, is ignored. Metadata
// matters for spec 8.6, where the outbox tags each post, so courier should set
// it on chat.stopStream and never rely on it here.
func (s *Server) startStream(form url.Values) (response, string) {
	channel := form.Get("channel")
	dm := strings.HasPrefix(channel, "D")
	switch {
	case channel == "":
		return nil, "channel_not_found"
	case form.Get("thread_ts") == "":
		return nil, "invalid_arguments"
	case !dm && form.Get("recipient_user_id") == "":
		return nil, "missing_recipient_user_id"
	case !dm && form.Get("recipient_team_id") == "":
		return nil, "missing_recipient_team_id"
	}
	text, chunks, code := parseChunks(form)
	if code != "" {
		return nil, code
	}
	if runes(text) > maxMessageText {
		return nil, "msg_too_long"
	}
	m := s.botMessage(text, form.Get("thread_ts"))
	m.Chunks, m.Streaming = chunks, true

	s.mu.Lock()
	defer s.mu.Unlock()
	if code := s.addLocked(channel, m); code != "" {
		return nil, code
	}
	s.queueOwnLocked(channel, nil, m)
	return response{"channel": channel, "ts": m.TS}, ""
}

// streamChunk implements chat.appendStream and, with stop set, chat.stopStream
// (https://docs.slack.dev/reference/methods/chat.appendStream/ and
// https://docs.slack.dev/reference/methods/chat.stopStream/). Both append the
// request's markdown_text and chunks; stopping also ends the stream, applies
// metadata and appends blocks under the streamed text (see stoppedBlocks).
//
// UNCONFIRMED(WP-04): the error codes message_not_in_streaming_state,
// invalid_chunks and, for an append with neither markdown_text nor chunks,
// invalid_arguments; that chat.appendStream returns only ok (no channel or ts);
// that chat.stopStream accepts blocks and metadata, while chat.appendStream
// parses a metadata parameter and discards it; and that task_update,
// plan_update and blocks chunks are accepted and kept on the message without
// being rendered into its text.
func (s *Server) streamChunk(form url.Values, stop bool) (response, string) {
	channel, ts := form.Get("channel"), form.Get("ts")
	switch {
	case channel == "":
		return nil, "channel_not_found"
	case ts == "":
		return nil, "invalid_arguments"
	}
	if _, code := checkBlocks(form.Get("blocks"), false); code != "" {
		return nil, code
	}
	text, chunks, code := parseChunks(form)
	if code != "" {
		return nil, code
	}
	if !stop && text == "" && len(chunks) == 0 {
		return nil, "invalid_arguments"
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
		if !m.Streaming {
			return nil, "message_not_in_streaming_state"
		}
		if runes(m.Text)+runes(text) > maxMessageText {
			return nil, "msg_too_long"
		}
		prev := m.clone()
		m.Text += text
		m.Chunks = append(m.Chunks, chunks...)
		if !stop {
			return response{}, ""
		}
		m.Streaming = false
		m.Blocks = stoppedBlocks(m.Text, blocksOrNil(form.Get("blocks")))
		if meta != nil {
			m.Metadata = meta
		}
		s.queueOwnLocked(channel, &prev, m)
		echo := m.clone()
		echo.Metadata = nil // as for chat.postMessage
		return response{"channel": channel, "ts": ts, "message": echo}, ""
	}
	return nil, "message_not_found"
}

// stoppedBlocks is the blocks of a message that chat.stopStream has ended: the
// text that was streamed, as one markdown block, followed by the blocks the
// stop sent. A stop never replaces the streamed content, so a stop that sends
// the text again shows it twice. A stream that carried no text has no such
// block, and one with no text and no stop blocks has none at all.
//
// Confirmed in dogfood 2026-10-03: stop blocks are appended. A streamed reply
// of 1222 characters, stopped with a markdown block of the same text, was read
// back as one message with two identical rich_text blocks (1222 + 1 + 1222
// characters). slack-go v0.29.0's own streaming example
// (examples/chat_streaming/chat_streaming.go) stops with feedback buttons
// alone, which is how the method is meant to be used.
//
// UNCONFIRMED(WP-04): that Slack stores the streamed text as a rich_text block,
// not a markdown one; this fake keeps it as a markdown block so that a test can
// count the copies.
func stoppedBlocks(streamed string, stop json.RawMessage) json.RawMessage {
	var all []json.RawMessage
	if streamed != "" {
		body, _ := json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{"markdown", streamed}) // a struct of two strings always marshals
		all = append(all, body)
	}
	if stop != nil {
		var list []json.RawMessage
		_ = json.Unmarshal(stop, &list) // blocksOrNil returned it as a non-empty list
		all = append(all, list...)
	}
	if all == nil {
		return nil
	}
	b, _ := json.Marshal(all) // raw messages that were valid JSON
	return b
}

// maxStreamMarkdown is the most characters one markdown_text parameter, or one
// markdown_text chunk, may carry in a single streaming call.
//
// UNCONFIRMED(WP-04): the 12000-character limit and the msg_too_long code for
// it. The limit on the message as a whole is maxMessageText, checked as text
// accumulates.
const maxStreamMarkdown = 12000

// chunkTypes are the chunk types slack-go v0.29.0 knows.
//
// UNCONFIRMED(WP-04): that Slack rejects any other type with invalid_chunks.
var chunkTypes = map[string]bool{"markdown_text": true, "task_update": true, "plan_update": true, "blocks": true}

// parseChunks returns the text a streaming request adds, markdown_text
// followed by the text of any markdown_text chunks, and the request's chunks
// as sent. It validates each markdown_text against maxStreamMarkdown, each
// blocks chunk's blocks like any blocks, and the chunk types. A blocks chunk
// with no blocks, whether the key is missing or the list empty, is invalid.
//
// UNCONFIRMED(WP-04): that Slack rejects a blocks chunk with no blocks with
// invalid_chunks.
func parseChunks(form url.Values) (text string, chunks []json.RawMessage, code string) {
	text = form.Get("markdown_text")
	if runes(text) > maxStreamMarkdown {
		return "", nil, "msg_too_long"
	}
	raw := form.Get("chunks")
	if raw == "" {
		return text, nil, ""
	}
	if json.Unmarshal([]byte(raw), &chunks) != nil {
		return "", nil, "invalid_chunks"
	}
	for _, r := range chunks {
		var c struct {
			Type   string          `json:"type"`
			Text   string          `json:"text"`
			Blocks json.RawMessage `json:"blocks"`
		}
		if json.Unmarshal(r, &c) != nil || !chunkTypes[c.Type] {
			return "", nil, "invalid_chunks"
		}
		switch c.Type {
		case "markdown_text":
			if runes(c.Text) > maxStreamMarkdown {
				return "", nil, "msg_too_long"
			}
			text += c.Text
		case "blocks":
			n, code := checkBlocks(string(c.Blocks), true)
			if code != "" {
				return "", nil, code
			}
			if n == 0 {
				return "", nil, "invalid_chunks"
			}
		}
	}
	return text, chunks, ""
}
