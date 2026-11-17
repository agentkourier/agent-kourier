package commonmark_test

import (
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/chat/commonmark"
)

// codeOf is the code a CommonMark reader finds in md, in order: each block's text and each span's, as goldmark reads them
// (chattest.ScanCommonMark), which is what a receiver shows as written. A block's last line break is not counted: goldmark
// keeps none for a last line that has none, and a receiver shows none.
func codeOf(md string) []string {
	var code []string
	for _, a := range chattest.ScanCommonMark(md).Active {
		if a.Kind == chattest.KindCode {
			code = append(code, strings.TrimSuffix(a.Text, "\n"))
		}
	}
	return code
}

// samples are the code that agents write, each as the agent wrote it: what a CommonMark reader finds in it is what the
// receiver must find in what Agent Kourier sends.
var samples = []struct {
	name string
	md   string
	want string // the text sent, where it is pinned; "" checks the code alone
}{
	{"kubectl table in a fence", "Pods:\n```\nNAME    READY   STATUS    NOMINATED NODE   READINESS GATES\nweb-0   1/1     Running   <none>           <none>\n```\nDone.",
		"Pods:\n```\nNAME    READY   STATUS    NOMINATED NODE   READINESS GATES\nweb-0   1/1     Running   <none>           <none>\n```\nDone."},
	{"kubectl in a span", "Run `kubectl get pods -l app=<name> -o jsonpath='{.items[*].metadata.name}'` first.",
		"Run `kubectl get pods -l app=<name> -o jsonpath='{.items[*].metadata.name}'` first."},
	{"log lines with brackets", "```log\n[INFO] 2026-11-16T10:00:00Z started [pid=42]\n[WARN] <nil> response from [::1]:8080\n```",
		"```log\n[INFO] 2026-11-16T10:00:00Z started [pid=42]\n[WARN] <nil> response from [::1]:8080\n```"},
	{"a log line in a span", "The error was `[ERROR] dial tcp: lookup <svc>: no such host`.", ""},
	{"Windows paths", "Open `C:\\Users\\ops\\config.yaml` or:\n```powershell\nGet-Content C:\\x\\[a].txt\n```", ""},
	{"a span of two backticks around a backtick", "Quote it as ``a ` b`` in the shell.", "Quote it as ``a ` b`` in the shell."},
	{"a span that starts with a backtick", "Type `` `date` `` to run it.", "Type `` `date` `` to run it."},
	{"a fence that holds a fence", "````md\n```go\nfmt.Println(\"<b>\")\n```\n````", "````md\n```go\nfmt.Println(\"<b>\")\n```\n````"},
	{"a tilde fence that holds backticks", "~~~\n```\n`x`\n```\n~~~", "````\n```\n`x`\n```\n````"},
	{"a fence of tildes and a longer close", "~~~sh\necho ok\n~~~~~", "```sh\necho ok\n```"},
	{"HTML in a fence", "```html\n<script>alert(1)</script>\n<a href=\"https://evil.test\">x</a>\n```", "```html\n<script>alert(1)</script>\n<a href=\"https://evil.test\">x</a>\n```"},
	{"HTML in a span", "Use `<img src=x onerror=alert(1)>` as the probe.", "Use `<img src=x onerror=alert(1)>` as the probe."},
	{"a markdown link in code", "```\n[x](https://evil.test) ![i](https://evil.test/i.png)\n```", ""},
	{"an indented block", "Output:\n\n    $ ls <dir>\n    [a] b\n\nDone.", "Output:\n\n```\n$ ls <dir>\n[a] b\n```\n\nDone."},
	{"an indented block with a blank line in it", "x\n\n    a\n\n    b\n\ny", "x\n\n```\na\n\nb\n```\n\ny"},
	{"an indented block of tabs", "x\n\n\tif a < b {\n\t\treturn [0]\n\t}", "x\n\n```\nif a < b {\n\treturn [0]\n}\n```"},
	{"an indented fence", "  ```\n  a <b>\n b\n  ```", "```\na <b>\nb\n```"},
	{"a fence in a list item", "1. Check:\n   ```bash\n   kubectl get pods <x>\n   ```\n2. Then [this].", ""},
	{"a fence on a list marker", "- ```sh\n  ls <dir>\n  ```", "-\n```sh\nls <dir>\n```"},
	{"a fence in a nested item", "- a\n  - b\n    ```\n    c <d>\n    ```\n  - e", ""},
	{"an indented block in a list item", "- item\n\n      code <x>\n\n  more", ""},
	{"a fence that is never closed", "Here:\n```\n[a] <b>\nend", "Here:\n```\n[a] <b>\nend\n```"},
	{"a fence that is never closed, with a line break", "```\nx\n", "```\nx\n```\n"},
	{"a fence of three backticks after code with four", "```\na ```` b\n```", "`````\na ```` b\n`````"},
	{"an info string that is not a word", "```{.python <x>}\nprint(1)\n```", "```\nprint(1)\n```"},
	{"an empty block, then a span", "```\n```\n`x`", "```\n```\n`x`"},
	{"an indented block after a setext heading", "x\n===\n    <b>", "x\n===\n```\n<b>\n```"},
	{"an indented block after a rule", "***\n    <b>", "***\n```\n<b>\n```"},
	{"an indented block after a rule of spaced dashes", "a\n\n- - -\n    <b>", "a\n\n- - -\n```\n<b>\n```"},
	{"an indented block on a list marker", "-     <img src=x>\n\n      [a]\n  b", "-\n```\n<img src=x>\n\n[a]\n```\nb"},
	{"an indented block after an empty item", "- \n\n    <b>", "- \n\n```\n<b>\n```"},
	{"an indented block after an empty ordered item", "1.\n\n    <b>\n2. x", "1.\n\n```\n<b>\n```\n2. x"},
	{"an empty item that goes on", "-\n  a\n\n      <b>", ""},
}

// The escapes of prose are sent as pinned here: a reader's view alone cannot tell "~~~" from "\~\~\~", which a receiver may read
// as a fence.
func TestProseEscapesAreSent(t *testing.T) {
	for _, tc := range []struct{ md, want string }{
		{"a ~~~ b <c>", `a \~\~\~ b &lt;c>`},
		{"a ~~ b", "a ~~ b"},
		{"it's a ` mark", "it's a \\` mark"},
		{`a\b [x]`, `a\\b \[x\]`},
	} {
		if got := commonmark.Inert(tc.md, math.MaxInt); got != tc.want {
			t.Errorf("%q: sent %q, want %q", tc.md, got, tc.want)
		}
	}
}

// A cut in prose drops the white space at the end of its line: a line of white space alone, four columns or more, would be code
// to a receiver once the marker follows it.
func TestACutDropsTheWhiteSpaceBeforeTheMarker(t *testing.T) {
	md := "    \n" + strings.Repeat("x", 50)
	if got := commonmark.Inert(md, 17); got != chat.Truncated {
		t.Errorf("sent %q, want the marker alone", got)
	}
	doc := "p\n\n    \n\t\n        \nq <b> r\n\n  - a\n\n      \n" + strings.Repeat("y", 30)
	whole := commonmark.Inert(doc, math.MaxInt)
	for limit := chat.Units(chat.Truncated) + 1; limit < chat.Units(whole); limit++ {
		got := commonmark.Inert(doc, limit)
		for _, c := range codeOf(got) {
			if strings.Contains(c, "truncated") {
				t.Fatalf("limit %d: the marker is in code: %q", limit, got)
			}
		}
	}
}

// Text in code spans and in fenced and indented blocks reaches the receiver as the agent wrote it, and a fence is one that
// the code's own backticks or tildes cannot close.
func TestCodeReachesTheReceiverAsWritten(t *testing.T) {
	for _, tc := range samples {
		t.Run(tc.name, func(t *testing.T) {
			got := commonmark.Inert(tc.md, math.MaxInt)
			if tc.want != "" && got != tc.want {
				t.Errorf("sent\n%s\nwant\n%s", got, tc.want)
			}
			if want, sent := codeOf(tc.md), codeOf(got); !slices.Equal(sent, want) {
				t.Errorf("the receiver finds the code\n%q\nin\n%s\nwant\n%q", sent, got, want)
			}
		})
	}
}

// Every sample holds code: a check that finds none checks nothing.
func TestEverySampleHoldsCode(t *testing.T) {
	for _, tc := range samples {
		if len(codeOf(tc.md)) == 0 {
			t.Errorf("%s: no code in %q", tc.name, tc.md)
		}
	}
}

// A block in a list item is written at column 0, which ends the list; the item's lines after it are text, and the next item
// starts a list again, at its own number.
func TestAListGoesOnAfterABlock(t *testing.T) {
	md := "1. Check:\n   ```bash\n   kubectl get pods <x>\n   ```\n   Then look.\n2. Then [this].\n   - a\n\n         code\n     b"
	want := "1. Check:\n```bash\nkubectl get pods <x>\n```\nThen look.\n\n2. Then \\[this\\].\n   - a\n\n```\ncode\n```\nb"
	got := commonmark.Inert(md, math.MaxInt)
	if got != want {
		t.Errorf("sent\n%s\nwant\n%s", got, want)
	}
	items := 0
	for _, a := range chattest.ScanCommonMark(got).Active {
		if a.Kind == chattest.KindList {
			items++
		}
	}
	if items != 3 {
		t.Errorf("the receiver reads %d list items in\n%s\nwant 3", items, got)
	}
}

// A span over two lines, which CommonMark reads as code, is prose: Agent Kourier reads a span within its line. The receiver shows
// the characters, backticks included, and nothing acts.
func TestASpanOverTwoLinesIsProse(t *testing.T) {
	if got, want := commonmark.Inert("a `b\nc <d>` e", math.MaxInt), "a \\`b\nc &lt;d>\\` e"; got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}

// Outside code nothing links, mentions or is HTML, whatever looks like code there, and the receiver shows the characters the
// agent wrote.
func TestProseIsDefused(t *testing.T) {
	for _, tc := range []struct{ name, md, shown string }{
		{"link", "[bank](https://evil.test/login)", ""},
		{"image", "![t](https://evil.test/p.png)", ""},
		{"reference link", "[x][1]\n\n[1]: https://evil.test", "[x][1]\n\n[1]: https://evil.test"},
		{"angle autolink", "<https://evil.test>", ""},
		{"HTML", "<b>hi</b> <img src=x onerror=alert(1)>", ""},
		{"HTML block", "<div>\n<script>alert(1)</script>\n</div>", ""},
		{"Slack tokens", "<!channel> <@U0123456789> <#C0123456789>", ""},
		{"a lone backtick", "it's a ` mark <b>", "it's a ` mark <b>"},
		{"backticks that close nothing", "``a` <b>", "``a` <b>"},
		{"a span with a backslash before it", "\\`<b>`", "\\<b>"},
		{"a fence in a quote", "> ```\n> <b>x</b>\n> ```", ""},
		{"tildes", "a ~~~ b <c>", "a ~~~ b <c>"},
		{"a backtick fence with a backtick in its info", "``` a ` <b>\nx", ""},
		{"backslashes", `a\b \[x\] \\`, `a\b \[x\] \\`},
		{"a span in a table with a pipe", "| a | b |\n|---|---|\n| `x|<b>y</b>` | z |", ""},
		{"a delimiter row after the span", "`x|<b>y</b>` | z\n--- | ---", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := commonmark.Inert(tc.md, math.MaxInt)
			shown := chattest.ScanCommonMarkLiteralCode(got)
			for _, a := range shown.Active {
				if !a.Kind.Style() && a.Kind != chattest.KindAutolink { // a bare URL: a receiver may link it, and nothing stops that
					t.Errorf("%q became a %s: %q, in\n%s", tc.md, a.Kind, a.Text, got)
				}
			}
			if tc.shown != "" && shown.Displayed != tc.shown {
				t.Errorf("shown %q, want %q (sent %q)", shown.Displayed, tc.shown, got)
			}
		})
	}
}

// In a GFM table, whose cells are split at every pipe before code is read, a span with a pipe in it is not code but prose: what
// follows the pipe would otherwise be read raw. A span with no pipe, and a span with one outside a table, stay code.
func TestASpanWithAPipeIsCodeOutsideATable(t *testing.T) {
	for _, tc := range []struct{ md, want string }{
		{"| a |\n|---|\n| `x|y` |", "| a |\n|---|\n| \\`x|y\\` |"},
		{"| a |\n|---|\n| `x y` |", "| a |\n|---|\n| `x y` |"},
		{"Run `ps | grep <x>`.", "Run `ps | grep <x>`."},
		{"| a |\n|---|\n\nRun `ps | grep x`.", "| a |\n|---|\n\nRun `ps | grep x`."},
	} {
		if got := commonmark.Inert(tc.md, math.MaxInt); got != tc.want {
			t.Errorf("%q: sent %q, want %q", tc.md, got, tc.want)
		}
	}
}

// A bare URL that a receiver links runs on to white space or "<" (GitHub's flavour, as Agent Kourier understands it, not
// measured here): it takes in what follows it, a backslash or a span's backticks. So "<" after it is an entity, and a span right
// after a word that may be linked is prose; a span after white space is code.
func TestNoURLRunsIntoMarkup(t *testing.T) {
	for _, tc := range []struct{ md, want string }{
		{"http://e.test<b>", "http://e.test&lt;b>"},
		{"see http://e.test`<b>`", "see http://e.test\\`&lt;b>\\`"},
		{"mail a@e.test`<b>`", "mail a@e.test\\`&lt;b>\\`"},
		{"WWW.e.test`<b>`", "WWW.e.test\\`&lt;b>\\`"},
		{"see http://e.test `<b>`", "see http://e.test `<b>`"},
		{"path/to/file`<b>`", "path/to/file`<b>`"},
	} {
		if got := commonmark.Inert(tc.md, math.MaxInt); got != tc.want {
			t.Errorf("%q: sent %q, want %q", tc.md, got, tc.want)
		}
	}
}

// A text that fits is not cut, and is cut at a limit one short of it.
func TestATextThatFitsIsNotCut(t *testing.T) {
	for _, tc := range samples {
		whole := commonmark.Inert(tc.md, math.MaxInt)
		n := chat.Units(whole)
		if got := commonmark.Inert(tc.md, n); got != whole {
			t.Errorf("%s: at a limit of its own %d units it was sent as %q", tc.name, n, got)
		}
		if n-1 > chat.Units(chat.Truncated) {
			if got := commonmark.Inert(tc.md, n-1); !strings.HasSuffix(got, chat.Truncated) {
				t.Errorf("%s: one unit short it was sent as %q, with no marker", tc.name, got)
			}
		}
	}
}

// What is counted is the text as it is sent: escapes and fences included.
func TestTheLimitCountsTheTextAsSent(t *testing.T) {
	got := commonmark.Inert(strings.Repeat("[", 100), 100)
	if n := chat.Units(got); n > 100 || n < 99 || !strings.HasSuffix(got, chat.Truncated) {
		t.Errorf("100 brackets at a limit of 100 sent %d units: %q", n, got)
	}
}

// A cut never leaves a block open: a cut in code closes the block with its own fence, and the marker follows it on a line of
// its own, as prose; a span is sent whole or not at all; and what is sent of the code is the start of it.
func TestACutLandsOutsideCode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		md    string
		limit int
		want  string
	}{
		{"in a fenced block", "a\n```\n0123456789abcdefghij\n```\nb", 25, "a\n```\n0\n```\n" + chat.Truncated},
		{"after a line break in a block", "a\n```\n0\n1\n2\n3\n4\n5\n6\n7\n8\n9\n```", 25, "a\n```\n0\n```\n" + chat.Truncated},
		{"right after the opener", "a\n```\n0123456789abcdef\n```", 23, "a\n```\n```\n" + chat.Truncated},
		{"before the opener", "abc\n```\n0123456789\n```", 18, "abc\n" + chat.Truncated},
		{"in an indented block", "x\n\n    0123456789abcdefghij", 26, "x\n\n```\n0\n```\n" + chat.Truncated},
		{"in a fence longer than the code's", "````\n```\n0123456789abcdefghij\n````", 29, "````\n```\n0\n````\n" + chat.Truncated},
		{"right after the block", "```\n0\n```\nabcdefghijklmnop", 24, "```\n0\n```\na" + chat.Truncated},
		{"a span that does not fit", "ab `0123456789abcdef` cd", 20, "ab" + chat.Truncated},
		{"in prose, between escapes", "[[[[[[[[[[", 16, `\[` + chat.Truncated},
		{"a limit too small for the marker", "```\n0123456789\n0123456789\n```", 5, string([]rune(chat.Truncated)[:5])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := commonmark.Inert(tc.md, tc.limit)
			if got != tc.want {
				t.Errorf("sent %q, want %q", got, tc.want)
			}
			if n := chat.Units(got); n > tc.limit {
				t.Errorf("sent %d units, over the limit of %d", n, tc.limit)
			}
		})
	}
}

// At every limit, a cut text is within it, ends in the marker outside code, makes nothing active, and holds the start of the
// code and nothing else.
func TestEveryCutLandsOutsideCode(t *testing.T) {
	doc := "Pods <x>:\n```\nNAME  READY\nweb-0 1/1 <none>\n```\nRun `kubectl logs web-0` and see [x].\n\n" +
		"1. Then:\n   ~~~\n   [INFO] C:\\x\n   ~~~\n2. And `` a`b `` too.\n\n    indented <b>\n    more\n\nEnd."
	whole := commonmark.Inert(doc, math.MaxInt)
	code := codeOf(whole)
	for limit := chat.Units(chat.Truncated) + 1; limit < chat.Units(whole); limit++ {
		got := commonmark.Inert(doc, limit)
		if n := chat.Units(got); n > limit || !strings.HasSuffix(got, chat.Truncated) {
			t.Fatalf("limit %d: %d units, want at most the limit and the marker at the end: %q", limit, n, got)
		}
		shown := chattest.ScanCommonMarkLiteralCode(got)
		if !strings.Contains(shown.Displayed, chat.Truncated) {
			t.Fatalf("limit %d: the marker is not shown: %q", limit, got)
		}
		cut := codeOf(got)
		for i, c := range cut {
			if strings.Contains(c, "truncated") {
				t.Fatalf("limit %d: the marker is in code: %q", limit, got)
			}
			if i >= len(code) || !strings.HasPrefix(code[i], strings.TrimSuffix(c, "\n")) {
				t.Fatalf("limit %d: code %q is not the start of the code sent whole, %q", limit, c, code)
			}
		}
		for _, a := range shown.Active {
			if !a.Kind.Style() {
				t.Fatalf("limit %d: a %s %q is active in %q", limit, a.Kind, a.Text, got)
			}
		}
	}
}

// fragments are pieces of markup that open code, containers, links and HTML, which texts made of them put in every order. A Slack
// token ("<!", "<@", "<#") is not among them: code holds it as the agent wrote it, and the scanner reports it there.
var fragments = []string{
	"```", "~~~", "`", "``", "    ", "  ", "\t", "- ", "1. ", "2) ", "> ", "# ", "\n", "\n\n", "a", "\\", "|", "|---|",
	"<b>", "</b>", "[x](http://e.test)", "![i](http://e.test/i)", "[x]: http://e.test", "<http://e.test>", "http://e.test",
	"www.e.test", "a@e.test", "`<b>`",
}

// Whatever the agent's markup does to the code that Agent Kourier reads, a receiver finds nothing active in what is sent but style
// and a bare URL, whole or cut: every text of three fragments, and texts of eight chosen at random (with a fixed seed).
func TestNoMarkupMakesAnythingActive(t *testing.T) {
	check := func(md string) {
		t.Helper()
		whole := commonmark.Inert(md, math.MaxInt)
		for _, got := range []string{whole, commonmark.Inert(md, max(chat.Units(whole)/2, chat.Units(chat.Truncated)+1))} {
			for _, a := range chattest.ScanCommonMarkLiteralCode(got).Active {
				if !a.Kind.Style() && a.Kind != chattest.KindAutolink {
					t.Fatalf("%q was sent as %q, in which a %s %q is active", md, got, a.Kind, a.Text)
				}
			}
		}
	}
	for _, a := range fragments {
		for _, b := range fragments {
			for _, c := range fragments {
				check(a + b + c)
			}
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 3000 {
		var b strings.Builder
		for range 8 {
			b.WriteString(fragments[rng.IntN(len(fragments))])
		}
		check(b.String())
	}
}

// The text is cleaned first: what no one sees is not sent.
func TestTheTextIsCleaned(t *testing.T) {
	if got, want := commonmark.Inert("a\u200b<\u200b!channel>\r\n```\nb\u202e\n```", math.MaxInt), "a&lt;!channel>\n```\nb\n```"; got != want {
		t.Errorf("sent %q, want %q", got, want)
	}
}
