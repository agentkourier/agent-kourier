package slack

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A pipe table is written as a block of code, its columns aligned. Slack makes a table of the markdown of one, and in
// the reply of 2026-10-18 the thread showed blank lines where its rows were (the message's text held one private-use
// character, U+E005, in its place), so the answer lost its evidence. A block is what Slack shows as written (rows 01, 02).
//
// A table is a header line that starts with "|", after white space, a delimiter line of as many cells, and the lines
// after them that start with "|", up to the first that does not. A table without the leading pipe is prose, as it was:
// that keeps a line of prose from being held back until its end in case it is a table's header.
//
// A table is held back until it ends, since its last row may widen a column, but no longer than its hold (tableHold):
// its columns are as wide as the rows that fit in that make them, and each row after those is written as it comes, in
// those columns. A table that would be much longer written than as the agent wrote it, aligned or a record a row, is
// written compact instead: its cells between pipes, unpadded (tooLong). The block is written here, not put back to be
// read as a block's lines are, since a row read again would be taken for a row of the table; its text goes through
// codePiece all the same (putCode), whatever cellText did.

// tableWidth is the widest line of an aligned table; a wider one is written a record a row, which a narrow window does
// not wrap into a mess.
const tableWidth = 80

// tableHold is about the most of a table, as it is written or as the agent wrote it, that is held back to align its
// columns. A table held whole would go out in one text, which the limit of a text can cut (and the rows and the text
// after the cut with it), and would leave a stream showing nothing for as long as the agent writes it.
const tableHold = 4000

// tooLong says whether a table of raw runes as the agent wrote it is too long at out runes written, aligned or a record
// a row: half as long again, and a quarter of the hold more, which a short table never is.
func tooLong(out, raw, hold int) bool { return out > raw*3/2+hold/4 }

// openTable is the table whose block is open: its layout, and its length so far as the agent wrote it and as it is
// written, by which its rows go compact once they are tooLong.
type openTable struct {
	layout   *tableLayout
	raw, out int
}

// holdSize is the hold of a table: tableHold, unless a test sets less.
func (m *mdText) holdSize() int {
	if m.hold > 0 {
		return m.hold
	}
	return tableHold
}

// tableState is what table did with the start of the text held.
type tableState int

const (
	noTable   tableState = iota // the line is not the start of a table
	tableWait                   // it may be: the text that says has not come
	tableDone                   // it is, and its block is written: whole, or open for the rows after (m.tbl)
	tableFull                   // it is, and what is written of it did not fit
)

// table reads the lines held from the start of a line out of a block, and writes a table there as a block.
func (m *mdText) table(final bool, o *mdOut) tableState {
	header, rest, ok := strings.Cut(m.held, "\n")
	if !isRow(header) {
		return noTable
	}
	if !ok {
		return waitUnless(final)
	}
	delim, after, hasNL := strings.Cut(rest, "\n")
	if !hasNL && !final {
		if strings.TrimFunc(delim, delimiterRune) == "" {
			return tableWait
		}
		return noTable
	}
	head := splitRow(header)
	aligns, ok := delimiter(delim, len(head))
	if !ok {
		return noTable
	}
	hold := m.holdSize()
	var f tableFit
	f.add(head, header)
	f.raw += utf8.RuneCountInString(delim) + 1
	end, ended := len(m.held), true // where the text after what is written starts, and whether the table ends there
	if hasNL {
		end = len(header) + 1 + len(delim) + 1
		for {
			line, more, lineNL := strings.Cut(after, "\n")
			if !lineNL && !final {
				if t := strings.TrimLeftFunc(line, unicode.IsSpace); t == "" || t[0] == '|' {
					return tableWait // a row may be coming
				}
			}
			if !isRow(line) {
				end-- // the line break before this line is the one after the table
				break
			}
			g := f.with(splitRow(line), line)
			if len(f.rows) > 1 && (g.size(hold) > hold || g.raw > hold) {
				ended = false // this row and the ones after it are written as they come
				break
			}
			f = g
			if !lineNL {
				end = len(m.held)
				break
			}
			end += len(line) + 1
			after = more
		}
	}
	l := f.layout(aligns, hold)
	text := l.render(f.rows)
	if !o.put(fenceMark + "\n") {
		return tableFull
	}
	m.code, m.indent = true, 0
	m.tbl = openTable{layout: l, raw: f.raw, out: utf8.RuneCountInString(text) + 1}
	if !putCode(text, o) {
		return tableFull
	}
	if !ended {
		if !o.put("\n") {
			return tableFull
		}
		m.held = m.held[end:]
		return tableDone
	}
	if !o.put("\n" + fenceMark) {
		return tableFull
	}
	m.code, m.tbl = false, openTable{}
	m.held, m.inLine = m.held[end:], true // the line break after the table, if any, ends the line of the fence
	return tableDone
}

// tableRow reads the line held at the start of a line in the open block of a table: a row is written in the table's
// layout, compact from the first that would make the table tooLong, and any other line closes the block before it. It
// says whether to go on, as step does.
func (m *mdText) tableRow(final bool, o *mdOut) bool {
	line, _, hasNL := strings.Cut(m.held, "\n")
	if !hasNL && !final {
		if t := strings.TrimLeftFunc(line, unicode.IsSpace); t == "" || t[0] == '|' {
			return false // a row may be coming
		}
	}
	if !isRow(line) {
		if !o.put(fenceMark + "\n") {
			return false
		}
		m.code, m.tbl = false, openTable{}
		return true
	}
	cells := splitRow(line)
	text := m.tbl.layout.row(cells)
	raw := m.tbl.raw + utf8.RuneCountInString(line) + 1
	if m.tbl.layout.form != formCompact && tooLong(m.tbl.out+utf8.RuneCountInString(text)+1, raw, m.holdSize()) {
		m.tbl.layout = m.tbl.layout.compact()
		text = m.tbl.layout.row(cells)
	}
	if !putCode(text, o) {
		return false
	}
	m.tbl.raw, m.tbl.out = raw, m.tbl.out+utf8.RuneCountInString(text)+1
	if !hasNL { // the end of the text, and of the table
		if !o.put("\n" + fenceMark) {
			return false
		}
		m.code, m.tbl, m.held = false, openTable{}, ""
		return false
	}
	if !o.put("\n") {
		return false
	}
	m.held = m.held[len(line)+1:]
	return true
}

// putCode writes s as the text of a block is written: raw, but for "<" before "!", "@" or "#" (codePiece).
func putCode(s string, o *mdOut) bool {
	for i := 0; i < len(s); {
		p, size := codePiece(s, i)
		if !o.put(p) {
			return false
		}
		i += size
	}
	return true
}

// waitUnless is tableWait, but noTable at the end of the text, where what has not come never will.
func waitUnless(final bool) tableState {
	if final {
		return noTable
	}
	return tableWait
}

// delimiterRune is a rune a delimiter line may have, as delimiter and splitRow read it.
func delimiterRune(r rune) bool { return r == '|' || r == ':' || r == '-' || unicode.IsSpace(r) }

// isRow says whether line starts with "|", after white space.
func isRow(line string) bool {
	return strings.HasPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), "|")
}

// delimiter reads the delimiter line of a table of n columns: its cells are dashes, with a colon at either end or
// both, and it has a pipe. It returns each column's alignment: 'l', 'r' or 'c'.
func delimiter(line string, n int) ([]byte, bool) {
	if !strings.Contains(line, "|") {
		return nil, false // "| a" over "---" is a heading
	}
	cells := splitRow(line)
	if len(cells) != n {
		return nil, false
	}
	aligns := make([]byte, n)
	for i, c := range cells {
		dashes := strings.TrimSuffix(strings.TrimPrefix(c, ":"), ":")
		if dashes == "" || strings.Trim(dashes, "-") != "" {
			return nil, false
		}
		left, right := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		switch {
		case left && right:
			aligns[i] = 'c'
		case right:
			aligns[i] = 'r'
		default:
			aligns[i] = 'l'
		}
	}
	return aligns, true
}

// splitRow is the cells of a row: split at each pipe that is not escaped, without the pipes at its ends, each trimmed.
func splitRow(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	if strings.HasSuffix(s, "|") && !strings.HasSuffix(s, `\|`) {
		s = s[:len(s)-1]
	}
	var cells []string
	var cell strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == '|':
			cell.WriteByte('|')
			i++
		case s[i] == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(s[i])
		}
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

// cellText is a cell as it is shown in the block: its code spans without their backticks, since all of it is code,
// and then its "<" before "!", "@" or "#" written as code writes it, so that its width is what Slack shows.
func cellText(cell string) string {
	var b strings.Builder
	for i := 0; i < len(cell); {
		if cell[i] != '`' {
			b.WriteByte(cell[i])
			i++
			continue
		}
		n := runLen(cell[i:], '`')
		if at := strings.IndexByte(cell[i+n:], '`'); at >= 0 && runLen(cell[i+n+at:], '`') == n {
			b.WriteString(cell[i+n : i+n+at])
			i += n + at + n
			continue
		}
		b.WriteString(cell[i : i+n])
		i += n
	}
	s := b.String()
	b.Reset()
	for i := 0; i < len(s); {
		p, size := codePiece(s, i)
		b.WriteString(p)
		i += size
	}
	return b.String()
}

// tableFit is the rows of a table that its layout is decided from, as cellText shows them, the header first.
//
// A width is a count of runes, which is the width a monospace font gives most text; a wide character (CJK, most
// emoji) takes two columns, and pushes the rest of its row out.
type tableFit struct {
	rows   [][]string
	widths []int // each column's widest cell, and 3 at least
	chars  int   // the runes of the cells of the rows after the header
	raw    int   // the runes of the table's lines as the agent wrote them, each with its line break
}

// with is f and a row more, which leaves f as it is.
func (f tableFit) with(row []string, line string) tableFit {
	f.widths = slices.Clone(f.widths)
	f.add(row, line)
	return f
}

// add adds a row, its cells as splitRow reads them from line.
func (f *tableFit) add(row []string, line string) {
	f.raw += utf8.RuneCountInString(line) + 1
	cells := make([]string, len(row))
	for j, c := range row {
		cells[j] = cellText(c)
		n := utf8.RuneCountInString(cells[j])
		if j == len(f.widths) {
			f.widths = append(f.widths, 3)
		}
		f.widths[j] = max(f.widths[j], n)
		if len(f.rows) > 0 {
			f.chars += n
		}
	}
	f.rows = append(f.rows, cells)
}

// line is the width of a line of the table aligned.
func (f *tableFit) line() int {
	n := 1
	for _, w := range f.widths {
		n += w + 3
	}
	return n
}

// records says whether the table is written a record a row: it is wider than tableWidth, it has rows, and no line of
// its header can start a fence (unfit).
func (f *tableFit) records() bool {
	return f.line() > tableWidth && len(f.rows) > 1 && !strings.ContainsFunc(strings.Join(f.rows[0], ""), unfit)
}

// label is the width of a record's labels: the widest header cell, a column past the header's an empty one, and a colon.
func (f *tableFit) label() int {
	n := 1
	for _, h := range f.rows[0] {
		n = max(n, utf8.RuneCountInString(h)+1)
	}
	return n
}

// spread is about how long the table is written aligned, or a record a row if it is too wide for that.
func (f *tableFit) spread() int {
	if !f.records() {
		return (len(f.rows) + 1) * (f.line() + 1) // its rows and the delimiter, each with a line break
	}
	rows, cols := len(f.rows)-1, len(f.widths)
	return rows*cols*(f.label()+2) + f.chars + 2*rows
}

// form is how the table is written: aligned, or a record a row if it is too wide for that, unless that is tooLong.
func (f *tableFit) form(hold int) tableForm {
	switch {
	case tooLong(f.spread(), f.raw, hold):
		return formCompact
	case f.records():
		return formRecords
	}
	return formAligned
}

// size is about how long the table is, written.
func (f *tableFit) size(hold int) int {
	if f.form(hold) == formCompact {
		return f.raw
	}
	return f.spread()
}

// layout is how the table is written, from the rows so far: the rows after them do not change it, but to make it compact.
func (f *tableFit) layout(aligns []byte, hold int) *tableLayout {
	head := make([]string, len(f.widths))
	copy(head, f.rows[0])
	return &tableLayout{head: head, aligns: aligns, widths: f.widths, form: f.form(hold), label: f.label()}
}

// tableForm is the form a table's rows are written in.
type tableForm int

const (
	formAligned tableForm = iota // in columns, padded
	formRecords                  // a record a row
	formCompact                  // the cells between pipes, unpadded
)

// tableLayout is how the rows of a table are written. It does not change once it is made.
type tableLayout struct {
	head   []string // the header's cells as shown, one a column
	aligns []byte   // the alignment of the header's columns, as delimiter reads it
	widths []int
	form   tableForm
	label  int
}

// compact is l, its rows written compact.
func (l *tableLayout) compact() *tableLayout {
	c := *l
	c.form = formCompact
	return &c
}

// render writes rows, the header first, as the lines of a block: in columns, with the delimiter after the header, or
// each row after the header a record, with a blank line between them, or compact, with a delimiter of three dashes a
// column.
func (l *tableLayout) render(rows [][]string) string {
	switch l.form {
	case formCompact:
		var b strings.Builder
		b.WriteString(compactRow(rows[0]) + "\n|" + strings.Repeat("---|", len(rows[0])))
		for _, r := range rows[1:] {
			b.WriteString("\n" + compactRow(r))
		}
		return b.String()
	case formRecords:
		recs := make([]string, 0, len(rows)-1)
		for _, r := range rows[1:] {
			recs = append(recs, l.record(r))
		}
		return strings.Join(recs, "\n\n")
	}
	var b strings.Builder
	b.WriteString(l.aligned(rows[0]))
	b.WriteString("\n|")
	for _, w := range l.widths {
		b.WriteString(strings.Repeat("-", w+2) + "|")
	}
	for _, r := range rows[1:] {
		b.WriteString("\n" + l.aligned(r))
	}
	return b.String()
}

// row writes a row after the ones render wrote, of cells as splitRow reads them: a record after a blank line.
func (l *tableLayout) row(raw []string) string {
	cells := make([]string, len(raw))
	for j, c := range raw {
		cells[j] = cellText(c)
	}
	switch l.form {
	case formCompact:
		return compactRow(cells)
	case formRecords:
		return "\n" + l.record(cells)
	}
	return l.aligned(cells)
}

// compactRow is a row as its cells between pipes, unpadded.
func compactRow(r []string) string { return "| " + strings.Join(r, " | ") + " |" }

// aligned is a row as a line of columns. A short row is padded, and none is cut: a cell wider than its column, or past
// the last, pushes the rest of its row out.
func (l *tableLayout) aligned(r []string) string {
	var b strings.Builder
	b.WriteByte('|')
	for j := range max(len(r), len(l.widths)) {
		c, w, align := "", 0, byte('l')
		if j < len(r) {
			c = r[j]
		}
		if j < len(l.widths) {
			w = l.widths[j]
		}
		if j < len(l.aligns) {
			align = l.aligns[j]
		}
		b.WriteString(" " + pad(c, w, align) + " |")
	}
	return b.String()
}

// unfit is a character a record's label may not have: one that could start a fence line, since a label starts a line
// of the block. An aligned row starts with "|", so a table with such a header is written aligned however wide.
func unfit(r rune) bool { return r == '`' || r == '~' }

// record is a row as lines of "Header: value", the values aligned. A cell past the header's has an empty label.
func (l *tableLayout) record(r []string) string {
	lines := make([]string, max(len(r), len(l.head)))
	for j := range lines {
		h, c := "", ""
		if j < len(l.head) {
			h = l.head[j]
		}
		if j < len(r) {
			c = r[j]
		}
		lines[j] = strings.TrimRightFunc(pad(h+":", l.label, 'l')+" "+c, unicode.IsSpace)
	}
	return strings.Join(lines, "\n")
}

// pad is s padded with spaces to width characters, at its right ('l'), its left ('r') or both ('c').
func pad(s string, width int, align byte) string {
	gap := max(width-utf8.RuneCountInString(s), 0)
	switch align {
	case 'r':
		return strings.Repeat(" ", gap) + s
	case 'c':
		return strings.Repeat(" ", gap/2) + s + strings.Repeat(" ", gap-gap/2)
	}
	return s + strings.Repeat(" ", gap)
}
