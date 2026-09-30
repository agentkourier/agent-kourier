package fakeslack

import (
	"encoding/json"
	"unicode/utf8"
)

// Message content limits, from the Block Kit reference
// (https://docs.slack.dev/reference/block-kit/blocks/ and the pages under it
// for each block and element) and the chat.postMessage method page
// (https://docs.slack.dev/reference/methods/chat.postMessage/). Courier's main
// output is long model text in section and markdown blocks, and WP-33 renders
// interactions with buttons, so these must bite in tests before they bite in
// the pilot.
//
// UNCONFIRMED(WP-04): the exact numbers as Slack enforces them today; the
// invalid_blocks code for every block rule; msg_too_long versus silent
// truncation of text over the limit; the section-field limit of 2000, which is
// recalled rather than read; the minimum of 1 character for section text and
// fields; the 12000-character limit on markdown blocks, which is cumulative
// over one message; and the counting unit. This fake counts Unicode code points
// (runes); Slack may count UTF-16 units, which differ for characters outside
// the basic plane.
const (
	maxBlocks          = 50    // blocks per message, and per blocks chunk
	maxMessageText     = 40000 // characters in a message's text
	maxSectionText     = 3000  // characters in a section block's text
	maxSectionField    = 2000  // characters in each of a section block's fields
	maxSectionFields   = 10    // fields in a section block
	maxHeaderText      = 150   // characters in a header block's text
	maxMarkdownBlocks  = 12000 // characters across all markdown blocks of a message
	maxActionsElements = 25    // elements in an actions block
	maxContextElements = 10    // elements in a context block
	maxButtonText      = 75    // characters in a button's label
	maxButtonValue     = 2000  // characters in a button's value
	maxID              = 255   // characters in a block_id or action_id
)

func runes(s string) int { return utf8.RuneCountInString(s) }

// blocksOrNil returns a validated blocks parameter as the JSON to store, or nil
// for an absent or empty one.
func blocksOrNil(v string) json.RawMessage {
	var list []json.RawMessage
	if json.Unmarshal([]byte(v), &list) != nil || len(list) == 0 {
		return nil
	}
	return json.RawMessage(v)
}

// validateContent checks the text and blocks of a request that writes message
// content and returns Slack's error code, or "" if they are acceptable. With
// required set, a request with no text and no blocks is no_text; blocks=[]
// counts as no blocks. Agent-UI blocks are rejected here; streaming chunks use
// checkBlocks directly.
func validateContent(text, blocks string, required bool) string {
	if runes(text) > maxMessageText {
		return "msg_too_long"
	}
	n, code := checkBlocks(blocks, false)
	if code == "" && required && text == "" && n == 0 {
		return "no_text"
	}
	return code
}

// checkBlocks validates a blocks parameter and reports how many blocks it
// holds. The agent-UI blocks (alert, card, carousel) are valid only when
// streaming is set.
//
// UNCONFIRMED(WP-04): that chat.postMessage and chat.update reject the agent-UI
// blocks and streaming chunks accept them. slack-go v0.23 notes the first;
// its AlertBlock doc says even streaming rejects alerts, while MsgOptionChunks
// says chunks carry Card and Carousel.
func checkBlocks(blocks string, streaming bool) (int, string) {
	var raw []json.RawMessage
	if blocks != "" && json.Unmarshal([]byte(blocks), &raw) != nil {
		return 0, "invalid_blocks_format"
	}
	if len(raw) > maxBlocks {
		return len(raw), "invalid_blocks"
	}
	markdown := 0
	for _, b := range raw {
		if !blockOK(b, streaming, &markdown) {
			return len(raw), "invalid_blocks"
		}
	}
	return len(raw), ""
}

type textObject struct {
	Text string `json:"text"`
}

// textLen returns the length of a text object's text; ok is false if raw is
// not a text object.
func textLen(raw json.RawMessage) (n int, ok bool) {
	var t textObject
	if json.Unmarshal(raw, &t) != nil {
		return 0, false
	}
	return runes(t.Text), true
}

// blockOK checks one block. The text of most blocks is an object, but the
// markdown block's is a plain string, so it is decoded per type. markdown
// accumulates the markdown text seen in the message so far.
func blockOK(raw json.RawMessage, streaming bool, markdown *int) bool {
	var b struct {
		Type     string            `json:"type"`
		BlockID  string            `json:"block_id"`
		Text     json.RawMessage   `json:"text"`
		Fields   json.RawMessage   `json:"fields"`
		Elements []json.RawMessage `json:"elements"`
	}
	if json.Unmarshal(raw, &b) != nil || b.Type == "" || runes(b.BlockID) > maxID {
		return false
	}
	switch b.Type {
	case "alert", "card", "carousel":
		return streaming
	case "markdown":
		var s string
		if json.Unmarshal(b.Text, &s) != nil {
			return false
		}
		*markdown += runes(s)
		return *markdown <= maxMarkdownBlocks
	case "header":
		n, ok := textLen(b.Text)
		return ok && n <= maxHeaderText
	case "section":
		return sectionOK(b.Text, b.Fields)
	case "context":
		return len(b.Elements) <= maxContextElements
	case "actions":
		if len(b.Elements) > maxActionsElements {
			return false
		}
		for _, e := range b.Elements {
			if !actionElementOK(e) {
				return false
			}
		}
	}
	return true
}

// sectionOK checks a section block's text and fields.
func sectionOK(text, fields json.RawMessage) bool {
	if text != nil {
		if n, ok := textLen(text); !ok || n < 1 || n > maxSectionText {
			return false
		}
	}
	var fs []textObject
	if fields != nil && json.Unmarshal(fields, &fs) != nil {
		return false
	}
	if len(fs) > maxSectionFields {
		return false
	}
	for _, f := range fs {
		if n := runes(f.Text); n < 1 || n > maxSectionField {
			return false
		}
	}
	return true
}

// actionElementOK checks an element of an actions block.
func actionElementOK(raw json.RawMessage) bool {
	var e struct {
		Type     string          `json:"type"`
		ActionID string          `json:"action_id"`
		Value    string          `json:"value"`
		Text     json.RawMessage `json:"text"`
	}
	if json.Unmarshal(raw, &e) != nil || runes(e.ActionID) > maxID {
		return false
	}
	if e.Type != "button" {
		return true
	}
	n, ok := textLen(e.Text)
	return ok && n <= maxButtonText && runes(e.Value) <= maxButtonValue
}
