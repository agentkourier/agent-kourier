package teams

import (
	"strings"
	"testing"
)

// The text of a Graph message's HTML body is what a person reads in it: the tags gone, a block a line of its own, the entities
// decoded, a mention as the activity's text writes it (<at>name</at>), and nothing of a script, a style or a comment. The first case
// is the body Graph gave on a test tenant for a mention whose activity's text was "<at>Agent Kourier</at> card".
func TestHTMLText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"measured mention", `<p><at id="0">Agent Kourier</at>&nbsp;card</p>`, "<at>Agent Kourier</at> card"},
		{"measured plain post", "<p>Poll test: a new top-level post</p>", "Poll test: a new top-level post"},
		{"measured card post", `<attachment id="91fe7b4401184414b97a9795e2e6abba"></attachment>`, ""},
		{"measured system event", "<systemEventMessage/>", ""},
		{"paragraphs", "<p>one</p><p>two</p>", "one\ntwo"},
		{"line breaks", "one<br>two<br/>three", "one\ntwo\nthree"},
		{"a list", "<ul><li>a</li><li>b</li></ul>", "a\nb"},
		{"divs and blank blocks", "<div>a</div><div></div><div></div><div>b</div>", "a\nb"},
		{"inline markup", `<p>a <b>bold</b>, <i>it</i> and <a href="https://x.test">a link</a></p>`, "a bold, it and a link"},
		{"entities", "&lt;at&gt;x&lt;/at&gt; &amp;amp; &#123;&#x7d; &quot;q&quot;", "<at>x</at> &amp; {} \"q\""},
		{"a script", "<p>a<script>alert('x')</script>b</p>", "ab"},
		{"a style", "<style>p{color:red}</style><p>a</p>", "a"},
		{"a comment", "a<!-- <p>hidden</p> -->b", "ab"},
		{"an unclosed comment", "a<!-- b", "a"},
		{"a quoted >", `<span title="a>b">x</span>`, "x"},
		{"a stray <", "a < b", "a < b"},
		{"an unclosed tag", "a <b", "a <b"},
		{"code keeps its lines", "<pre>line 1\n  line 2</pre>", "line 1\n  line 2"},
		{"trailing spaces", "<p>a   </p><p>  b</p>", "a\n  b"},
		{"uppercase tags", "<P>a</P><BR>b", "a\nb"},
		{"an at tag with other attributes", `<at id="1" data-x="y">Someone</at>`, "<at>Someone</at>"},
		{"a tag that only begins with at", `<attention>x</attention>`, "x"},
	} {
		if got := htmlText(tc.in); got != tc.want {
			t.Errorf("%s: htmlText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// A body of contentType text is taken as written; html is converted; any other type has no text.
func TestBodyText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ kind, content, want string }{
		{"text", "Reply3 &amp; <b>", "Reply3 &amp; <b>"},
		{"html", "<p>a &amp; b</p>", "a & b"},
		{"HTML", "<p>a</p>", "a"},
		{"", "<p>a</p>", "a"},
		{"other", "a", ""},
	} {
		if got := (graphBody{ContentType: tc.kind, Content: tc.content}).text(); got != tc.want {
			t.Errorf("%q %q: %q, want %q", tc.kind, tc.content, got, tc.want)
		}
	}
}

// The text of an Adaptive Card is its TextBlocks, its RichTextBlocks (their runs on one line) and its FactSets (a line "title: value"
// each), in the order of the card, through its containers, columns and tables, to cardMaxDepth.
func TestCardText(t *testing.T) {
	t.Parallel()
	// nested is a TextBlock inside n Containers: the card's body holds the outermost at depth 1, and the TextBlock is at n+1.
	nested := func(n int) string {
		s := `{"type":"TextBlock","text":"deep"}`
		for range n {
			s = `{"type":"Container","items":[` + s + `]}`
		}
		return `{"body":[` + s + `]}`
	}
	for _, tc := range []struct{ name, in, want string }{
		{"Workflows' alert card", `{"type":"AdaptiveCard","body":[{"type":"TextBlock","text":"Alert"},{"type":"FactSet","facts":[` +
			`{"title":"severity","value":"critical"},{"title":"","value":"no title"}]}]}`, "Alert\nseverity: critical\nno title"},
		{"containers and columns", `{"body":[{"type":"Container","items":[{"type":"TextBlock","text":"a"}]},{"type":"ColumnSet",` +
			`"columns":[{"type":"Column","items":[{"type":"TextBlock","text":"b"}]},{"type":"Column","items":[{"type":"TextBlock","text":"c"}]}]}]}`,
			"a\nb\nc"},
		{"a table", `{"body":[{"type":"Table","rows":[{"type":"TableRow","cells":[{"type":"TableCell","items":[{"type":"TextBlock","text":"x"}]}]}]}]}`, "x"},
		{"a rich text block", `{"body":[{"type":"RichTextBlock","inlines":["plain ",{"type":"TextRun","text":"run"}]}]}`, "plain run"},
		{"actions are not text", `{"body":[{"type":"TextBlock","text":"a"}],"actions":[{"type":"Action.OpenUrl","title":"Open"}]}`, "a"},
		{"blank text", `{"body":[{"type":"TextBlock","text":"  "},{"type":"TextBlock","text":"b"}]}`, "b"},
		{"text of another type", `{"body":[{"type":"TextBlock","text":7}]}`, ""},
		{"not JSON", `{"body":`, ""},
		{"too deep", nested(cardMaxDepth), ""},
		{"just deep enough", nested(cardMaxDepth - 1), "deep"},
	} {
		if got := cardText(tc.in); got != tc.want {
			t.Errorf("%s: cardText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Teams' message IDs are compared as the numbers they are, a post's creation in milliseconds.
func TestMessageIDs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1794965000000", "1794965000001", -1},
		{"1794965000001", "1794965000000", 1},
		{"1794965000000", "1794965000000", 0},
		{"999", "1000", -1},
		{"0999", "1000", -1},
		{"0001000", "1000", 0},
	} {
		if got := compareIDs(tc.a, tc.b); got != tc.want {
			t.Errorf("compareIDs(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	for id, ok := range map[string]bool{"1794965000000": true, "0": true, "": false, "1x": false, "-1": false, "1 2": false,
		strings.Repeat("1", maxActivityID): true, strings.Repeat("1", maxActivityID+1): false, "١٢": false} {
		if isMessageID(id) != ok {
			t.Errorf("isMessageID(%q) = %t", id, !ok)
		}
	}
}
