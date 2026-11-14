package fakeslack

import (
	"encoding/json"
	"net/url"
	"slices"
)

// updateMessage implements chat.update
// (https://docs.slack.dev/reference/methods/chat.update/).
//
// Error codes from the docs: channel_not_found, message_not_found, no_text,
// invalid_blocks_format, msg_too_long. The request itself must carry text or at
// least one block, so a metadata-only update, or blocks=[] without text, is
// no_text.
//
// An update with blocks replaces the content of a stream that was stopped, and its
// cards with it: the stream's chunks are dropped, and the task_card blocks of the
// update are the cards the message shows (Slack, probed 2026-10-21 in a sandbox
// workspace by chat.update, read back through conversations.replies and seen in
// the web client: an update whose blocks had no task_card dropped the stream's
// cards, and one whose blocks had them showed them).
//
// block_mismatch refuses an update whose blocks would take away a message's text, when
// text is all the message holds (dropsText). Slack documents the code as "Rich-text blocks
// cannot be replaced with non-rich-text blocks"; the rest is as probed on 2026-11-03 in a
// sandbox workspace by chat.update, on messages posted or streamed for the purpose:
// a message that holds any other block beside its text takes any update, and so does
// one posted with text and no blocks.
//
// UNCONFIRMED(WP-04): invalid_arguments for a missing ts; fields the request
// omits are kept, though real Slack may clear omitted blocks, so Agent Kourier should
// always send text and blocks together; blocks=[] alongside text clears the
// message's blocks, as slack-go v0.29.0's MsgOptionBlocks() with no arguments
// intends; a message that is still streaming cannot be updated
// (cant_update_message), so the renderer's two paths (spec 8.3) cannot collide
// on one message, though real Slack's behavior and code for it are unknown; and cant_update_message for a
// message another app posted, which is Slack's code for a message the caller may not change, here also for
// another app's, where Slack's code for that is not known.
func (s *Server) updateMessage(app App, form url.Values) (response, string) {
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
		if m.AppID != "" && m.AppID != app.AppID {
			return nil, "cant_update_message"
		}
		if m.Streaming {
			return nil, "cant_update_message"
		}
		if form.Has("blocks") && dropsText(m, form.Get("blocks")) {
			return nil, "block_mismatch"
		}
		prev := m.clone()
		if form.Get("text") != "" {
			m.Text = form.Get("text")
		}
		if form.Has("blocks") {
			m.Blocks, m.Chunks = blocksOrNil(form.Get("blocks")), nil
		}
		if meta != nil {
			m.Metadata = meta
		}
		m.Edited = &Edited{User: app.BotUser, TS: s.nextTSLocked()}
		s.queueOwnLocked(channel, &prev, m)
		echo := m.clone()
		echo.Metadata = nil // as for chat.postMessage
		return response{"channel": channel, "ts": ts, "text": m.Text, "message": echo}, ""
	}
	return nil, "message_not_found"
}

// dropsText says whether blocks would replace a message that holds only text with blocks that hold none. A message with a
// card or any other block beside its text is not protected, and an empty blocks takes nothing away.
func dropsText(m *Message, blocks string) bool {
	held := blockTypes(m.Blocks)
	if len(held) == 0 || len(m.Tasks()) > 0 || slices.ContainsFunc(held, func(t string) bool { return !isTextBlock(t) }) {
		return false
	}
	sent := blockTypes(json.RawMessage(blocks))
	return len(sent) > 0 && !slices.ContainsFunc(sent, isTextBlock)
}

// isTextBlock says whether a block of the type t is text: a rich_text block, or a markdown block, which Slack stores as one.
func isTextBlock(t string) bool { return t == "rich_text" || t == "markdown" }

// blockTypes is the type of each block of raw, which the request's checks have parsed already.
func blockTypes(raw json.RawMessage) []string {
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	types := make([]string, 0, len(blocks))
	for _, b := range blocks {
		types = append(types, b.Type)
	}
	return types
}
