package slack

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/render"
)

// Agent Kourier notifies the people a reply names outside code (render.Mentions), and the adapter shows code as the block or
// span it writes (mdText). They read the same text for what is code with two functions in two packages, which only comments
// kept in step. These tests read what the adapter wrote for the names in its prose, outside its blocks and spans, and compare
// them with the people render.Mentions made of the text the agent wrote, over every shape of fence line, opener, block line
// and closer that the two have a rule for: white space, list markers and quote marks, and the marks that are neither.
//
// The test is here because only the adapter, the broker and cmd may import the adapter, and this package may import render.

var mentionsDeclared = []v1alpha1.ChatMention{
	{Name: "ann", User: "U0ANN"}, {Name: "bob", User: "U0BOB"}, {Name: "joe", User: "U0JOE"}, {Name: "kim", User: "U0KIM"},
	{Name: "lee", User: "U0LEE"}, {Name: "sam", User: "U0SAM"}, {Name: "zed", User: "U0ZED"},
}

var nameInProse = regexp.MustCompile("(?:^|[\\s\\p{Z}(`~])@([a-z]+)")

// proseNames is the declared names that the text the adapter sends has outside the blocks it writes (a line of "```" alone
// opens or closes one, which is all the adapter writes as a fence) and the spans in its prose, sorted.
func proseNames(sent string) []string {
	var prose strings.Builder
	code := false
	for line := range strings.SplitSeq(sent, "\n") {
		if line == fenceMark {
			code = !code
			continue
		}
		if !code {
			prose.WriteString(withoutSpans(line) + "\n")
		}
	}
	var names []string
	for _, m := range nameInProse.FindAllStringSubmatch(prose.String(), -1) {
		if slices.ContainsFunc(mentionsDeclared, func(d v1alpha1.ChatMention) bool { return d.Name == m[1] }) && !slices.Contains(names, m[1]) {
			names = append(names, m[1])
		}
	}
	slices.Sort(names)
	return names
}

// withoutSpans is line without the spans the adapter writes in prose: a backtick not escaped, to the next one.
func withoutSpans(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			b.WriteString(line[i:min(i+2, len(line))])
			i++
		case '`':
			if end := unescapedBacktick(line, i+1); end >= 0 {
				i = end
				continue
			}
			b.WriteByte('`')
		default:
			b.WriteByte(line[i])
		}
	}
	return b.String()
}

// unescapedBacktick is where in s from i on the next backtick that is not escaped is, or -1.
func unescapedBacktick(s string, i int) int {
	for ; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '`':
			return i
		}
	}
	return -1
}

// madeNames is the people render.Mentions makes of text, by name, sorted.
func madeNames(text string) []string {
	scan := render.Mentions(text, mentionsDeclared, len(mentionsDeclared))
	var names []string
	for _, m := range scan.Made {
		names = append(names, m.Name)
	}
	slices.Sort(names)
	return names
}

// The cases that say what is code, for the two readers together: the people named, besides that they agree.
func TestMentionsAndTheAdapterReadTheSameCode(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"a fence at column 0", "```\nowner @sam failed\n```\nthen @lee please", []string{"lee"}},
		{"the issue's example, after a list marker", "- ```\n  owner @sam failed\n  ```\nthen @lee please", []string{"lee"}},
		{"after a number", "1. ```\n   owner @sam failed\n   ```\nthen @lee please", []string{"lee"}},
		{"a quote with a mark on every line", "> ```\n> owner @sam failed\n> ```\n> then @lee please", []string{"lee"}},
		{"a quote closed by a line with no mark", "> ```\n> owner @sam failed\n```\nthen @lee please", []string{"lee"}},
		{"a list with a fence in each item", "- ```\n  @sam\n  ```\n- @lee\n- ```\n  @kim\n  ```", []string{"lee"}},
		{"a marker with a second mark is prose", "- > ```\n@sam\n- - ```\n@lee", []string{"lee", "sam"}},
		{"a list marker in a block at column 0 is code", "```\n- ```\n@sam\n```\n@lee", []string{"lee"}},
		{"a quote mark in a block at column 0 is code", "```\n> ```\n@sam\n```\n@lee", []string{"lee"}},
		{"a block that never closes", "- ```\n  @sam\n  @lee", nil},
		{"a list item with a span", "- ``` @sam ```\n@lee", []string{"lee"}},
		{"a numbered item with a span, and a name after it", "1. ```make test``` first, then @sam", []string{"sam"}},
		{"a quote with a span, and a name after it", "> ```x``` y @sam", []string{"sam"}},
		{"a one-line block after tildes", "- ~~~ @sam ~~~\n@lee", []string{"lee"}},
		{"a second block after the first", "- ```\n  @sam\n  ```\n> ```\n> @kim\n> ```\n@lee", []string{"lee"}},
		{"a span", "see `@sam` and @lee", []string{"lee"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := markdown(tc.in, math.MaxInt)
			if got, made := proseNames(sent), madeNames(tc.in); !slices.Equal(got, made) || !slices.Equal(made, tc.want) {
				t.Fatalf("prose of %q is %v, Mentions made %v; want %v\nin  %q", sent, got, made, tc.want, tc.in)
			}
		})
	}
}

// Over every combination of an opener, the lines of a block and a closer, the adapter shows a name as prose if and only if
// render.Mentions makes a person of it. The two readers of the text differ in a place only if this fails there, and then the
// text that shows a name in code notifies someone, or the text that shows one as prose notifies nobody.
func TestMentionsAndTheAdapterAgreeOnWhatIsCode(t *testing.T) {
	openers := []string{ // what stands before the opener's run
		"", "  ", "\t", "- ", "* ", "+ ", "1. ", "12) ", "  - ", "-   ", " 1.  ", "> ", ">", "  > ", ">   ",
		"- - ", "- > ", "> > ", "> - ", "-", "1234567890. ", "1 ",
		"-\t", "-\u00a0", "1.\t", "1)\u00a0", ">\t", ">\u00a0", "- \t",
	}
	lines := []string{"", "  ", "   ", "> ", ">", "- ", "  - ", "  > ", "1. "} // what stands before a line of the block
	closers := []string{"", "  ", "    ", "> ", ">", "- ", "  - ", "1. ", "> > ", "> - ", "  > "}
	tails := []string{"", " tail @joe", " ```x``` @zed", " - ``` @joe\n```\n@zed", " > ``` @joe\n```"} // what follows a closing run
	conts := []string{"", "  ", "    ", "      "}                                                      // what stands before the line after the closer
	blanks := []string{"", "\n"}
	var n, bad int
	compare := func(in string) {
		n++
		sent := markdown(in, math.MaxInt)
		if got, made := proseNames(sent), madeNames(in); !slices.Equal(got, made) {
			if bad++; bad <= 10 {
				t.Errorf("the adapter shows %v as prose, Mentions made %v\nin   %q\nsent %q", got, made, in, sent)
			}
		}
	}
	for _, open := range openers {
		for _, run := range []string{"```", "~~~"} {
			for _, info := range []string{"", "bash"} {
				for _, l1 := range lines {
					for _, l2 := range lines {
						for _, cl := range closers {
							// The second block opens after the first, with a mark taken from the lists, and its closer
							// has a tail: both blocks of the text, and the prose between and after them, are compared.
							open2, cl2 := openers[n%len(openers)], closers[n/7%len(closers)]
							compare(fmt.Sprintf("before @ann\n%s%s%s\n%sowner @sam failed\n%scc @kim\n%s%s%s\n%s%sthen @lee please\n- @bob\n%s%s\nagain @joe\n%s%s\nlast @zed",
								open, run, info, l1, l2, cl, run, tails[n%len(tails)], blanks[n/3%len(blanks)], conts[n/5%len(conts)], open2, run, cl2, run))
						}
					}
				}
			}
		}
	}
	for _, open := range openers { // a block of one line, or a code span, on the line of the opener
		for _, run := range []string{"```", "~~~", "````"} {
			for _, inner := range []string{" @sam ", " x @sam ", "\u00a0@sam\u00a0", "", " - ``` @sam ", " > ``` @sam "} {
				for _, tail := range []string{"", " @lee", "\n@lee", "\n```\n@kim\n```\n@bob"} {
					compare("before @ann\n" + open + run + inner + run + tail + "\nthen @joe\n")
					compare("before @ann\n" + open + run + inner + run + "`" + tail + "\nthen @joe\n")
					compare("before @ann\n" + open + run + inner + "\n@zed")
				}
			}
		}
	}
	if bad > 10 {
		t.Errorf("and %d more of %d", bad-10, n)
	}
	if n < 1000 {
		t.Fatalf("only %d texts compared", n)
	}
}

// After the block of a list marker the item's lines lose the item's content column, so that none that is less than four columns
// past the item is indented four or more, which Slack reads as code after a blank line (row 23). The readers agree on the names
// besides. Four columns past the item are code to Slack all the same, as they are at the top level, and a name in them counts.
func TestAnItemsLinesAfterItsBlockAreNotIndentedCode(t *testing.T) {
	for _, open := range []string{"- ", "* ", "+ ", "1. ", "9) ", "10. ", "123) ", "  - ", "    - ", "-   ", " 12.  "} {
		col := len([]rune(open))
		for _, blank := range []string{"", "\n", "\n\n"} {
			for k := range 6 {
				in := open + "```\n" + strings.Repeat(" ", col) + "x\n" + strings.Repeat(" ", col) + "```\n" + blank + strings.Repeat(" ", col+k) + "then ask @lee"
				sent := markdown(in, math.MaxInt)
				want := "```\nx\n```\n" + blank + strings.Repeat(" ", k) + "then ask @lee"
				if sent != want {
					t.Errorf("%q: sent %q, want %q", in, sent, want)
				}
				if got, made := proseNames(sent), madeNames(in); !slices.Equal(got, made) || !slices.Equal(made, []string{"lee"}) {
					t.Errorf("%q: prose %v, Mentions made %v; want [lee]", in, got, made)
				}
			}
		}
	}
}
