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
//   - a line that starts, after white space, with three or more backticks or tildes is a fence line. Agent Kourier writes
//     "```" alone at column 0 in its place, and the block's lines are dedented by the white space of the opener
//     (row 22: a fence in a list item is not code to Slack, but one at column 0 is);
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
// A line's start is held back until it is known not to be a fence line, a prose line is held back from its first
// backtick to its end, and "<" at the end of code until the next character is known: the text reaches Slack a line
// late at most, but for a table, and the end of the document (final) writes what is held.
//
// A stream that Agent Kourier did not start in this process, or one that lost a call without an answer, is blind: Slack may be
// inside a block that Agent Kourier does not know of, so everything is escaped, prose and code alike (escapeRune), and no code
// is written or read. That is safe whatever state Slack is in, and costs only the look of code in what is sent.
type mdText struct {
	code   bool         // inside a block that Agent Kourier opened
	inLine bool         // past the start of the line that held begins in
	indent int          // the white space of the opener, which the lines of the block lose
	held   string       // text not yet written: the cleaned input from the first thing that is not decided
	blind  bool         // Slack's state is not known: escape everything (see above)
	tbl    *tableLayout // the layout of the table whose block is open, which the rows still to come are written in
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
	if final && m.held == "" && m.tbl != nil && !m.inLine && o.put(fenceMark) {
		m.code, m.tbl = false, nil // the text ends with a row of a table, whose block is closed
	}
}

// step reads and writes the next thing of the first line held, and says whether to go on: false is that it needs more
// text, or that nothing more fits.
func (m *mdText) step(final bool, o *mdOut) bool {
	line, _, hasNL := strings.Cut(m.held, "\n")
	complete := hasNL || final
	switch {
	case !m.inLine && m.tbl != nil:
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
	lead, ch, run, rest := readFence(line)
	switch {
	case run >= 3:
		return complete && m.fence(line, lead, ch, run, rest, o) // the rest of the line says what it is
	case !complete && rest == "" && (ch != 0 || lead == line):
		return false // a fence may be coming
	}
	keep := lead
	if m.code {
		keep = dedent(lead, m.indent)
	}
	if !o.put(keep) {
		return false
	}
	m.held = m.held[len(lead):]
	m.inLine = true
	return true
}

// readFence reads the white space at the start of line, then the run of backticks or tildes after it (ch is 0 if
// the line has none, and run is how many there are, of the same character), and rest, which is what follows them.
func readFence(line string) (lead string, ch byte, run int, rest string) {
	body := strings.TrimLeftFunc(line, unicode.IsSpace)
	lead = line[:len(line)-len(body)]
	if body == "" || (body[0] != '`' && body[0] != '~') {
		return lead, 0, 0, body
	}
	run = runLen(body, body[0])
	return lead, body[0], run, body[run:]
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
func (m *mdText) fence(line, lead string, ch byte, run int, rest string, o *mdOut) bool {
	after := m.held[len(line):] // the line break, if any, and the lines after it
	code, indent, held := false, m.indent, ""
	if m.code {
		held = strings.TrimLeftFunc(rest, unicode.IsSpace) + after
		if strings.TrimSpace(rest) == "" {
			held = strings.TrimPrefix(after, "\n") // a closing line with nothing else: its break is the one written
		}
	} else {
		code, indent = true, utf8.RuneCountInString(lead)
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
	mark := fenceMark + "\n"
	if held == "" && after == "" {
		mark = fenceMark // the text ends with the fence, and with no line break after it
	}
	if !o.put(mark) {
		return false
	}
	m.code, m.indent, m.inLine, m.held = code, indent, false, held
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
