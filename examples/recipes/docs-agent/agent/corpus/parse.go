package corpus

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// section is the text under one heading, up to the next heading of any level. The text before the first heading is
// a section of level 0 with no heading.
type section struct {
	heading string
	anchor  string
	level   int
	// text is the section's own lines: the heading line and its subsections are not in it.
	text string
	// offset is where the heading line starts in parsed.body.
	offset int
}

// parsed is one Markdown file split into sections.
type parsed struct {
	title    string
	body     string
	sections []section
}

var (
	headingLine = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$`)
	mdLink      = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	attrList    = regexp.MustCompile(`\s*\{[^}]*\}\s*$`)
	spaces      = regexp.MustCompile(`\s+`)
	anchorCount = regexp.MustCompile(`^(.*)_([0-9]+)$`)
)

// parse splits src, the text of the Markdown file at filePath, into its title and sections. A heading is an ATX
// heading (#, ##, ...) outside a fenced code block; Setext headings are not recognised. The anchors follow the
// scheme of Python-Markdown's table of contents, which MkDocs uses, so they match the anchors of the published
// page, except that a non-ASCII letter is kept.
func parse(src, filePath string) parsed {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	front, body := splitFrontMatter(src)
	p := parsed{body: body, title: frontMatterTitle(front)}

	var (
		cur      = section{}
		inFence  bool
		fenceCh  byte
		fenceLen int
		used     = map[string]bool{}
		pos      int
		bodyFrom int // where the current section's own text starts
	)
	flush := func(end int) {
		cur.text = strings.TrimSpace(body[bodyFrom:end])
		if cur.level > 0 || cur.text != "" {
			p.sections = append(p.sections, cur)
		}
	}
	for pos < len(body) {
		end := strings.IndexByte(body[pos:], '\n')
		next := len(body)
		line := body[pos:]
		if end >= 0 {
			line = body[pos : pos+end]
			next = pos + end + 1
		}
		if fence := strings.TrimLeft(line, " \t"); strings.HasPrefix(fence, "```") || strings.HasPrefix(fence, "~~~") {
			ch := fence[0]
			n := len(fence) - len(strings.TrimLeft(fence, string(ch)))
			switch {
			case !inFence:
				inFence, fenceCh, fenceLen = true, ch, n
			case ch == fenceCh && n >= fenceLen && strings.TrimSpace(fence[n:]) == "":
				inFence = false
			}
		} else if m := headingLine.FindStringSubmatch(line); m != nil && !inFence {
			flush(pos)
			text := cleanHeading(m[2])
			cur = section{heading: text, level: len(m[1]), anchor: uniqueAnchor(slug(text), used), offset: pos}
			bodyFrom = next
			if p.title == "" && cur.level == 1 && text != "" {
				p.title = text
			}
		}
		pos = next
	}
	flush(len(body))
	if p.title == "" {
		p.title = strings.TrimSuffix(path.Base(filePath), path.Ext(filePath))
	}
	return p
}

// span returns the text of section i with its subsections: from its heading to the next heading of the same or a
// higher level. The text before the first heading ends at the first heading.
func (p parsed) span(i int) string {
	s := p.sections[i]
	end := len(p.body)
	for _, later := range p.sections[i+1:] {
		if s.level == 0 || later.level <= s.level {
			end = later.offset
			break
		}
	}
	return strings.TrimSpace(p.body[s.offset:end])
}

// splitFrontMatter separates a leading YAML block fenced by lines of three dashes from the rest.
func splitFrontMatter(src string) (front, body string) {
	rest, ok := strings.CutPrefix(src, "---\n")
	if !ok {
		return "", src
	}
	if i := strings.Index(rest, "\n---\n"); i >= 0 {
		return rest[:i], rest[i+len("\n---\n"):]
	}
	if front, ok := strings.CutSuffix(rest, "\n---"); ok {
		return front, ""
	}
	return "", src
}

// frontMatterTitle reads a top-level "title:" line of a front matter block. It is not a YAML parser: the
// documentation sources use plain or quoted scalars on one line.
func frontMatterTitle(front string) string {
	for _, line := range strings.Split(front, "\n") {
		if v, ok := strings.CutPrefix(line, "title:"); ok {
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			}
			return v
		}
	}
	return ""
}

// cleanHeading reduces a heading's Markdown to its text: links give their label, code and emphasis marks go, and
// a trailing attribute list is dropped.
func cleanHeading(s string) string {
	s = attrList.ReplaceAllString(s, "")
	s = mdLink.ReplaceAllString(s, "$1")
	s = strings.NewReplacer("`", "", "**", "", "__", "").Replace(s)
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

// slug is the anchor of a heading: lower case, letters, digits and underscores kept, runs of spaces and hyphens
// as one hyphen, everything else dropped.
func slug(s string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			if hyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			hyphen = false
			b.WriteRune(r)
		case unicode.IsSpace(r) || r == '-':
			hyphen = true
		}
	}
	return b.String()
}

// uniqueAnchor makes id unique among used, the way Python-Markdown does: a second "example" becomes "example_1",
// and a third "example_2". An empty id becomes "section".
func uniqueAnchor(id string, used map[string]bool) string {
	if id == "" {
		id = "section"
	}
	for used[id] {
		if m := anchorCount.FindStringSubmatch(id); m != nil {
			n, _ := strconv.Atoi(m[2])
			id = m[1] + "_" + strconv.Itoa(n+1)
		} else {
			id += "_1"
		}
	}
	used[id] = true
	return id
}
