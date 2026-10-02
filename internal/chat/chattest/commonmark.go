package chattest

import (
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// ScanCommonMark reads CommonMark, as in Slack's markdown block and a stream's markdown_text, with goldmark, the
// standard CommonMark parser for Go (strict, no extensions; used by tests only: go list -deps ./cmd/courier does
// not hold it). The structure is its: links inline and by reference, images, angle autolinks, raw HTML, emphasis,
// code, headings, quotes and lists, with every backslash escape, entity and label rule as CommonMark has it, which
// a hand-written reader got wrong one hole at a time. What goldmark does not know is read as the platform might:
// in the text between constructs, Slack's tokens ("<@U1>", "<!channel>", ...), bare URLs and email addresses, which
// a platform may link, and a "<" that opens nothing, which a platform may take as a tag.
//
// It errs towards finding a construct, and shows what a person sees: the text content of the tree, whose paragraphs
// are separated by a blank line. The adapter's own tests, against its platform, are the last word.
func ScanCommonMark(wire string) Shown {
	src := []byte(wire)
	doc := goldmark.New().Parser().Parse(text.NewReader(src))
	r := &cmReader{src: src}
	r.blocks(doc)
	return Shown{Displayed: r.out.String(), Active: r.active}
}

type cmReader struct {
	src    []byte
	out    strings.Builder
	active []Active
}

// blocks reads the children of a block, which are blocks, separated in what is shown as a list's items are by a
// line break, and others by a blank line. A link reference definition is not shown.
func (r *cmReader) blocks(parent ast.Node) {
	sep := "\n\n"
	if _, ok := parent.(*ast.List); ok {
		sep = "\n"
	}
	shown := false
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		if _, ok := c.(*ast.LinkReferenceDefinition); ok {
			r.active = append(r.active, Active{KindLink, "[" + string(c.(*ast.LinkReferenceDefinition).Label) + "]: " + string(c.(*ast.LinkReferenceDefinition).Destination)})
			continue
		}
		if shown {
			r.out.WriteString(sep)
		}
		shown = true
		r.block(c)
	}
}

func (r *cmReader) block(n ast.Node) {
	switch v := n.(type) {
	case *ast.Heading:
		r.active = append(r.active, Active{KindHeading, strings.Repeat("#", v.Level)})
		r.inlines(n)
	case *ast.Blockquote:
		r.active = append(r.active, Active{KindQuote, ">"})
		r.blocks(n)
	case *ast.List:
		r.blocks(n)
	case *ast.ListItem:
		r.active = append(r.active, Active{KindList, "-"})
		r.blocks(n)
	case *ast.FencedCodeBlock, *ast.CodeBlock:
		var b strings.Builder
		for i := 0; i < n.Lines().Len(); i++ {
			seg := n.Lines().At(i)
			b.Write(seg.Value(r.src))
		}
		r.code(b.String())
		r.out.WriteString(strings.TrimSuffix(b.String(), "\n"))
	case *ast.HTMLBlock:
		var b strings.Builder
		for i := 0; i < n.Lines().Len(); i++ {
			seg := n.Lines().At(i)
			b.Write(seg.Value(r.src))
		}
		r.leaf(b.String())
	case *ast.ThematicBreak:
		// A rule is cosmetic, and nothing is shown for it.
	case *ast.Paragraph, *ast.TextBlock:
		r.inlines(n)
	default:
		if n.HasChildren() && n.Type() == ast.TypeBlock {
			r.blocks(n)
		} else {
			r.inlines(n)
		}
	}
}

// inlines reads the inline children of n. Goldmark cuts the text between constructs into pieces at every character
// that could open one, so the pieces that follow each other are read as the one text they are: a Slack token
// that goldmark does not know ("<@U1>") must not be cut in two.
func (r *cmReader) inlines(n ast.Node) {
	var pending strings.Builder
	flush := func() {
		if pending.Len() > 0 {
			r.leaf(pending.String())
			pending.Reset()
		}
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			pending.Write(t.Segment.Value(r.src))
			if t.SoftLineBreak() || t.HardLineBreak() {
				pending.WriteByte('\n')
			}
			continue
		}
		flush()
		r.inline(c)
	}
	flush()
}

func (r *cmReader) inline(n ast.Node) {
	switch v := n.(type) {
	case *ast.String:
		r.out.Write(v.Value)
	case *ast.CodeSpan:
		var b strings.Builder
		for c := v.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*ast.Text); ok {
				b.Write(t.Segment.Value(r.src))
			}
		}
		s := strings.ReplaceAll(b.String(), "\n", " ")
		r.code("`" + s + "`")
		r.out.WriteString(s)
	case *ast.Emphasis:
		r.active = append(r.active, Active{KindEmphasis, strings.Repeat("*", v.Level)})
		r.inlines(n)
	case *ast.Link:
		shown := r.shownIn(n)
		r.active = append(r.active, Active{KindLink, "[" + shown + "](" + string(v.Destination) + ")"})
		r.out.WriteString(shown)
	case *ast.Image:
		shown := r.shownIn(n)
		r.active = append(r.active, Active{KindImage, "![" + shown + "](" + string(v.Destination) + ")"})
		r.out.WriteString(shown)
	case *ast.AutoLink:
		r.leaf("<" + string(v.URL(r.src)) + ">")
	case *ast.RawHTML:
		var b strings.Builder
		for i := 0; i < v.Segments.Len(); i++ {
			seg := v.Segments.At(i)
			b.Write(seg.Value(r.src))
		}
		r.leaf(b.String())
	default:
		r.inlines(n)
	}
}

// code reports a code span or a block, and what in it a platform might still act on. CommonMark's code is
// literal, but whether Slack's markdown block leaves a token, a link or a URL alone inside code is not known
// (UNCONFIRMED(WP-04)), so, as ScanSlack does, they are reported there too, and an adapter must not rely on code to
// make its text inert. An indented block is code like a fenced one.
func (r *cmReader) code(text string) {
	r.active = append(r.active, Active{KindCode, text})
	inner := &cmReader{src: r.src}
	inner.leaf(text)
	for _, a := range inner.active {
		if !a.Kind.Style() {
			r.active = append(r.active, a)
		}
	}
}

// shownIn is the text that a link or an image shows: the text of what is inside it, in which nothing is reported
// again, since a platform makes no link, and no autolink, inside a link.
func (r *cmReader) shownIn(n ast.Node) string {
	inner := &cmReader{src: r.src}
	inner.inlines(n)
	return inner.out.String()
}

// leaf reads text that goldmark leaves as text, or that is raw markup it took (a tag, an angle autolink), with the
// rules of the text between constructs: a backslash makes the ASCII punctuation after it a character, entities are
// characters, a "<" opens a Slack token or a tag, and a bare URL or an email address is autolinked.
func (r *cmReader) leaf(s string) {
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && isPunct(s[i+1]):
			r.out.WriteByte(s[i+1])
			i += 2
		case c == '&':
			i += entity(s[i:], commonMarkDialect, &r.out)
		case c == '<':
			i += token(s[i:], &r.out, &r.active)
		case wordStart(s, i) && (autolink.MatchString(s[i:]) || email.MatchString(s[i:])):
			url := autolink.FindString(s[i:])
			if url == "" {
				url = email.FindString(s[i:])
			}
			r.active = append(r.active, Active{KindAutolink, url})
			r.out.WriteString(url)
			i += len(url)
		default:
			r.out.WriteByte(c)
			i++
		}
	}
}
