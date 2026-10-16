package corpus

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseTitle(t *testing.T) {
	tests := []struct {
		name, src, fallback, want string
	}{
		{"first H1", "# Hello world\n\ntext\n", "x.md", "Hello world"},
		{"front matter wins", "---\ntitle: From front matter\n---\n# Heading\n", "x.md", "From front matter"},
		{"quoted front matter title", "---\ntitle: \"Quoted: title\"\n---\nbody\n", "x.md", "Quoted: title"},
		{"front matter without a title falls back to the H1", "---\ntags: [a]\n---\n# Heading\n", "x.md", "Heading"},
		{"no heading falls back to the file name", "just text\n", "how-to/connect-agent.md", "connect-agent"},
		{"an H2 does not title the page", "## Section\n", "a/b.md", "b"},
		{"a heading inside a fence is not the title", "```\n# not a title\n```\n# Real\n", "x.md", "Real"},
		{"inline code and links are cleaned", "# The `a2a` [dialect](x.md) guide\n", "x.md", "The a2a dialect guide"},
		{"closing hashes are dropped", "# Title ##\n", "x.md", "Title"},
		{"CRLF line endings", "# Title\r\n\r\ntext\r\n", "x.md", "Title"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parse(tt.src, tt.fallback)
			if got.title != tt.want {
				t.Errorf("title = %q, want %q", got.title, tt.want)
			}
		})
	}
}

func TestParseSections(t *testing.T) {
	src := "intro line\n\n# One\n\nbody one\n\n## Two\n\n```sh\n# a comment in a fence\n```\n\n~~~\n## also in a fence\n~~~\n\n### Three\n\nthree\n\n## Two\n\nsecond two\n"
	got := parse(src, "x.md")
	var headings []string
	for _, s := range got.sections {
		headings = append(headings, s.anchor+"|"+s.heading)
	}
	want := []string{"|", "one|One", "two|Two", "three|Three", "two_1|Two"}
	if !reflect.DeepEqual(headings, want) {
		t.Errorf("sections = %q, want %q", headings, want)
	}
	two := got.sections[2]
	if !strings.Contains(two.text, "a comment in a fence") || !strings.Contains(two.text, "also in a fence") {
		t.Errorf("fenced lines must stay in their section's text: %q", two.text)
	}
	if strings.Contains(two.text, "three") {
		t.Errorf("a section's own text excludes its subsections: %q", two.text)
	}
	if got.sections[0].level != 0 || got.sections[1].level != 1 || got.sections[3].level != 3 {
		t.Errorf("levels = %d %d %d", got.sections[0].level, got.sections[1].level, got.sections[3].level)
	}
}

func TestParseNoIntroSection(t *testing.T) {
	got := parse("# Only\n\ntext\n", "x.md")
	if len(got.sections) != 1 || got.sections[0].heading != "Only" {
		t.Errorf("sections = %+v, want the one heading and no empty intro", got.sections)
	}
}

func TestParseSpan(t *testing.T) {
	got := parse("# A\n\na\n\n## B\n\nb\n\n### C\n\nc\n\n## D\n\nd\n", "x.md")
	span := got.span
	if s := span(1); !strings.Contains(s, "## B") || !strings.Contains(s, "### C") || strings.Contains(s, "## D") {
		t.Errorf("span of B = %q: want B and its subsection C, not D", s)
	}
	if s := span(0); !strings.Contains(s, "## D") {
		t.Errorf("span of the H1 = %q: want the whole page", s)
	}
}

func TestSlug(t *testing.T) {
	tests := []struct{ in, want string }{
		{"What each field does", "what-each-field-does"},
		{"Names the regexes take, and values the prompt takes", "names-the-regexes-take-and-values-the-prompt-takes"},
		{"Use `maxRuns` here", "use-maxruns-here"},
		{"snake_case stays", "snake_case-stays"},
		{"  spaced   out  ", "spaced-out"},
		{"!!!", ""},
	}
	for _, tt := range tests {
		if got := slug(tt.in); got != tt.want {
			t.Errorf("slug(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
