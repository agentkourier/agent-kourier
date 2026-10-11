package main

import (
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
)

var (
	// scheme matches a URL with a scheme (https:, mailto:), which is not a file in the repository.
	scheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
	// codeSpan matches an inline code span, in which a link is an example, not a link.
	codeSpan = regexp.MustCompile("`+[^`]*`+")
	// inlineLink matches the target of [text](target "title") and ![alt](target).
	inlineLink = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+(?:"[^"]*"|'[^']*'))?\s*\)`)
	// refDef matches the target of a reference definition, [label]: target.
	refDef = regexp.MustCompile(`^\s{0,3}\[[^\]]+\]:\s*<?([^\s>]+)>?`)
	// htmlLink matches href and src attributes of inline HTML.
	htmlLink = regexp.MustCompile(`\b(?:href|src)=["']([^"']+)["']`)

	// atxHeading matches a heading line and captures its text; a closing run of # needs a space before it.
	atxHeading = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$`)
	// htmlID matches an id or name attribute of inline HTML, which is an anchor too.
	htmlID = regexp.MustCompile(`\b(?:id|name)=["']([^"']+)["']`)
	// mdLink, htmlTag and emphasis strip inline Markdown from a heading to leave the text that is slugged.
	mdLink   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	htmlTag  = regexp.MustCompile(`<[^>]+>`)
	emphasis = regexp.MustCompile("[*`]")
	// codeSpanText matches a code span, whose underscores are literal.
	codeSpanText = regexp.MustCompile("`[^`]*`")
	// underscoreEm matches _text_ or __text__ with no word character outside it: an underscore inside an identifier
	// (chat_triggers) is not emphasis.
	underscoreEm = regexp.MustCompile(`(^|[^\p{L}\p{N}_])_{1,2}([^_]+)_{1,2}($|[^\p{L}\p{N}_])`)
)

// linkChecker checks the relative links of Markdown files against a file system rooted at the repository.
type linkChecker struct {
	fsys fs.FS
	ids  map[string]map[string]bool // the anchors of a Markdown file, by path, read on first use
}

// fence follows fenced code blocks through a file, line by line.
type fence struct {
	mark string // the opening run of ` or ~, empty outside a block
}

// skip reports whether line is a fence line or inside a fenced block, and advances the state.
func (f *fence) skip(line string) bool {
	t := strings.TrimLeft(line, " \t")
	if f.mark == "" {
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			f.mark = t[:len(t)-len(strings.TrimLeft(t, t[:1]))]
			return true
		}
		return false
	}
	// A closing fence is the opening character at least as many times, and nothing but spaces after it.
	if closing := strings.TrimRight(t, " \t"); strings.HasPrefix(closing, f.mark) && strings.Trim(closing, closing[:1]) == "" {
		f.mark = ""
	}
	return true
}

// check returns a finding for each relative link of text (the content of file) that does not resolve.
func (c *linkChecker) check(file, text string) []finding {
	if c.ids == nil {
		c.ids = map[string]map[string]bool{}
	}
	c.ids[file] = headingIDs(text) // a link to #anchor in the file itself reads the text in hand
	var out []finding
	var fn fence
	for i, line := range strings.Split(text, "\n") {
		if fn.skip(line) {
			continue
		}
		prose := codeSpan.ReplaceAllString(line, "")
		var targets []string
		for _, m := range inlineLink.FindAllStringSubmatch(prose, -1) {
			targets = append(targets, m[1])
		}
		if m := refDef.FindStringSubmatch(prose); m != nil {
			targets = append(targets, m[1])
		}
		for _, m := range htmlLink.FindAllStringSubmatch(prose, -1) {
			targets = append(targets, m[1])
		}
		for _, target := range targets {
			if rule, msg := c.problem(file, target); msg != "" {
				f := finding{path: file, line: i + 1, rule: rule, msg: fmt.Sprintf("link %q: %s", target, msg), text: line}
				if at := strings.Index(line, target); at >= 0 {
					f.start, f.end = at, at+len(target)
				}
				out = append(out, f)
			}
		}
	}
	return out
}

// problem resolves target as a link from file. It returns the rule and message when it does not resolve, and an empty
// message when it does or when it is not a repository path.
func (c *linkChecker) problem(file, target string) (rule, msg string) {
	if scheme.MatchString(target) || strings.HasPrefix(target, "//") {
		return "", ""
	}
	p, frag, _ := strings.Cut(target, "#")
	p, _, _ = strings.Cut(p, "?")
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	if u, err := url.PathUnescape(frag); err == nil {
		frag = u
	}
	var resolved string
	switch {
	case p == "":
		resolved = file
	case strings.HasPrefix(p, "/"):
		resolved = path.Clean(strings.TrimPrefix(p, "/"))
	default:
		resolved = path.Join(path.Dir(file), p)
	}
	if !fs.ValidPath(resolved) {
		return "dead-link", "points outside the repository"
	}
	info, err := fs.Stat(c.fsys, resolved)
	if err != nil {
		return "dead-link", resolved + " does not exist"
	}
	if frag == "" || info.IsDir() || !strings.HasSuffix(resolved, ".md") {
		return "", ""
	}
	ids, err := c.anchors(resolved)
	if err != nil {
		return "dead-link", fmt.Sprintf("cannot read %s: %v", resolved, err)
	}
	if !ids[frag] {
		return "dead-anchor", fmt.Sprintf("%s has no heading or anchor %q", resolved, frag)
	}
	return "", ""
}

// anchors returns the anchor IDs of a Markdown file, cached.
func (c *linkChecker) anchors(p string) (map[string]bool, error) {
	if ids, ok := c.ids[p]; ok {
		return ids, nil
	}
	b, err := fs.ReadFile(c.fsys, p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	if c.ids == nil {
		c.ids = map[string]map[string]bool{}
	}
	c.ids[p] = headingIDs(string(b))
	return c.ids[p], nil
}

// headingIDs returns the anchors a Markdown file defines as GitHub renders it, which is how the READMEs' links are
// followed: the slug of each ATX heading outside code fences (a Setext heading is not read), the -1, -2 suffixes of a
// repeated heading, and the id and name attributes of inline HTML. GitHub does not read an attr_list {#id}: it shows
// the braces and slugs them with the rest of the heading text. mkdocs's own slug rules do not apply, because the READMEs
// are outside docs_dir and the site never renders them.
func headingIDs(md string) map[string]bool {
	ids := map[string]bool{}
	seen := map[string]int{}
	var fn fence
	for _, line := range strings.Split(md, "\n") {
		if fn.skip(line) {
			continue
		}
		for _, m := range htmlID.FindAllStringSubmatch(line, -1) {
			ids[m[1]] = true
		}
		m := atxHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		text := mdLink.ReplaceAllString(m[1], "$1")
		text = htmlTag.ReplaceAllString(text, "")
		text = emphasis.ReplaceAllString(stripUnderscoreEmphasis(text), "")
		slug := githubSlug(text)
		if n := seen[slug]; n == 0 {
			ids[slug] = true
		} else {
			ids[fmt.Sprintf("%s-%d", slug, n)] = true
		}
		seen[slug]++
	}
	return ids
}

// stripUnderscoreEmphasis removes the underscores of _emphasis_ and __strong__ outside code spans, as the host renders
// them, and keeps an identifier's underscores and a code span's.
func stripUnderscoreEmphasis(s string) string {
	spans := codeSpanText.FindAllStringIndex(s, -1)
	var b strings.Builder
	prev := 0
	plain := func(t string) {
		// Adjacent emphasis (_a_ _b_) shares its boundary character, so replace until nothing changes.
		for {
			n := underscoreEm.ReplaceAllString(t, "$1$2$3")
			if n == t {
				break
			}
			t = n
		}
		b.WriteString(t)
	}
	for _, sp := range spans {
		plain(s[prev:sp[0]])
		b.WriteString(s[sp[0]:sp[1]])
		prev = sp[1]
	}
	plain(s[prev:])
	return b.String()
}

// githubSlug follows GitHub's heading anchors: lowercase, drop punctuation, and turn each space into a hyphen. A non-ASCII
// letter is kept as it is.
func githubSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsSpace(r):
			b.WriteByte('-')
		case keep(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// keep reports whether a slug keeps r: a letter, digit, underscore or hyphen (a space becomes a hyphen).
func keep(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}
