package chattest

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// SlackMessage is the text of one Slack message, or of one request that posts, updates or streams one, as Slack
// receives it: each field is a kind of text Slack reads in its own way. An empty field holds nothing.
type SlackMessage struct {
	// Text is the top-level text, which Slack reads as mrkdwn.
	Text string
	// MarkdownText is a streaming request's markdown_text, which Slack reads as CommonMark.
	MarkdownText string
	// Blocks is the Block Kit blocks, a JSON array, as sent or as stored.
	Blocks json.RawMessage
	// Chunks is a streaming request's chunks, a JSON array, as sent.
	Chunks json.RawMessage
}

// ShownSlackMessage is what the person sees of the text of one Slack message, and the constructs Slack acts on in it,
// read as Slack reads each kind of text, each by the reader of its own dialect: the top-level text and a mrkdwn object as
// mrkdwn (ScanSlack, which has no backslash escape), a markdown block and markdown_text as CommonMark with code shown
// literally (ScanCommonMarkLiteralCode: what the probe of 2026-10-04 measured, rows 04, 19 and 20 of
// docs/spikes/slack-code-rendering.md; the code that is there is only what Agent Kourier wrote; a "<!", "<@" or "<#" in it
// is still reported, as it is not in what Agent Kourier writes), and a plain_text object as the characters it holds
// (ScanSlackPlain), in which style is not a construct but a token or a link would still be one (a plain_text object is
// not known to parse them, UNCONFIRMED(WP-04), and the adapter does not rely on that). A rich_text block is read by
// richTextShown, a task_card's title as the characters it holds, and a stream's chunks by chunkTexts.
//
// The Shown come in the order of the fields: the top-level text, markdown_text, the chunks, then the blocks. Blocks or
// chunks that are not a JSON array of the expected shape show nothing.
func ShownSlackMessage(m SlackMessage) []Shown {
	var out []Shown
	if m.Text != "" {
		out = append(out, ScanSlack(m.Text))
	}
	if m.MarkdownText != "" {
		out = append(out, ScanCommonMarkLiteralCode(m.MarkdownText))
	}
	if len(m.Chunks) != 0 {
		out = append(out, chunkTexts(m.Chunks)...)
	}
	if len(m.Blocks) != 0 {
		var blocks any
		if json.Unmarshal(m.Blocks, &blocks) == nil {
			walkBlocks(blocks, &out)
		}
	}
	return out
}

// chunkTexts reads the chunks of a streaming request: a markdown_text chunk as CommonMark, as markdown_text is, and the
// title, details and output of a task_update chunk by taskShown.
func chunkTexts(raw json.RawMessage) []Shown {
	var chunks []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Title   string `json:"title"`
		Details string `json:"details"`
		Output  string `json:"output"`
	}
	if json.Unmarshal(raw, &chunks) != nil {
		return nil
	}
	var out []Shown
	if len(chunks) == 0 {
		return nil
	}
	for _, c := range chunks {
		if c.Text != "" {
			out = append(out, ScanCommonMarkLiteralCode(c.Text))
		}
		for _, s := range []string{c.Title, c.Details, c.Output} {
			if s != "" {
				out = append(out, taskShown(s))
			}
		}
	}
	return out
}

// taskEscapes undoes the escapes the Slack adapter writes in a task's text: the three entities of Slack's text, and the
// backslash escapes that keep a link, an image or a link reference definition out of markdown.
var taskEscapes = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", `\\`, `\`, `\[`, "[", `\(`, "(")

// taskShown is how a task's text is read, which Slack does not say (UNCONFIRMED(WP-04)): the constructs in it are
// those of CommonMark, the strictest of the readings it may have, and what a person sees is the characters it holds with
// the adapter's escapes undone. A reading as plain text finds no construct and shows the backslashes; one as mrkdwn
// finds only the tokens that the entities already stop. Style is the one thing that may act (the Slack adapter's
// conformance subject concedes it for a step's title and detail).
func taskShown(s string) Shown {
	return Shown{Displayed: taskEscapes.Replace(s), Active: ScanCommonMark(s).Active}
}

// richTextShown is how a rich_text block is read, which is where the Slack adapter shows the name and the arguments of a
// tool: one Shown for each element of the block, with the characters of its text elements as what is displayed. A
// rich_text_section, which carries a name, is only that. A rich_text_preformatted, which carries arguments, is a block
// of code, the one construct reported. A token in a text element is not reported, because Block Kit has no token in
// text: a mention, a broadcast, a channel and a link in rich_text are elements of their own type. Every element or
// sub-element of the block that is not a text element with no other field (a user, a broadcast, a channel, a link,
// a styled or any other text, an element of another type) is reported as the construct it is, on every Shown of the block,
// so that the adapter cannot put one anywhere in it by accident. The 2026-10-13 live approval probe returned the
// generated hostile fields as text leaves; this reader remains structural so the unit suite still rejects any future
// renderer change that introduces an active rich_text element.
func richTextShown(block map[string]any) []Shown {
	var found []Active
	report := func(kind Kind, el map[string]any) {
		found = append(found, Active{Kind: kind, Text: fmt.Sprint(el)})
	}
	var shown []Shown
	var code []bool
	for _, e := range asList(block["elements"]) {
		el, _ := e.(map[string]any)
		typ, _ := el["type"].(string)
		if typ != "rich_text_section" && typ != "rich_text_preformatted" {
			report(KindHTML, el)
			continue
		}
		var text strings.Builder
		for _, c := range asList(el["elements"]) {
			child, _ := c.(map[string]any)
			ctyp, _ := child["type"].(string)
			s, isString := child["text"].(string)
			switch {
			case ctyp == "text" && isString && len(child) == 2: // no style, no other field
				text.WriteString(s)
			case ctyp == "user":
				report(KindMention, child)
			case ctyp == "broadcast":
				report(KindBroadcast, child)
			case ctyp == "channel":
				report(KindChannelLink, child)
			case ctyp == "link":
				report(KindLink, child)
			default:
				report(KindHTML, child)
			}
		}
		shown = append(shown, Shown{Displayed: text.String()})
		code = append(code, typ == "rich_text_preformatted")
	}
	if len(shown) == 0 { // a block of nothing readable still reports what it holds
		shown = append(shown, Shown{})
		code = append(code, false)
	}
	for i := range shown {
		shown[i].Active = slices.Clone(found)
		if code[i] {
			shown[i].Active = append(shown[i].Active, Active{Kind: KindCode, Text: shown[i].Displayed})
		}
	}
	return shown
}

// walkBlocks reads every text object, markdown block, rich_text block and task_card title in v, a decoded JSON value of
// Block Kit, wherever it is nested, and appends what each shows to out.
func walkBlocks(v any, out *[]Shown) {
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			walkBlocks(e, out)
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		if typ == "rich_text" {
			*out = append(*out, richTextShown(v)...)
			return
		}
		// A task_card's title is the characters it holds, with no construct in it: the probe of 2026-10-21 in the web client
		// showed hostile titles (tokens, links, markdown, entities) as typed, acting on none (the Slack adapter's taskCard).
		if s, isString := v["title"].(string); typ == "task_card" && isString {
			*out = append(*out, Shown{Displayed: s})
		}
		switch s, isString := v["text"].(string); {
		case typ == "plain_text" && isString:
			*out = append(*out, ScanSlackPlain(s))
		case typ == "mrkdwn" && isString:
			*out = append(*out, ScanSlack(s))
		case typ == "markdown" && isString:
			*out = append(*out, ScanCommonMarkLiteralCode(s))
		}
		for k, e := range v {
			if k != "text" || typ == "" { // a text object is a leaf; a block's text is walked as its own object
				walkBlocks(e, out)
			} else if _, isObject := e.(map[string]any); isObject {
				walkBlocks(e, out)
			}
		}
	}
}

func asList(v any) []any { l, _ := v.([]any); return l }
