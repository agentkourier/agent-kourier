package slack

import (
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// This file is where chat's security contract is kept for Slack (spec 8.4, R2): text arrives in the
// neutral model as literal characters, and leaves as text that Slack reads as those characters and
// nothing else. Three things make it so, and they work together:
//
//   - the client sends every request inert (inertOptions): no parse, so "@here" and "#general" stay text, no
//     link names, no unfurl, so Slack does not fetch a URL the agent wrote;
//   - the text is cleaned (chat.Clean: no control, hidden or reordering character, so none can split
//     "<!channel>" apart or reorder what the reader sees) and cut to Slack's limit;
//   - "&", "<" and ">" are written as entities, the only escape Slack's text has, so no "<!channel>",
//     "<@U1>", "<https://x|y>" or "<b>" is a token; and in a markdown block "\", "[" and "]" are escaped
//     with a backslash too, so no "[text](url)" or "![alt](url)" is a link or an image.
//
// Literal places are plain_text objects, which carry no emphasis, code or list, and the only text that
// is mrkdwn is what courier wrote itself (a status line built from a verified user ID and a time).
// What Slack does that no escape stops is listed with the adapter's concessions in platform_test.go.
//
// UNCONFIRMED(WP-04): that Slack shows "&amp;", "&lt;" and "&gt;" in a plain_text object as the
// characters, and that a plain_text object parses no mention, link or emphasis; that Slack honours
// CommonMark backslash escapes in a markdown block and in markdown_text. The first two rest on the
// text being escaped as for mrkdwn anyway, so that if plain_text does parse tokens it still finds none.

// escapeRune is r as it may appear in the text of a message: "&", "<" and ">" as entities, and, in a markdown
// block and a stream's markdown_text, "\", "[" and "]" with a backslash too.
func escapeRune(r rune, markdown bool) string {
	switch r {
	case '&':
		return "&amp;"
	case '<':
		return "&lt;"
	case '>':
		return "&gt;"
	case '\\', '[', ']':
		if markdown {
			return `\` + string(r)
		}
	}
	return string(r)
}

// text returns s cleaned, cut to limit units and escaped, ready to put in a plain_text object or a
// message's top-level text. The cut is made on the escaped text, which is what Slack counts, one character
// at a time, so it never splits a rune or an entity, and a cut is marked with chat.Truncated. line says the
// place is one line.
func text(s string, limit int, line bool) string { return fit(chat.Clean(s, line), limit, false) }

// markdown is text for a markdown block or a stream's markdown_text.
func markdown(s string, limit int) string { return fit(chat.Clean(s, false), limit, true) }

// fit escapes the cleaned text s and cuts the result to limit units.
func fit(s string, limit int, md bool) string {
	var out strings.Builder
	total := 0
	for _, r := range s {
		u := escapeRune(r, md)
		out.WriteString(u)
		total += chat.Units(u)
	}
	if total <= limit {
		return out.String()
	}
	if limit <= chat.Units(chat.Truncated) {
		return string([]rune(chat.Truncated)[:max(limit, 0)])
	}
	out.Reset()
	used := 0
	for _, r := range s {
		u := escapeRune(r, md)
		if used+chat.Units(u) > limit-chat.Units(chat.Truncated) {
			break
		}
		out.WriteString(u)
		used += chat.Units(u)
	}
	return out.String() + chat.Truncated
}

// within returns s cleaned and cut, as for a field that is shown after prefix and before suffix in a
// place whose limit is limit: the whole field is at most limit units, and the text of s is what is cut.
func within(prefix, s, suffix string, limit int, line bool) string {
	room := limit - chat.Units(prefix) - chat.Units(suffix)
	return prefix + text(s, room, line) + suffix
}
