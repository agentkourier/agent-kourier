package slack

import (
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
// The whole table is held back until it ends, since its last row may widen a column, and is then put back to be read
// as the lines of a block (mdText.fence does the same): the reading of blocks writes it, escapes it and cuts it.

// tableWidth is the widest line of an aligned table; a wider one is written a record a row, which a narrow window does
// not wrap into a mess.
const tableWidth = 80

// tableState is what table finds at the start of the text held.
type tableState int

const (
	noTable   tableState = iota // the line is not the start of a table
	tableWait                   // it may be: the text that says has not come
	tableRead                   // it is, and the text held now starts with the block it is written as
)

// table reads the lines held from the start of a line out of a block, and rewrites a table there into a block.
func (m *mdText) table(final bool) tableState {
	header, rest, ok := strings.Cut(m.held, "\n")
	if !isRow(header) {
		return noTable
	}
	if !ok {
		return waitUnless(final)
	}
	delim, after, hasNL := strings.Cut(rest, "\n")
	if !hasNL && !final {
		if strings.Trim(delim, "|:- \t") == "" {
			return tableWait
		}
		return noTable
	}
	head := splitRow(header)
	aligns, ok := delimiter(delim, len(head))
	if !ok {
		return noTable
	}
	rows := [][]string{head}
	end := len(m.held) // where the text after the table starts
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
			rows = append(rows, splitRow(line))
			if !lineNL {
				end = len(m.held)
				break
			}
			end += len(line) + 1
			after = more
		}
	}
	block := fenceMark + "\n" + renderTable(rows, aligns) + "\n" + fenceMark
	m.held = block + m.held[end:]
	return tableRead
}

// waitUnless is tableWait, but noTable at the end of the text, where what has not come never will.
func waitUnless(final bool) tableState {
	if final {
		return noTable
	}
	return tableWait
}

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
// and its "<" before "!", "@" or "#" written as code writes it, so that its width is what Slack shows.
func cellText(cell string) string {
	var b strings.Builder
	for i := 0; i < len(cell); {
		if cell[i] != '`' {
			p, size := codePiece(cell, i)
			b.WriteString(p)
			i += size
			continue
		}
		n := runLen(cell[i:], '`')
		if at := strings.IndexByte(cell[i+n:], '`'); at >= 0 && runLen(cell[i+n+at:], '`') == n {
			for content, k := cell[i+n:i+n+at], 0; k < len(content); {
				p, size := codePiece(content, k)
				b.WriteString(p)
				k += size
			}
			i += n + at + n
			continue
		}
		b.WriteString(cell[i : i+n])
		i += n
	}
	return b.String()
}

// renderTable writes rows, the header first, as the lines of a block: aligned in columns, or a record a row if that is
// wider than tableWidth. A row of fewer cells than the widest is padded, and none is cut.
func renderTable(rows [][]string, aligns []byte) string {
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	cells := make([][]string, len(rows))
	widths := make([]int, cols)
	for i, r := range rows {
		cells[i] = make([]string, cols)
		for j, c := range r {
			cells[i][j] = cellText(c)
		}
		for j, c := range cells[i] {
			widths[j] = max(widths[j], 3, utf8.RuneCountInString(c))
		}
	}
	line := 1
	for _, w := range widths {
		line += w + 3
	}
	if line > tableWidth && len(rows) > 1 && !strings.ContainsFunc(strings.Join(cells[0], ""), unfit) {
		return renderRecords(cells)
	}
	var b strings.Builder
	for i, r := range cells {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteByte('|')
		for j, c := range r {
			align := byte('l')
			if j < len(aligns) {
				align = aligns[j]
			}
			b.WriteString(" " + pad(c, widths[j], align) + " |")
		}
		if i == 0 {
			b.WriteString("\n|")
			for _, w := range widths {
				b.WriteString(strings.Repeat("-", w+2) + "|")
			}
		}
	}
	return b.String()
}

// unfit is a character a record's label may not have: one that could start a fence line, since a label starts a line
// of the block. An aligned row starts with "|", so a table with such a header is written aligned however wide.
func unfit(r rune) bool { return r == '`' || r == '~' }

// renderRecords writes each row after the header as lines of "Header: value", the values aligned, with a blank line
// between rows.
func renderRecords(cells [][]string) string {
	label := 0
	for _, h := range cells[0] {
		label = max(label, utf8.RuneCountInString(h)+1)
	}
	var b strings.Builder
	for i, r := range cells[1:] {
		if i > 0 {
			b.WriteString("\n\n")
		}
		for j, c := range r {
			if j > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(strings.TrimRightFunc(pad(cells[0][j]+":", label, 'l')+" "+c, unicode.IsSpace))
		}
	}
	return b.String()
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
