// This file writes an agent's markdown (chat.Body, a stream's text) as the CommonMark that Teams reads when an activity's
// textFormat is markdown: emphasis, headings, quotes, lists and code render, and nothing links, shows an image, is HTML, makes a
// mention (Teams' <at> tag) or an entity. It is the CommonMark writer that Agent Kourier had for a receiver that reads CommonMark
// (removed, unreleased, on 2026-11-17), with what Teams needs besides: a "<" is never sent raw, in prose or in code, and an "&" in
// prose is sent as "&amp;".
// That a Teams client renders these escapes and fences as CommonMark does is not measured (no live probe of Teams' rendering
// was made); what the conformance suite holds it to is CommonMark's reading, which treats code as not inert (chattest.ScanCommonMark).
//
// Agent Kourier decides what is code, and writes every block of it itself:
//
//   - a fenced block (a run of three or more backticks or tildes, at most three columns past the list item it is in, or right
//     after the item's marker) and an indented block (four columns past the item, where no paragraph goes on: after a blank line,
//     a heading, a rule or a block, at the start, or five columns or more after the item's marker; an empty item ends at a blank
//     line) are each written as a fence of backticks at column 0, longer than every run of backticks in the code, then the
//     code's lines with each "<" written "&lt;", then the same fence. The fence's info string keeps its first word if that word is
//     only letters, digits and "-", "_", "+", "." or "#", and nothing else. A fence at column 0 is in no list item, so a block ends
//     the list it was in: the lines of the item that follow it lose the item's indent while they have it, so that they are read as
//     text and not as code;
//   - a code span (a run of backticks, then text, then a run of the same length, in one line) is written as it is: the agent's
//     runs and the text between them. Where a reader could take its text for anything but code, it is prose instead
//     (reader.span): a span that holds a "<", a span with a "|" in a line that may be a row of a table, and a span right after a
//     word that may be linked;
//   - everything else is prose, in which "\", "[" and "]" get a backslash, "<" is written "&lt;" and "&" "&amp;", so that no link,
//     image, link reference, autolink, HTML, mention or entity is made, and every backtick and every tilde of a run of three or more
//     get a backslash, so that no code opens that Agent Kourier did not write. CommonMark shows each escaped character as itself.
//
// So the only code a reader finds is what Agent Kourier wrote, and no "<" reaches Teams from the agent's text, so that nothing
// the agent wrote can be taken for the "<at>" of a mention entity that Agent Kourier adds. Where Agent Kourier reads the agent's
// text otherwise than CommonMark would (code in a block quote, a span over two lines), the text is prose: the reader shows its
// characters, without the look of code.

package teams

import (
	"math"
	"strings"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// markdown returns md cleaned (chat.Clean) and written as the package says, cut so that what is sent is at most limit UTF-16 units:
// the escapes and the fences are counted. A cut is marked with chat.Truncated, which is never in code that Agent Kourier writes: a
// cut in a block closes the block with its fence, and the marker follows on a line of its own; a cut in prose drops the white space
// at the end of its line. A receiver may still read the marker as code where it reads as code a line that Agent Kourier took for
// prose, where the two read differently: an indented block in a block quote, or in a list item opened by a second marker on the
// same line ("1. - " and then four more columns), since Agent Kourier reads one marker a line. A character, an escape, a span
// and a fence line are each sent whole or not at all, so a span that does not fit is dropped whole. A limit too small for the
// marker gets the start of the marker alone.
func markdown(md string, limit int) string {
	s := chat.Clean(md, false)
	whole := &writer{limit: math.MaxInt}
	write(s, whole)
	if whole.units <= limit {
		return whole.b.String()
	}
	if limit <= markerUnits {
		return string([]rune(chat.Truncated)[:max(limit, 0)])
	}
	part := &writer{limit: limit, cut: true}
	write(s, part)
	return part.end()
}

var markerUnits = chat.Units(chat.Truncated)

// writer is the text being sent and the room left in it. In a cut, a piece is written only if what would end the text after
// it still fits: the fence that closes the block it leaves open, and the marker.
type writer struct {
	b      strings.Builder
	units  int
	limit  int
	cut    bool
	full   bool   // a piece did not fit: nothing more is written
	fence  string // the fence of the block that is open, and "" in prose
	nl     bool   // the text ends with a line break
	closed bool   // the text ends with a closing fence, with no line break after it
}

// put writes piece, after which fence is the fence of the block that is open ("" in prose), and says whether it did.
func (w *writer) put(piece, fence string) bool {
	if w.full {
		return false
	}
	nl := strings.HasSuffix(piece, "\n")
	closed := w.fence != "" && fence == "" && !nl
	u := chat.Units(piece)
	if w.cut && w.units+u+endUnits(fence, nl, closed) > w.limit {
		w.full = true
		return false
	}
	w.b.WriteString(piece)
	w.units += u
	w.fence, w.nl, w.closed = fence, nl, closed
	return true
}

// chars writes s, the text of a line of code, with each "<" written "&lt;", one character at a time in a cut, so that the cut
// may fall inside the line.
func (w *writer) chars(s, fence string) bool {
	if !w.cut {
		return w.put(strings.ReplaceAll(s, "<", "&lt;"), fence)
	}
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		piece := s[i : i+size]
		if piece == "<" {
			piece = "&lt;"
		}
		if !w.put(piece, fence) {
			return false
		}
		i += size
	}
	return true
}

// end is the text of a cut, ended: the block that is open closed, and the marker on a line of its own after a fence. In prose, the
// white space at the end of the line the marker ends is dropped, so that a line of white space alone, which a receiver may read as
// code once the marker follows it, is not one.
func (w *writer) end() string {
	text, tail := w.b.String(), ""
	switch {
	case w.fence != "" && w.nl:
		tail = w.fence + "\n"
	case w.fence != "":
		tail = "\n" + w.fence + "\n"
	case w.closed:
		tail = "\n"
	default:
		text = strings.TrimRight(text, " \t")
	}
	return text + tail + chat.Truncated
}

// endUnits is the length of what end adds in the state that fence, nl and closed describe.
func endUnits(fence string, nl, closed bool) int {
	n := markerUnits
	switch {
	case fence != "" && nl:
		n += len(fence) + 1
	case fence != "":
		n += len(fence) + 2
	case closed:
		n++
	}
	return n
}

// reader reads the agent's text a line at a time, and writes it.
type reader struct {
	w      *writer
	lines  []string
	last   bool   // the text ends with a line break: its last line has one too
	table  []bool // the lines that may be rows of a GFM table (tableRows)
	items  []int  // the content columns of the list items that are open, the innermost last
	para   bool   // the line before is text of a paragraph, which a line indented further goes on with
	empty  bool   // the line before opened a list item with nothing after its marker, which a blank line ends
	dedent int    // the columns that the lines of a list item lose after a block of it, while they have them
}

func write(s string, w *writer) {
	if s == "" {
		return
	}
	r := &reader{w: w, lines: strings.Split(s, "\n")}
	if n := len(r.lines) - 1; r.lines[n] == "" {
		r.lines, r.last = r.lines[:n], true
	}
	r.table = tableRows(r.lines)
	for i := 0; i < len(r.lines); {
		next, ok := r.block(i)
		if !ok {
			return
		}
		i = next
	}
}

// block reads the line i and what belongs with it, writes them, and returns the line after them; false is that the text is full.
func (r *reader) block(i int) (int, bool) {
	line := r.lines[i]
	ind, body := indent(line, 0)
	if body == "" {
		if r.empty {
			r.items = r.items[:len(r.items)-1] // an item may start with one blank line, but not when it is empty
		}
		r.para, r.empty = false, false
		return i + 1, r.prose(i, line)
	}
	r.empty = false
	marker := listMarker(body)
	rule := thematicBreak(body)
	if rule {
		marker = 0 // "* * *" and "- - -" are rules, not items
	}
	_, run, _ := fenceOf(body)
	if ind < r.dedent {
		r.dedent = 0
		// The item ends, and a paragraph taken out of it may be just before: a blank line lets the marker start a list, which an
		// ordered one that starts at another number than 1 cannot do in a paragraph.
		if r.para && marker > 0 && !r.w.put("\n", "") {
			return 0, false
		}
	}
	for len(r.items) > 0 && ind < r.items[len(r.items)-1] && (!r.para || marker > 0 || run > 0 || rule) {
		r.items = r.items[:len(r.items)-1] // the item ends: a paragraph of it does not go on lazily into this line
	}
	base := 0
	if len(r.items) > 0 {
		base = r.items[len(r.items)-1]
	}
	switch rel := ind - base; {
	case rel >= 4 && !r.para:
		return r.indented(i, base, "", strip(line, base+4))
	case rel < 0 || rel >= 4:
		r.para = true // a paragraph's line, which the paragraph before goes on with
	case run > 0:
		return r.fenced(i, "", body, ind, base)
	case marker > 0:
		after := ind + marker // the column after the marker, which is ASCII
		col, rest := indent(body[marker:], after)
		content := col
		if rest == "" || col-after > 4 {
			content = after + 1
		}
		r.items = append(r.items, content)
		r.empty = rest == ""
		head := line[:len(line)-len(body)+marker]
		if rest != "" && content != col {
			// The item starts with an indented block: its first line is what follows the marker and one column.
			return r.indented(i, content, head, stripAt(body[marker:], after, content+4))
		}
		if _, run, _ := fenceOf(rest); run > 0 {
			return r.fenced(i, head, rest, col, content)
		}
		r.para = paragraph(rest, false)
	default:
		r.para = paragraph(body, r.para)
	}
	return i + 1, r.prose(i, line)
}

// fenced reads the block whose opening fence, body, is in the line i at column ind, in a list item whose content is at column base
// (0 for none), and writes it. head is what stands before the fence on its line (a list marker), if anything, which is written
// on a line of its own before the block.
//
// The block ends with a line of the fence's character, at least as long as its run, and white space, at most three columns past
// base; with the item, at a line that is not blank and is indented less than base; or with the text. Its lines lose ind columns of
// white space, if they have them.
func (r *reader) fenced(i int, head, body string, ind, base int) (int, bool) {
	ch, run, info := fenceOf(body)
	var code []string
	next := len(r.lines)
	for j := i + 1; j < len(r.lines); j++ {
		col, rest := indent(r.lines[j], 0)
		if rest != "" && col < base {
			next = j // the item ends, and its block with it: the line is read again, as the first after the block
			break
		}
		if rest != "" && col-base <= 3 && closes(rest, ch, run) {
			next = j + 1
			break
		}
		code = append(code, strip(r.lines[j], ind))
	}
	return next, r.code(head, word(info), code, next, base)
}

// indented reads the indented block that starts with the line i, in a list item whose content is at column base, and writes
// it: the lines indented four columns past base, and the blank lines between them, each without those columns. first is the
// text of the line i in the block, and head what stands before it on its line (a list marker), if anything, which is written on
// a line of its own before the block.
func (r *reader) indented(i, base int, head, first string) (int, bool) {
	last := i
	for j := i + 1; j < len(r.lines); j++ {
		col, rest := indent(r.lines[j], 0)
		if rest == "" {
			continue
		}
		if col < base+4 {
			break
		}
		last = j
	}
	code := make([]string, 0, last+1-i)
	code = append(code, first)
	for _, l := range r.lines[i+1 : last+1] {
		code = append(code, strip(l, base+4))
	}
	return last + 1, r.code(head, "", code, last+1, base)
}

// code writes a block: head on a line of its own if there is one, the fence with info, the lines of code raw, and the fence again,
// with a line break after it if a line follows (next is the line after the block). The fence is a run of backticks longer than
// every run in the code, and at least three.
func (r *reader) code(head, info string, code []string, next, base int) bool {
	w := r.w
	if head != "" && (!w.put(strip(head, r.dedent), "") || !w.put("\n", "")) {
		return false
	}
	longest := 0
	for _, l := range code {
		for i := 0; i < len(l); i++ {
			if l[i] == '`' {
				n := runLen(l[i:], '`')
				longest = max(longest, n)
				i += n - 1
			}
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	if !w.put(fence+info+"\n", fence) {
		return false
	}
	for _, l := range code {
		if !w.chars(l, fence) || !w.put("\n", fence) {
			return false
		}
	}
	end := fence
	if next < len(r.lines) || r.last {
		end += "\n"
	}
	if !w.put(end, "") {
		return false
	}
	r.para = false
	if base > 0 {
		r.dedent = base // the block took the item's lines out of the list: those after it are taken out too
	}
	return true
}

// prose writes the line i, line, as prose, and its line break if it has one. It loses dedent columns of white space first.
func (r *reader) prose(i int, line string) bool {
	if r.dedent > 0 {
		line = strip(line, r.dedent)
	}
	runs := backtickRuns(line)
	next := 0     // the first of runs not yet passed
	link := false // the word so far may be linked: it has a ":" or an "@" in it, or "www."
	for j := 0; j < len(line); {
		var piece string
		size := 1
		switch c := line[j]; c {
		case '`':
			for runs[next].at < j {
				next++
			}
			n := runs[next].n
			if k := runs[next].same; k >= 0 && r.span(i, link, line[j+n:runs[k].at]) {
				piece, size = line[j:runs[k].at+n], runs[k].at+n-j // a span, as the agent wrote it
			} else {
				piece, size = strings.Repeat("\\`", n), n
			}
		case '~':
			size = runLen(line[j:], '~')
			piece = line[j : j+size]
			if size >= 3 {
				piece = strings.Repeat(`\~`, size)
			}
		case '\\', '[', ']':
			piece = `\` + string(c)
		case '<':
			piece = "&lt;" // not "\<": a bare URL that a receiver links runs on over a backslash, and would leave the "<" bare
		case '&':
			piece = "&amp;" // so that no entity of the agent's makes a character, "<" among them
		default:
			_, size = utf8.DecodeRuneInString(line[j:])
			piece = line[j : j+size]
		}
		if !r.w.put(piece, "") {
			return false
		}
		for k := j; k < j+size; k++ { // what was read, span text included, makes the word that a span after it follows
			switch line[k] {
			case ' ', '\t':
				link = false
			case ':', '@':
				link = true
			case '.':
				link = link || k >= 3 && strings.EqualFold(line[k-3:k+1], "www.")
			}
		}
		j += size
	}
	if i < len(r.lines)-1 || r.last {
		return r.w.put("\n", "")
	}
	return true
}

// run is a run of backticks in a line: where it starts, its length, and the index of the next run of the same length (-1 if none).
type run struct{ at, n, same int }

// backtickRuns are the runs of backticks in line, in order, each with the next of its length, which closes a span it opens. A
// last run past the end of the line stands for none, so that a search for the run at a position always stops.
func backtickRuns(line string) []run {
	end := run{at: len(line), same: -1}
	if strings.IndexByte(line, '`') < 0 {
		return []run{end}
	}
	var runs []run
	for i := 0; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		n := runLen(line[i:], '`')
		runs = append(runs, run{at: i, n: n})
		i += n
	}
	last := map[int]int{}
	for k := len(runs) - 1; k >= 0; k-- {
		runs[k].same = -1
		if s, ok := last[runs[k].n]; ok {
			runs[k].same = s
		}
		last[runs[k].n] = k
	}
	return append(runs, end)
}

// span says whether a span whose text is code, in the line i, is written as code: raw. link says that the word right before it may
// be linked. It is not written as code, and is prose, where a receiver could read its text otherwise than as code, or where it
// holds what is never sent raw:
//
//   - if code holds a "<", which a span cannot escape: CommonMark shows "&lt;" in a span as it is written;
//   - in a line that may be a row of a GFM table, if code holds a "|": the table splits the row into cells there before it reads
//     code, and the text after the "|" would be read raw;
//   - right after a word that a receiver may link (one with a ":" or an "@" in it, or "www."): a bare URL runs on to white space or
//     "<", and so takes in the span's opening backticks, and the text after them would be read raw.
func (r *reader) span(i int, link bool, code string) bool {
	return !link && !strings.Contains(code, "<") && (!r.table[i] || !strings.Contains(code, "|"))
}

// indent reads the white space s starts with, from column col, and returns the column after it and the rest of s. A tab goes on to
// the next multiple of 4, as CommonMark's tab stops do.
func indent(s string, col int) (int, string) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ':
			col++
		case '\t':
			col += 4 - col%4
		default:
			return col, s[i:]
		}
	}
	return col, ""
}

// strip is line without its first n columns of white space, or without all of it if it has less. A tab that reaches past column
// n leaves the columns past it as spaces.
func strip(line string, n int) string { return stripAt(line, 0, n) }

// stripAt is strip for a line that starts at column col: s without its white space up to column n.
func stripAt(line string, col, n int) string {
	for i := 0; i < len(line); i++ {
		if col >= n {
			return line[i:]
		}
		switch line[i] {
		case ' ':
			col++
		case '\t':
			next := col + 4 - col%4
			if next > n {
				return strings.Repeat(" ", next-n) + line[i+1:]
			}
			col = next
		default:
			return line[i:]
		}
	}
	return ""
}

// fenceOf reads the fence that body opens: its character, the length of its run, and its info string, trimmed. run is 0 if body
// opens none: it starts with no run of three backticks or tildes, or with backticks and has another in its info string.
func fenceOf(body string) (ch byte, run int, info string) {
	if body == "" || (body[0] != '`' && body[0] != '~') {
		return 0, 0, ""
	}
	ch, run = body[0], runLen(body, body[0])
	info = body[run:]
	if run < 3 || (ch == '`' && strings.IndexByte(info, '`') >= 0) {
		return 0, 0, ""
	}
	return ch, run, strings.Trim(info, " \t")
}

// closes says whether body closes a fence of run characters ch: a run of ch at least as long, and nothing but white space after.
func closes(body string, ch byte, run int) bool {
	n := runLen(body, ch)
	return n >= run && strings.Trim(body[n:], " \t") == ""
}

// word is the first word of info if it is a plain one, which a receiver may take for the code's language, and "" otherwise.
func word(info string) string {
	w, _, _ := strings.Cut(strings.ReplaceAll(info, "\t", " "), " ")
	plain := func(c rune) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_+.#", c)
	}
	if strings.IndexFunc(w, func(c rune) bool { return !plain(c) }) >= 0 {
		return ""
	}
	return w
}

// listMarker is the length of the list marker body starts with, or 0: "-", "*" or "+", or one to nine digits and "." or ")",
// followed by white space or the end of the line.
func listMarker(body string) int {
	n := 0
	switch c := body[0]; {
	case c == '-' || c == '*' || c == '+':
		n = 1
	case c >= '0' && c <= '9':
		for n < len(body) && body[n] >= '0' && body[n] <= '9' {
			n++
		}
		if n > 9 || n == len(body) || (body[n] != '.' && body[n] != ')') {
			return 0
		}
		n++
	default:
		return 0
	}
	if n < len(body) && body[n] != ' ' && body[n] != '\t' {
		return 0
	}
	return n
}

// paragraph says whether body, the text of a line after its white space and list marker, is text that a paragraph goes on
// with: it is not empty, not a heading and not a rule. After a paragraph's line (after), a line of "=" or "-" underlines it as a
// heading, which ends it too.
func paragraph(body string, after bool) bool {
	if body == "" || thematicBreak(body) || after && setextUnderline(body) {
		return false
	}
	n := runLen(body, '#')
	return n < 1 || n > 6 || (n < len(body) && body[n] != ' ' && body[n] != '\t')
}

// thematicBreak says whether body is a rule: three or more of one of "*", "-" and "_", and nothing else but spaces and tabs.
func thematicBreak(body string) bool {
	c := body[0]
	if c != '*' && c != '-' && c != '_' {
		return false
	}
	n := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case c:
			n++
		case ' ', '\t':
		default:
			return false
		}
	}
	return n >= 3
}

// setextUnderline says whether body is a run of "=" or of "-", and nothing after it but spaces and tabs.
func setextUnderline(body string) bool {
	c := body[0]
	if c != '=' && c != '-' {
		return false
	}
	return strings.Trim(body[runLen(body, c):], " \t") == ""
}

// runLen is the length of the run of c that s starts with.
func runLen(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// tableRows marks the lines that may be rows of a GFM table: every line of a group of lines with no blank line between them, one
// of which is a delimiter row. It errs towards marking: a table needs no more than such a row and the line before it, and ends at a
// blank line.
func tableRows(lines []string) []bool {
	rows := make([]bool, len(lines))
	for start := 0; start < len(lines); {
		end, table := start, false
		for ; end < len(lines) && strings.Trim(lines[end], " \t") != ""; end++ {
			table = table || delimiterRow(lines[end])
		}
		for j := start; j < end; j++ {
			rows[j] = table
		}
		start = end + 1
	}
	return rows
}

// delimiterRow says whether line may be the delimiter row of a GFM table, in a quote or a list item or not: after quote marks and
// white space, only "|", "-", ":" and white space, with a "|" and a "-" among them.
func delimiterRow(line string) bool {
	t := strings.TrimLeft(line, " \t>")
	return strings.Trim(t, "|-: \t") == "" && strings.Contains(t, "|") && strings.Contains(t, "-")
}
