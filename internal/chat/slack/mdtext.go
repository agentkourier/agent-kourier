package slack

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// fenceMark is the line Agent Kourier writes to open and to close a block of code, alone at column 0, and nothing else
// ever does (the contract in escape.go). The row numbers in this file are those of the table in
// docs/spikes/slack-code-rendering.md: a probe of the sandbox workspace, 2026-10-04.
const fenceMark = "```"

// mdText turns the markdown of the agent into the markdown Agent Kourier sends, one document that may come in pieces: the
// text of a message is one piece, and a stream's is as many as it has Appends, which Slack reads as one (row 12). So the
// state of the reading is kept here, across the pieces, and the zero value is a document that has not begun.
//
// It reads the text as Agent Kourier means to show it, and writes every fence and every code span itself, in one form:
//
//   - a line that starts, after white space and at most one list marker or quote mark, with three or more backticks or
//     tildes is a fence line (readFence has the marks). Agent Kourier writes "```" alone at column 0 in its place, with the
//     mark dropped, and the block's lines are dedented by the white space of the opener, and by the width of the list marker
//     too, or lose a quote mark if they have one (row 22: a fence in a list item is not code to Slack, but one at column 0
//     is). The list or the quote goes on as prose after the block;
//   - in a block the text is raw (rows 01, 02, 19), but for "<" before "!", "@" or "#", which is written "&lt;" so
//     that no broadcast, mention or channel token can be in it, whatever reads it;
//   - a backtick run, then text with no backtick, then a run of the same length, in a line of prose, is a code span
//     (rows 03, 21, 50), written with one backtick each side and raw inside like a block;
//   - a pipe table out of a block is written as a block, its columns aligned (mdtable.go), and is held back until it
//     ends, or until enough of it has come to set its columns;
//   - everything else is prose: escapeRune, and every backtick as "\`" (rows 34, 53, 55) and every tilde of a run of
//     three or more as "\~" (rows 35, 42, 43), so that Slack never opens code by itself.
//
// Slack then sees code only where Agent Kourier wrote it, whatever the agent wrote.
//
// A line's start is held back until it is known not to be a fence line (so a list marker or a quote mark, and the white
// space after it, wait for the character that says), a prose line is held back from its first backtick to its end, and "<"
// at the end of code until the next character is known: the text reaches Slack a line late at most, but for a table, and
// the end of the document (final) writes what is held.
//
// A stream that Agent Kourier did not start in this process, or one that lost a call without an answer, is blind: Slack may be
// inside a block that Agent Kourier does not know of, so everything is escaped, prose and code alike (escapeRune), and no code
// is written or read. That is safe whatever state Slack is in, and costs only the look of code in what is sent.
type mdText struct {
	code   bool      // inside a block that Agent Kourier opened
	inLine bool      // past the start of the line that held begins in
	opener mark      // what stood before the run of the opener of the block that is open, besides white space
	indent int       // the characters of white space the lines of the block lose: see readFence
	held   string    // text not yet written: the cleaned input from the first thing that is not decided
	blind  bool      // Slack's state is not known: escape everything (see above)
	tbl    openTable // the table whose block is open, which the rows still to come are written in
	hold   int       // the hold of a table (holdSize): tableHold if 0, and less only in tests
}

// text returns md cleaned, escaped as the document is read so far and cut to limit units: what is held back is not in
// it, unless final says md ends the document. A cut is marked with chat.Truncated, and leaves the state as what was
// sent: nothing after the cut is read, and it is the middle of a line.
func (m *mdText) text(md string, limit int, final bool) string {
	s := chat.Clean(md, false)
	if m.blind {
		s, m.held = m.held+s, "" // what was held back when the stream went blind is written with the rest
		return fit(s, limit, true)
	}
	saved := *m
	whole := &mdOut{budget: math.MaxInt}
	m.feed(s, final, whole)
	if whole.units <= limit {
		return whole.sb.String()
	}
	*m = saved
	if limit <= chat.Units(chat.Truncated) {
		m.cut()
		return string([]rune(chat.Truncated)[:max(limit, 0)])
	}
	part := &mdOut{budget: limit - chat.Units(chat.Truncated)}
	m.feed(s, final, part)
	m.cut()
	return part.sb.String() + chat.Truncated
}

// cut ends the reading of a text that was cut: the rest of it is dropped, and what is written next follows the
// marker, in the middle of a line.
func (m *mdText) cut() { m.held, m.inLine = "", true }

// mdOut is what is written, with the room left for it. Its pieces are the unit of a cut: a fence, a span, a run of tildes or
// a character is written whole or not at all, so that a cut never leaves half of one.
type mdOut struct {
	sb     strings.Builder
	units  int
	budget int
	full   bool // a piece did not fit
}

func (o *mdOut) put(piece string) bool {
	u := chat.Units(piece)
	if o.units+u > o.budget {
		o.full = true
		return false
	}
	o.sb.WriteString(piece)
	o.units += u
	return true
}

// feed reads s after what is held, and writes what is decided.
func (m *mdText) feed(s string, final bool, o *mdOut) {
	m.held += s
	for m.held != "" && !o.full {
		if !m.step(final, o) {
			break
		}
	}
	if final && m.held == "" && m.tbl.layout != nil && !m.inLine && o.put(fenceMark+"\n") {
		m.code, m.tbl = false, openTable{} // the text ends with a row of a table and its line break: the block is closed
	}
}

// step reads and writes the next thing of the first line held, and says whether to go on: false is that it needs more
// text, or that nothing more fits.
func (m *mdText) step(final bool, o *mdOut) bool {
	line, _, hasNL := strings.Cut(m.held, "\n")
	complete := hasNL || final
	switch {
	case !m.inLine && m.tbl.layout != nil:
		return m.tableRow(final, o)
	case !m.inLine:
		if !m.code {
			switch m.table(final, o) {
			case tableWait, tableFull:
				return false
			case tableDone:
				return true
			}
		}
		return m.start(line, complete, o)
	case m.code:
		return m.codeLine(line, hasNL, complete, o)
	default:
		return m.proseLine(line, hasNL, complete, o)
	}
}

// start reads the start of a line, which is either a fence line or the white space before the rest of it.
func (m *mdText) start(line string, complete bool, o *mdOut) bool {
	h := readFence(line, m.code, m.opener)
	switch {
	case h.run >= 3:
		return complete && m.fence(line, h, o) // the rest of the line says what it is
	case !complete && h.pending:
		return false // a fence may be coming
	}
	keep := h.lead
	if m.code {
		keep = dedent(h.lead, m.indent)
	}
	if !o.put(keep) {
		return false
	}
	m.held = m.held[h.drop+len(h.lead):]
	m.inLine = true
	return true
}

// mark is what may stand before the run of a fence besides white space.
type mark uint8

const (
	markNone  mark = iota // nothing
	markList              // a list marker: "-", "*" or "+", or one to nine digits and "." or ")", and then white space
	markQuote             // a quote mark: ">" and, if the next character is white space, that one character
)

// fenceHead is the start of a line, read for a fence.
type fenceHead struct {
	lead    string // the white space the line starts with, after the quote mark it loses (drop)
	drop    int    // the bytes at the start of the line that are the quote mark of a line of a quoted block
	run     int    // how many backticks or tildes (ch) start the line after the white space and the mark: 0 if none
	ch      byte
	rest    string // what follows the run, if the line has one of 3 or more
	mark    mark   // what stands before the run
	indent  int    // for a fence line: the indent of the block it opens (mdText.indent)
	pending bool   // the line may become a fence line with more text: all it holds so far could be the start of one
}

// readFence reads the start of line as a fence line: three or more backticks or tildes (run, of one character, ch) after white
// space and, outside a block, one list marker or one quote mark and white space again. So "- ```", "1) ```", ">```" and
// "> ```" are fence lines, and "- - ```", "> > ```", "> - ```" and "-```" are not: a second mark, or a marker with no white
// space after it, leaves the line prose, which escapes the run.
//
// The mark is dropped from the opener's line, and the lines of its block lose indent characters of white space: those before
// the run, a list marker's among them. A quote mark is dropped from each line of its block instead, with the one character
// of white space after it, so its indent is the white space after that, if the opener has any.
//
// In a block that Agent Kourier opened (code) after a list marker (opener markList), a line may close it with a list marker
// before the run. After a quote mark (markQuote), every line first loses its quote mark, if it has one, and is then read as
// in any block. A block opened with white space alone takes only white space before a closing run, as a mark in it is the
// text of the block (rows 33, 36 and 37).
func readFence(line string, code bool, opener mark) fenceHead {
	var h fenceHead
	if code && opener == markQuote {
		if t := strings.TrimLeftFunc(line, unicode.IsSpace); strings.HasPrefix(t, ">") {
			h.drop = len(line) - len(t) + 1 + spaceSize(t[1:])
			line = line[h.drop:]
		}
	}
	body := strings.TrimLeftFunc(line, unicode.IsSpace)
	h.lead = line[:len(line)-len(body)]
	n, mk := 0, markNone
	if body != "" && body[0] != '`' && body[0] != '~' {
		switch {
		case body[0] == '>' && !code:
			n, mk = 1+spaceSize(body[1:]), markQuote
		case !code || opener == markList:
			var st markerState
			switch n, st = listMarker(body); st {
			case markerMaybe:
				h.pending = true // the line so far could be a marker
				return h
			case markerYes:
				mk = markList
			}
		}
	}
	around := body[n:]
	after := strings.TrimLeftFunc(around, unicode.IsSpace)
	switch {
	case after == "":
		h.pending = true // white space, or a mark and white space: a fence may follow
		return h
	case after[0] != '`' && after[0] != '~':
		return h
	}
	h.ch, h.run = after[0], runLen(after, after[0])
	h.rest = after[h.run:]
	if h.run < 3 {
		h.pending = h.rest == "" // a run that may go on
		return h
	}
	h.mark = mk
	h.indent = utf8.RuneCountInString(line[:len(line)-len(after)]) // white space and a list marker, up to the run
	if mk == markQuote {
		h.indent = utf8.RuneCountInString(around[:len(around)-len(after)]) // the white space after the quote mark
	}
	return h
}

// spaceSize is the size of the character of white space s starts with, or 0.
func spaceSize(s string) int {
	if r, size := utf8.DecodeRuneInString(s); size > 0 && unicode.IsSpace(r) {
		return size
	}
	return 0
}

// markerState is what listMarker made of the start of a line.
type markerState uint8

const (
	markerNo    markerState = iota // not a list marker
	markerYes                      // a marker, followed by white space
	markerMaybe                    // a marker if more text comes: the line ends in it, or in what may be one
)

// listMarker reads the list marker body starts with and says how long it is (white space after it not counted): a "-", "*" or
// "+", or one to nine digits and a "." or ")", and then a character of white space.
func listMarker(body string) (n int, st markerState) {
	switch c := body[0]; {
	case c == '-' || c == '*' || c == '+':
		n = 1
	case c >= '0' && c <= '9':
		for n < len(body) && body[n] >= '0' && body[n] <= '9' {
			n++
		}
		if n > 9 {
			return 0, markerNo
		}
		if n == len(body) {
			return 0, markerMaybe
		}
		if body[n] != '.' && body[n] != ')' {
			return 0, markerNo
		}
		n++
	default:
		return 0, markerNo
	}
	switch {
	case n == len(body):
		return 0, markerMaybe
	case spaceSize(body[n:]) > 0:
		return n, markerYes
	}
	return 0, markerNo
}

// runLen is the length of the run of c that s starts with.
func runLen(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// fence reads a whole fence line. In a block it closes it, whatever else is on the line (row 40, 41, 44: Slack would
// close on fewer lines, which is safe, since the line Agent Kourier writes is the one Slack closes on). Out of one it opens
// one: the rest of the line is dropped if it is an info string (row 10), and otherwise it is the text of the block, with
// a closing run of the opener's in it or without.
//
// What follows the fence on its line is put back to be read in its turn, which is how the text of a one-line block
// meets the rules of a block's lines: the line is rewritten into the lines it would be.
func (m *mdText) fence(line string, h fenceHead, o *mdOut) bool {
	ch, run, rest := h.ch, h.run, h.rest
	after := m.held[len(line):] // the line break, if any, and the lines after it
	code, opener, indent, held := false, markNone, m.indent, ""
	if m.code {
		held = strings.TrimLeftFunc(rest, unicode.IsSpace) + after
		if strings.TrimSpace(rest) == "" {
			held = strings.TrimPrefix(after, "\n") // a closing line with nothing else: its break is the one written
		}
	} else {
		code, opener, indent = true, h.mark, h.indent
		switch at, n := closingRun(rest, ch, run); {
		case at >= 0: // a block of one line: the inner text, then a closing line, with what follows the run after it
			held = fenceMark + rest[at+n:] + after
			if inner := strings.TrimSpace(rest[:at]); inner != "" {
				held = inner + "\n" + held
			}
		case !strings.ContainsFunc(strings.TrimSpace(rest), unicode.IsSpace): // the info string, a word or nothing
			held = strings.TrimPrefix(after, "\n")
		default: // the first line of the block
			held = strings.TrimLeftFunc(rest, unicode.IsSpace) + after
		}
	}
	piece := fenceMark + "\n"
	if held == "" && after == "" {
		piece = fenceMark // the text ends with the fence, and with no line break after it
	}
	if !o.put(piece) {
		return false
	}
	m.code, m.opener, m.indent, m.inLine, m.held = code, opener, indent, false, held
	return true
}

// closingRun finds the first run of c of at least atLeast in s, and says where it is and how long.
func closingRun(s string, c byte, atLeast int) (at, n int) {
	for i := 0; i < len(s); {
		if s[i] != c {
			i++
			continue
		}
		n := runLen(s[i:], c)
		if n >= atLeast {
			return i, n
		}
		i += n
	}
	return -1, 0
}

// dedent is lead without its first n characters.
func dedent(lead string, n int) string {
	for ; n > 0 && lead != ""; n-- {
		_, size := utf8.DecodeRuneInString(lead)
		lead = lead[size:]
	}
	return lead
}

// codeLine writes the rest of a line of a block, raw. A "<" at the end of what is held waits for the next character.
func (m *mdText) codeLine(line string, hasNL, complete bool, o *mdOut) bool {
	text := line
	if !complete {
		text = strings.TrimSuffix(text, "<")
	}
	for i := 0; i < len(text); {
		piece, size := codePiece(text, i)
		if !o.put(piece) {
			m.held = m.held[i:]
			return false
		}
		i += size
	}
	m.held = m.held[len(text):]
	if !complete {
		return false
	}
	return m.endLine(hasNL, o)
}

// endLine ends a line whose text is all written: held is left with its line break, if it has one, and the lines after.
func (m *mdText) endLine(hasNL bool, o *mdOut) bool {
	if !hasNL {
		m.held = ""
		return false
	}
	if !o.put("\n") {
		return false
	}
	m.held = m.held[1:]
	m.inLine = false
	return true
}

// codePiece is what the character at s[i] is in code, and its size: itself, but for "<" before "!", "@" or "#".
func codePiece(s string, i int) (piece string, size int) {
	_, size = utf8.DecodeRuneInString(s[i:])
	if s[i] == '<' && i+1 < len(s) && strings.IndexByte("!@#", s[i+1]) >= 0 {
		return "&lt;", size
	}
	return s[i : i+size], size
}

// proseLine writes the rest of a line of prose. A backtick holds the line back from there to its end, since a span
// needs its closing run; a run of tildes holds only itself back, until its end.
func (m *mdText) proseLine(line string, hasNL, complete bool, o *mdOut) bool {
	for i := 0; i < len(line); {
		var piece string
		var size int
		switch line[i] {
		case '`':
			if !complete {
				m.held = m.held[i:]
				return false
			}
			piece, size = codeSpan(line[i:])
		case '~':
			size = runLen(line[i:], '~')
			if i+size == len(line) && !complete {
				m.held = m.held[i:]
				return false
			}
			piece = line[i : i+size]
			if size >= 3 {
				piece = strings.Repeat(`\~`, size)
			}
		default:
			r, n := utf8.DecodeRuneInString(line[i:])
			piece, size = escapeRune(r, true), n
		}
		if !o.put(piece) {
			m.held = m.held[i:]
			return false
		}
		i += size
	}
	m.held = m.held[len(line):]
	return m.endLine(hasNL, o)
}

// codeSpan reads the backtick run at the start of s (a line of prose from the run to its end), and returns what it
// is written as and how much of s it takes: the span it opens, if the next run is of the same length, which is
// "`", the content raw and "`"; and otherwise the run alone, escaped. A span with a backtick in it, or none to close it,
// is no span: the cost is that its backticks show (rows 14, 53, 55), which is safe.
func codeSpan(s string) (piece string, size int) {
	n := runLen(s, '`')
	if at := strings.IndexByte(s[n:], '`'); at >= 0 && runLen(s[n+at:], '`') == n {
		var b strings.Builder
		b.WriteByte('`')
		content := s[n : n+at]
		for i := 0; i < len(content); {
			p, size := codePiece(content, i)
			b.WriteString(p)
			i += size
		}
		b.WriteByte('`')
		return b.String(), n + at + n
	}
	return strings.Repeat("\\`", n), n
}
