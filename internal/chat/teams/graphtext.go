package teams

import (
	"cmp"
	"encoding/json"
	"html"
	"strings"
	"unicode/utf8"
)

// This file reads a Microsoft Graph chatMessage: who sent it, and what it says as text. The shapes are those that Graph gave on a test
// tenant in November 2026 (a person's post and a mention of the bot as HTML, the bot's own replies, a card of another app with its
// body only an <attachment> tag and the card's JSON in attachments[].content, a system event), and Microsoft's documentation for the
// rest; HTML that Teams writes for other formatting (lists, code, tables, quotes) was not seen, and is read as HTML in general.

// cardContentTypeIn is the contentType of an Adaptive Card attachment, as Graph gives it.
const cardContentTypeIn = "application/vnd.microsoft.card.adaptive"

// cardMaxDepth is how deep in a card's elements cardText reads: the card's body holds elements of depth 1, a Container's items are one
// deeper. Text deeper than this is not read.
const cardMaxDepth = 32

// graphTextMax is the most of a message's body that is read as text, in bytes, and the most of its cards' text that is kept: what is past
// it is not read, or dropped. A person's message in Teams
// is at most about 28 KB (Microsoft's documentation, not measured), and htmlText is linear, so the cap bounds the work on a body that
// Graph serves of any size.
const graphTextMax = 128 << 10

// graphMessage is the part of a Graph chatMessage that the adapter reads. Every field is untrusted: Graph serves what people and apps
// wrote.
type graphMessage struct {
	ID              string          `json:"id"`
	MessageType     string          `json:"messageType"`
	DeletedDateTime *string         `json:"deletedDateTime"`
	From            *graphFrom      `json:"from"`
	Body            graphBody       `json:"body"`
	Attachments     []graphAttached `json:"attachments"`
	Mentions        []struct {
		Mentioned *graphFrom `json:"mentioned"`
	} `json:"mentions"`
}

// graphFrom is a message's sender, or a mention's party: a user, or an application (a bot, Workflows among them).
type graphFrom struct {
	Application *graphIdentity `json:"application"`
	User        *graphIdentity `json:"user"`
}

type graphIdentity struct {
	ID string `json:"id"`
}

// graphBody is a message's body: html or text.
type graphBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

// graphAttached is an attachment of a message. Content is a string for a card (its JSON), and may be null or absent.
type graphAttached struct {
	ContentType string          `json:"contentType"`
	Content     json.RawMessage `json:"content"`
}

// text is what the body says, of its first graphTextMax bytes: a text body as written, an html one as htmlText reads it (Graph's
// default), and nothing for another type.
func (b graphBody) text() string {
	content := cutText(b.Content)
	switch strings.ToLower(cmp.Or(b.ContentType, "html")) {
	case "text":
		return content
	case "html":
		return htmlText(content)
	}
	return ""
}

// cutText is s cut to its first graphTextMax bytes, at the start of a rune.
func cutText(s string) string {
	if len(s) <= graphTextMax {
		return s
	}
	cut := graphTextMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// cardsText is the text of the message's Adaptive Cards, one after another, of at most graphTextMax bytes: what is past it is dropped,
// as a body's text past it is not read.
func (m graphMessage) cardsText() string {
	var parts []string
	for _, a := range m.Attachments {
		var content string
		if a.ContentType != cardContentTypeIn || json.Unmarshal(a.Content, &content) != nil {
			continue
		}
		if t := cardText(content); t != "" {
			parts = append(parts, t)
		}
	}
	return cutText(strings.Join(parts, "\n"))
}

// mentions says whether the message mentions the application appID, by the mentions Graph lists (not by its text, which a person can
// write).
func (m graphMessage) mentions(appID string) bool {
	for _, mn := range m.Mentions {
		if mn.Mentioned != nil && mn.Mentioned.Application != nil && mn.Mentioned.Application.ID == appID {
			return true
		}
	}
	return false
}

// blockTags begin and end a line of their own.
var blockTags = map[string]bool{"p": true, "div": true, "li": true, "ul": true, "ol": true, "tr": true, "table": true,
	"blockquote": true, "pre": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "codeblock": true}

// skippedTags are read past with all they hold.
var skippedTags = map[string]bool{"script": true, "style": true}

// htmlText is the text a person reads in the HTML of a message: every tag gone, a block and a <br> a line, the entities decoded, a
// non-breaking space a space, and nothing of a comment, a script or a style. A mention, <at id="n">name</at>, is written <at>name</at>,
// as the text of a live activity writes it, so that a message reads the same whether it came live or from Graph. A "<" that begins no
// tag is text, and so is the rest of the HTML from a "<" that no ">" closes, so the read is linear in the HTML. Lines lose their trailing
// blanks, and blank lines and the blank ends of the text are dropped.
func htmlText(s string) string {
	var b strings.Builder
	text := func(t string) { b.WriteString(strings.ReplaceAll(html.UnescapeString(t), "\u00a0", " ")) }
	newline := func() { b.WriteByte('\n') }
	for len(s) > 0 {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			text(s)
			break
		}
		text(s[:i])
		s = s[i:]
		if strings.HasPrefix(s, "<!--") {
			end := strings.Index(s, "-->")
			if end < 0 {
				break
			}
			s = s[end+3:]
			continue
		}
		end, name, closing, kind := tagAt(s)
		switch kind {
		case notATag:
			text("<")
			s = s[1:]
			continue
		case unclosed:
			text(s)
			s = ""
			continue
		}
		s = s[end:]
		switch {
		case skippedTags[name] && !closing:
			s = s[closingTag(s, name):]
		case name == "at" && closing:
			b.WriteString("</at>")
		case name == "at":
			b.WriteString("<at>")
		case name == "br":
			newline()
		case blockTags[name]:
			newline()
		}
	}
	return tidyLines(b.String())
}

// tagKind says what a "<" begins.
type tagKind int

const (
	aTag     tagKind = iota // a tag, closed by its ">"
	notATag                 // "<" then no letter: text
	unclosed                // a tag's name, and no ">" that ends it (a quoted ">" in an attribute does not)
)

// tagAt reads the tag that s begins with: where it ends, its name in lowercase, whether it closes one, and its kind.
func tagAt(s string) (end int, name string, closing bool, kind tagKind) {
	i := 1
	if i < len(s) && s[i] == '/' {
		closing = true
		i++
	}
	start := i
	for i < len(s) && (isASCIILetter(s[i]) || (i > start && s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i == start {
		return 0, "", false, notATag
	}
	name = strings.ToLower(s[start:i])
	var quote byte
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i + 1, name, closing, aTag
		}
	}
	return 0, "", false, unclosed
}

// closingTag is where in s the first "</name" begins, the name's letters in either case, or len(s) if there is none. name is lowercase
// ASCII, and only ASCII letters are folded, so the index is one of s as written.
func closingTag(s, name string) int {
	for i := 0; i+2+len(name) <= len(s); i++ {
		if s[i] == '<' && s[i+1] == '/' && asciiFold(s[i+2:i+2+len(name)], name) {
			return i
		}
	}
	return len(s)
}

// asciiFold says whether a is lower, an ASCII lowercase word, with its ASCII capitals lowered.
func asciiFold(a, lower string) bool {
	for i := range len(lower) {
		c := a[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// tidyLines drops the trailing blanks of each line, the blank lines, and the blank ends of s. Leading blanks stay: code is indented.
func tidyLines(s string) string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimRight(line, " \t\r"); strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// cardText is the text of the Adaptive Card whose JSON is content: each TextBlock's text, each RichTextBlock's runs as one line, and
// each fact of a FactSet as a line "title: value" (the value alone when there is no title), in the order of the card, through its
// containers, columns and tables down to cardMaxDepth. Actions, inputs and images are not text. Content that is not JSON has none.
func cardText(content string) string {
	var card any
	if json.Unmarshal([]byte(content), &card) != nil {
		return ""
	}
	var lines []string
	add := func(s string) {
		if strings.TrimSpace(s) != "" {
			lines = append(lines, s)
		}
	}
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		el, ok := v.(map[string]any)
		if !ok || depth > cardMaxDepth {
			return
		}
		switch el["type"] {
		case "TextBlock":
			add(str(el["text"]))
		case "RichTextBlock":
			var run strings.Builder
			for _, in := range list(el["inlines"]) {
				if s, ok := in.(string); ok {
					run.WriteString(s)
				} else if r, ok := in.(map[string]any); ok && r["type"] == "TextRun" {
					run.WriteString(str(r["text"]))
				}
			}
			add(run.String())
		case "FactSet":
			for _, f := range list(el["facts"]) {
				if fact, ok := f.(map[string]any); ok {
					title, value := str(fact["title"]), str(fact["value"])
					if title == "" {
						add(value)
					} else {
						add(title + ": " + value)
					}
				}
			}
		}
		for _, key := range []string{"body", "items", "columns", "rows", "cells"} {
			for _, child := range list(el[key]) {
				walk(child, depth+1)
			}
		}
	}
	walk(card, 0)
	return strings.Join(lines, "\n")
}

// str is v if it is a string, and "" if not.
func str(v any) string {
	s, _ := v.(string)
	return s
}

// list is v if it is a JSON array, and nil if not.
func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// isMessageID says whether id has the form of a Teams message ID: decimal digits, at most maxActivityID of them. Every ID seen on a test
// tenant was a post's creation time in milliseconds (13 digits), as Microsoft's examples are; Microsoft calls them opaque.
func isMessageID(id string) bool {
	if id == "" || len(id) > maxActivityID {
		return false
	}
	for i := range len(id) {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

// compareIDs orders two Teams message IDs (isMessageID) as the numbers they are, which is the order of their posts: by their length
// without leading zeros, then their digits.
func compareIDs(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}
