package slack

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// The rows are those of the table in docs/spikes/slack-code-rendering.md: Slack's behaviour as measured in the probe of
// the sandbox workspace, 2026-10-04. Each case is what the agent wrote and the exact text Agent Kourier sends.
//
// Raw strings are used for the text of cases that hold backslashes.
var markdownCases = []struct {
	name, in, want string
}{
	// Code is raw.
	{"01 a fence keeps backslashes and entities", "```\na\\[0\\] &amp; &lt;c&gt; \\\\n\n```", "```\na\\[0\\] &amp; &lt;c&gt; \\\\n\n```"},
	{"02 a fence keeps brackets", "```\nmodel = []\nprintf '\\n'\n```", "```\nmodel = []\nprintf '\\n'\n```"},
	{"03 a span keeps brackets and angle brackets", "`a[0] <b>`", "`a[0] <b>`"},
	{"21 a span keeps 2>&1", "run `cmd 2>&1` now", "run `cmd 2>&1` now"},
	{"26 a span keeps a doubled backslash", `use ` + "`a\\\\b`", `use ` + "`a\\\\b`"},
	{"54 a span keeps what Slack drops a backslash from", "`a\\[0] b\\\\c [d]`", "`a\\[0] b\\\\c [d]`"},
	{"04 a span keeps a link", "`[t](https://example.com)`", "`[t](https://example.com)`"},
	{"19 a fence keeps what looks like a link and a user", "```\na & b <c> 2>&1 <https://example.com|masked>\n```", "```\na & b <c> 2>&1 <https://example.com|masked>\n```"},
	{"19 a fence writes no broadcast, mention or channel token", "```\n<@U1> <!here> <#C1> <U1>\n```", "```\n&lt;@U1> &lt;!here> &lt;#C1> <U1>\n```"},
	{"20 a span keeps what looks like a link", "`a & b <c> 2>&1 <https://example.com|masked>`", "`a & b <c> 2>&1 <https://example.com|masked>`"},
	{"20 a span writes no broadcast, mention or channel token", "`<@U1>` `<!channel>` `<#C1>`", "`&lt;@U1>` `&lt;!channel>` `&lt;#C1>`"},
	{"52 a span may end with a backslash", "`a\\` b", "`a\\` b"},

	// Prose is escaped.
	{"05 prose link", "[t](https://example.com)", `\[t\](https://example.com)`},
	{"05 prose image", "![t](https://example.com/a.png)", `!\[t\](https://example.com/a.png)`},
	{"18 prose entities", "& <c> 2>&1", "&amp; &lt;c&gt; 2&gt;&amp;1"},
	{"27 prose user and masked link", "<@U1> <https://example.com|masked>", "&lt;@U1&gt; &lt;https://example.com|masked&gt;"},
	{"51 a backslash is escaped before a span", "\\\\ `x`", "\\\\\\\\ `x`"},

	// A fence in a list item is a block at column 0.
	{"07 a fence in a list item", "- item:\n  ```\n  x\n  ```\n- next", "- item:\n```\nx\n```\n- next"},
	{"25 a fence in a numbered item", "1. step:\n   ```bash\n   x\n     y\n   ```\n2. next", "1. step:\n```\nx\n  y\n```\n2. next"},
	{"22 a list, a fence at column 0, a list", "1. a\n```\nx\n```\n2. b", "1. a\n```\nx\n```\n2. b"},
	{"39 a fence on the marker line is prose", "- ```\n  x", "- \\`\\`\\`\n  x"},
	{"08 a fence indented by 1", " ```\nx\n ```", "```\nx\n```"},
	{"09 a fence indented by 3", "   ```\nx\n   ```", "```\nx\n```"},
	{"09 a fence indented by 4 or a tab", "    ```\n    x\n    ```\n\t```\n\tx\n\t```", "```\nx\n```\n```\nx\n```"},
	{"10 the info string is dropped", "```bash\nx\n```", "```\nx\n```"},
	{"10 so is a longer one when it is a word", "````yaml\nx\n````", "```\nx\n```"},
	{"11 a fence never closed stays open", "a\n```\nx", "a\n```\nx"},
	{"13 a tilde fence is written as a backtick one", "~~~\nx\n~~~", "```\nx\n```"},
	{"13 a fence opened by tildes is closed by backticks", "~~~\nx\n```\ny", "```\nx\n```\ny"},
	{"23 an indented block is prose", "a\n\n    x <y>", "a\n\n    x &lt;y&gt;"},
	{"24 a fence in a quote is prose", "> ```\n> x\n> ```", "&gt; \\`\\`\\`\n&gt; x\n&gt; \\`\\`\\`"},
	{"30 a fence after a quote line", "> q\n```\nx\n```", "&gt; q\n```\nx\n```"},
	{"31 a fence after an unclosed span", "a ` b\n```\nx\n```", "a \\` b\n```\nx\n```"},

	// What is inside a block is not a closer, but for a line that starts with a fence.
	{"32 a fence mid-line in a block", "```\nfoo ``` bar\n```", "```\nfoo ``` bar\n```"},
	{"33 a quoted fence in a block", "```\n> ```\n```", "```\n> ```\n```"},
	{"36 a list marker before a fence in a block", "```\n- ```\n1. ```\n```", "```\n- ```\n1. ```\n```"},
	{"40 a tab-indented fence closes the block, where Slack would not", "```\nx\n\t```\ny", "```\nx\n```\ny"},
	{"41 so does a 4-space-indented one", "```\nx\n    ```\ny", "```\nx\n```\ny"},
	{"44 a fence with a tail closes the block, and the tail is prose", "```\nx\n``` tail <b>\ny", "```\nx\n```\ntail &lt;b&gt;\ny"},
	{"a closing run longer than the opener", "```\nx\n``````\ny", "```\nx\n```\ny"},
	{"a block closed by the end of the text", "```\nx\n```", "```\nx\n```"},
	{"a block closed, and the line break after it kept", "```\nx\n```\n", "```\nx\n```\n"},
	{"an empty block", "```\n```", "```\n```"},
	{"a lone fence at the end opens an empty block", "a\n```", "a\n```"},

	// A one-line block, and a first line on the fence line.
	{"a one-line block", "```echo hi```", "```\necho hi\n```"},
	{"a one-line block with a tail", "```echo hi``` and then", "```\necho hi\n```\nand then"},
	{"a one-line block can't be closed twice", "````a ``` b````", "```\na ``` b\n```"},
	{"a one-line block whose inner text is a fence is a closing line, and the rest is read again", "````` ``` `````\nx", "```\n```\n```\nx"},
	{"a first line after the fence", "```kubectl get pods\nx\n```", "```\nkubectl get pods\nx\n```"},
	{"an info string may be anything without white space", "```` ```\nx", "```\nx"},
	{"a first line that is a fence closes the block", "```` ``` x\ny", "```\n```\nx\ny"},

	// Backticks and tildes in prose.
	{"14 a backtick never closed", "a ` b [t](u)", "a \\` b \\[t\\](u)"},
	{"15 a span across a line break is two stray backticks", "`a\nb`", "\\`a\nb\\`"},
	{"16 a double-backtick span", "``x``", "`x`"},
	{"17 a triple-backtick span mid-line", "see ```x``` ok", "see `x` ok"},
	{"a span with a backtick in it is no span", "``a`b``", "\\`\\`a\\`b\\`\\`"},
	{"34 backticks mid-line", "a ``` b", "a \\`\\`\\` b"},
	{"35 tildes mid-line", "a ~~~ b", "a \\~\\~\\~ b"},
	{"42 tildes in a quote", "> ~~~", "&gt; \\~\\~\\~"},
	{"43 backticks in a quote", "> ```", "&gt; \\`\\`\\`"},
	{"tildes of two are strikethrough", "~~x~~ and ~y~", "~~x~~ and ~y~"},
	{"50 a stray run, then a span", "a `` b `[t](url) <@U1> 2>&1 <pod>`", "a \\`\\` b `[t](url) &lt;@U1> 2>&1 <pod>`"},
	{"two spans in a line", "`a` and `b`", "`a` and `b`"},
	{"two backtick runs side by side are one run", "`a``b`", "\\`a\\`\\`b\\`"},

	// A pipe table is a block, its columns aligned.
	{"a table", "| a | b |\n|---|---|\n| 1 | 2 |", "```\n| a   | b   |\n|-----|-----|\n| 1   | 2   |\n```"},
	{"a table and the line break after it", "| a | b |\n|---|---|\n| 1 | 2 |\n", "```\n| a   | b   |\n|-----|-----|\n| 1   | 2   |\n```\n"},
	{"a table ends at a blank line", "| a |\n|---|\n| 1 |\n\nafter <b>", "```\n| a   |\n|-----|\n| 1   |\n```\n\nafter &lt;b&gt;"},
	{"a table ends at a line that is not a row", "| a |\n|---|\n| 1 |\nafter", "```\n| a   |\n|-----|\n| 1   |\n```\nafter"},
	{"a table with no rows", "| head | x |\n| --- | --- |", "```\n| head | x   |\n|------|-----|\n```"},
	{"a table's spans lose their backticks", "| Pod | Node |\n|---|---|\n| `web-1` | ``a b`` and ``c`d`` |", "```\n| Pod   | Node            |\n|-------|-----------------|\n| web-1 | a b and ``c`d`` |\n```"},
	{"a table's escaped pipe is a pipe in its cell", "| a | b |\n|---|---|\n| x \\| y | 2 |", "```\n| a     | b   |\n|-------|-----|\n| x | y | 2   |\n```"},
	{"a table's alignment is kept", "| name | n | mid |\n|:--|--:|:-:|\n| a | 1 | b |\n| bb | 100 | ccccc |", "```\n| name |   n |  mid  |\n|------|-----|-------|\n| a    |   1 |   b   |\n| bb   | 100 | ccccc |\n```"},
	{"a table's short row is padded and its long row kept whole", "| a | b |\n|---|---|\n| 1 |\n| 1 | 2 | 3 |", "```\n| a   | b   |     |\n|-----|-----|-----|\n| 1   |     |     |\n| 1   | 2   | 3   |\n```"},
	{"a table's code is raw but for tokens", "| a |\n|---|\n| <@U1> <!here> [t](u) & 2>&1 |", "```\n| a                                 |\n|-----------------------------------|\n| &lt;@U1> &lt;!here> [t](u) & 2>&1 |\n```"},
	{"a table's fence in a cell is text", "| ``` |\n|---|\n| ~~~ |", "```\n| ``` |\n|-----|\n| ~~~ |\n```"},
	{"a table in a list item is a block at column 0", "- see:\n  | a |\n  |---|\n  | 1 |\n- next", "- see:\n```\n| a   |\n|-----|\n| 1   |\n```\n- next"},
	{"a table too wide for a line is a record a row", "| Pod | Namespace | Request |\n|---|---|---|\n| kube-apiserver-proxmox-quickstart-control-plane-2zfxq | kube-system | 250m |\n| `sre-agent-54b48c9c66-tg829` | kagent | 1500m |",
		"```\nPod:       kube-apiserver-proxmox-quickstart-control-plane-2zfxq\nNamespace: kube-system\nRequest:   250m\n\nPod:       sre-agent-54b48c9c66-tg829\nNamespace: kagent\nRequest:   1500m\n```"},
	{"a header and a delimiter of different widths is no table", "| a | b |\n|---|", "| a | b |\n|---|"},
	{"a header and no delimiter is no table", "| a | b |\nnot | a delimiter", "| a | b |\nnot | a delimiter"},
	{"a delimiter with no pipe is a setext heading", "| a\n---", "| a\n---"},
	{"a table needs its leading pipe", "a | b\n|---|---|\n1 | 2", "a | b\n|---|---|\n1 | 2"},
	{"a lone row is prose", "| a & b |", "| a &amp; b |"},
	{"the table of 2026-10-18",
		"**Highest CPU requests on the busiest worker (`gq82h`)**\n\n| Pod | Request | Limit |\n|---|---:|---:|\n| `metrics-server-7d9c` | 100m | — |\n| `cilium-x2b4k` | 100m | — |\n| `sre-agent-54b48c9c66-tg829` | 500m | 1500m |\n\nTo get actual usage, run:",
		"**Highest CPU requests on the busiest worker (`gq82h`)**\n\n```\n| Pod                        | Request | Limit |\n|----------------------------|---------|-------|\n| metrics-server-7d9c        |    100m |     — |\n| cilium-x2b4k               |    100m |     — |\n| sre-agent-54b48c9c66-tg829 |    500m | 1500m |\n```\n\nTo get actual usage, run:"},

	// The trial of 2026-10-04.
	{"trial: the OOMKilled answer's list item",
		"2. *Update the Deployment* to fix the limits, if the code is legitimate:\n   ```bash\n   kubectl set resources deployment/recommender -n shop --limits=memory=512Mi\n   # or edit the deployment YAML and fix the container command\n   kubectl edit deployment recommender -n shop\n   ```",
		"2. *Update the Deployment* to fix the limits, if the code is legitimate:\n```\nkubectl set resources deployment/recommender -n shop --limits=memory=512Mi\n# or edit the deployment YAML and fix the container command\nkubectl edit deployment recommender -n shop\n```"},
	{"trial: the NotReady answer's JSON patch in a block",
		"Patch the probe:\n```\nkubectl patch deployment search -n shop --type='json' -p='[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/readinessProbe/httpGet/path\",\"value\":\"/\"}]'\n```",
		"Patch the probe:\n```\nkubectl patch deployment search -n shop --type='json' -p='[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/readinessProbe/httpGet/path\",\"value\":\"/\"}]'\n```"},
	{"trial: the same patch in a span",
		"run `kubectl patch deployment search -n shop --type='json' -p='[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/readinessProbe/httpGet/path\",\"value\":\"/\"}]'` now",
		"run `kubectl patch deployment search -n shop --type='json' -p='[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/readinessProbe/httpGet/path\",\"value\":\"/\"}]'` now"},
	{"trial: model = []", "```\nmodel = []\n```", "```\nmodel = []\n```"},
	{"live test of the docs agent: a yaml fence after a blank line, with prose on the line after the closer",
		"*Declare the backend* in your configuration:\n\n```yaml\nconfig:\n  agentBackends:\n    my-agent:\n      spec:\n        dialect: a2a\n        allowedNamespaces: [payments]\n```\n*Choose how the Binding identifies itself* via:\n",
		"*Declare the backend* in your configuration:\n\n```\nconfig:\n  agentBackends:\n    my-agent:\n      spec:\n        dialect: a2a\n        allowedNamespaces: [payments]\n```\n*Choose how the Binding identifies itself* via:\n"},
	{"trial: kubectl logs in a span", "Check `kubectl logs <pod> -n shop` and add `2>&1`.", "Check `kubectl logs <pod> -n shop` and add `2>&1`."},
}

func TestMarkdownIsWhatSlackIsSent(t *testing.T) {
	for _, tc := range markdownCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := markdown(tc.in, math.MaxInt); got != tc.want {
				t.Fatalf("markdown(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			if bad := slackReading(tc.want); len(bad) > 0 {
				t.Fatalf("the expectation itself breaks the invariant: %v", bad)
			}
		})
	}
}

// A stream is one document: the same text in any pieces comes out as the same text, a piece at a time or all at once.
func TestMarkdownInPiecesIsMarkdownAtOnce(t *testing.T) {
	for _, tc := range markdownCases {
		t.Run(tc.name, func(t *testing.T) {
			runes := []rune(tc.in)
			for at := 0; at <= len(runes); at++ { // two pieces, cut anywhere (row 12: even inside the closing run)
				var m mdText
				got := m.text(string(runes[:at]), math.MaxInt, false) + m.text(string(runes[at:]), math.MaxInt, true)
				if got != tc.want {
					t.Fatalf("cut at %d: %q\nwant %q", at, got, tc.want)
				}
			}
			var m mdText // and a rune at a time
			var got strings.Builder
			for _, r := range runes {
				got.WriteString(m.text(string(r), math.MaxInt, false))
			}
			got.WriteString(m.text("", math.MaxInt, true))
			if got.String() != tc.want {
				t.Fatalf("a rune at a time: %q\nwant %q", got.String(), tc.want)
			}
		})
	}
}

// What a stream shows while it is written: prose at once, and a line late where the line says what it is.
func TestMarkdownStreamHoldsBackOnlyWhatIsUndecided(t *testing.T) {
	var m mdText
	steps := []struct {
		in, want string
		final    bool
	}{
		{"Hello wor", "Hello wor", false},
		{"ld\nsee `x", "ld\nsee ", false}, // a span needs its closing run
		{"` and `y` now\n", "`x` and `y` now\n", false},
		{"``", "", false}, // a fence may be coming
		{"`bash\nx", "```\nx", false},
		{"<", "", false}, // so may a token, in code
		{"!channel>\n  ", "&lt;!channel>\n", false},
		{"``", "", false}, // a closer, which is a line that is not known until its end
		{"`\ndone ~", "```\ndone ", false},
		{"~", "", false},
		{"~ and ~~", "\\~\\~\\~ and ", false}, // a run of tildes, until it ends
		{"x", "~~x", false},
		{"`", "\\`", true}, // the end of the text writes what is held
	}
	for i, st := range steps {
		if got := m.text(st.in, math.MaxInt, st.final); got != st.want {
			t.Fatalf("step %d, %q: got %q, want %q", i, st.in, got, st.want)
		}
	}
	if m.held != "" {
		t.Fatalf("held %q at the end", m.held)
	}
}

// A table is held back until it ends, since its last row may widen a column.
func TestMarkdownStreamHoldsBackATableUntilItEnds(t *testing.T) {
	var m mdText
	steps := []struct {
		in, want string
		final    bool
	}{
		{"Rows:\n", "Rows:\n", false},
		{"| a |", "", false}, // a header, until the next line says
		{"\n|--", "", false}, // what may be a delimiter
		{"-|\n| 1 |\n", "", false},
		{"| 22222 |\n  ", "", false}, // a row may follow
		{"x", "```\n| a     |\n|-------|\n| 1     |\n| 22222 |\n```\n  x", false},
		{"\n| a |\nb", "\n| a |\nb", true}, // a header without a delimiter is prose
	}
	for i, st := range steps {
		if got := m.text(st.in, math.MaxInt, st.final); got != st.want {
			t.Fatalf("step %d, %q: got %q, want %q", i, st.in, got, st.want)
		}
	}
	if m.held != "" {
		t.Fatalf("held %q at the end", m.held)
	}
}

func TestMarkdownBlindEscapesEverything(t *testing.T) {
	m := mdText{blind: true}
	in := "```bash\nmodel = [] <!here>\n```\nsee `a<b>`"
	want := "```bash\nmodel = \\[\\] &lt;!here&gt;\n```\nsee `a&lt;b&gt;`"
	if got := m.text(in, math.MaxInt, false); got != want {
		t.Fatalf("blind: got %q, want %q", got, want)
	}
	if got := m.text("```", math.MaxInt, true); got != "```" {
		t.Fatalf("blind: later text read as %q", got)
	}
}

// A cut leaves the state as what was sent, so what follows is read as Slack reads it.
func TestMarkdownCutLeavesTheStateAsSent(t *testing.T) {
	marker := chat.Units(chat.Truncated)
	for _, tc := range []struct {
		name, in string
		room     int // the units that fit besides the marker
		want     string
		code     bool
	}{
		{"in prose", "abcdefgh", 3, "abc", false},
		{"before a span, which is whole or not at all", "ab `cdef` gh", 4, "ab ", false},
		{"after a span", "ab `c` gh", 6, "ab `c`", false},
		{"before a run of tildes", "ab ~~~ cd", 5, "ab ", false},
		{"in a block", "```\nabcdef\n```", 7, "```\nabc", true},
		{"before a fence", "```\nabc\n```\nx", 8, "```\nabc\n", true},
		{"a fence is written with its line break, or not at all", "```\nabc\n```\nx", 11, "```\nabc\n", true},
		{"after a block", "```\nabc\n```\nx", 12, "```\nabc\n```\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m mdText
			got := m.text(tc.in+strings.Repeat("z", 40), tc.room+marker, true)
			if !strings.HasSuffix(got, chat.Truncated) || strings.TrimSuffix(got, chat.Truncated) != tc.want {
				t.Fatalf("got %q, want %q and the marker", got, tc.want)
			}
			if m.code != tc.code || !m.inLine || m.held != "" {
				t.Fatalf("state after the cut: %+v; want code %v, in a line, nothing held", m, tc.code)
			}
		})
	}
}

// Every cut of every case, with the rest of the document after it, is still a document that Slack reads as Agent Kourier does.
func TestMarkdownAfterACutIsStillSafe(t *testing.T) {
	for _, tc := range markdownCases {
		for limit := 0; limit < 80; limit += 3 {
			var m mdText
			first := m.text(tc.in, limit, false)
			if chat.Units(first) > limit {
				t.Fatalf("%s: %d units for a limit of %d: %q", tc.name, chat.Units(first), limit, first)
			}
			rest := m.text("\n<@U1> `x` [a](b)\n```\n<!here>\n```\nend ``` ~~~", math.MaxInt, true)
			if bad := slackReading(first + rest); len(bad) > 0 {
				t.Fatalf("%s, limit %d: %v in %q", tc.name, limit, bad, first+rest)
			}
		}
	}
}

// slackReading reads text as Slack reads a markdown document, as the probe measured it (CommonMark fences, and spans of one
// line, rows 08 to 17 and 32 to 44), and returns what breaks the invariant: Slack sees only the code Agent Kourier writes.
//
//   - in prose: no "<", ">", "[" or "]", no backtick that is not escaped (or in a span), no run of three tildes that is
//     not escaped, and no "&" that does not start "&amp;", "&lt;" or "&gt;";
//   - in code: no "<!", "<@" or "<#";
//   - and every line that opens or closes a block is "```", alone at column 0.
func slackReading(doc string) []string {
	var bad []string
	var fence byte // the character of the open block's fence, or 0
	var fenceLen int
	for _, line := range strings.Split(doc, "\n") {
		if fence != 0 {
			if isCloser(line, fence, fenceLen) {
				if line != fenceMark {
					bad = append(bad, fmt.Sprintf("a closing fence that is not %q: %q", fenceMark, line))
				}
				fence = 0
			} else {
				bad = append(bad, codeBreaks(line)...)
			}
			continue
		}
		if ch, n := isOpener(line); ch != 0 {
			if line != fenceMark {
				bad = append(bad, fmt.Sprintf("an opening fence that is not %q: %q", fenceMark, line))
			}
			fence, fenceLen = ch, n
			continue
		}
		bad = append(bad, proseBreaks(line)...)
	}
	return bad
}

// isOpener says whether line opens a fenced block as CommonMark has it: up to three spaces, then three or more backticks
// (with no backtick after them) or tildes. It returns the character and the length of the run.
func isOpener(line string) (byte, int) {
	body := strings.TrimLeft(line, " ")
	if len(line)-len(body) > 3 || body == "" || (body[0] != '`' && body[0] != '~') {
		return 0, 0
	}
	n := runLen(body, body[0])
	if n < 3 || (body[0] == '`' && strings.Contains(body[n:], "`")) {
		return 0, 0
	}
	return body[0], n
}

// isCloser says whether line closes a block opened by a fence of n of ch: up to three spaces, a run of ch of at least n,
// and nothing but white space.
func isCloser(line string, ch byte, n int) bool {
	body := strings.TrimLeft(line, " ")
	if len(line)-len(body) > 3 {
		return false
	}
	run := runLen(body, ch)
	return run >= n && strings.Trim(body[run:], " \t") == ""
}

func codeBreaks(code string) []string {
	var bad []string
	for _, tok := range []string{"<!", "<@", "<#"} {
		if strings.Contains(code, tok) {
			bad = append(bad, fmt.Sprintf("%q in code %q", tok, code))
		}
	}
	return bad
}

func proseBreaks(line string) []string {
	var bad []string
	add := func(what string, i int) { bad = append(bad, fmt.Sprintf("%s at %d of prose %q", what, i, line)) }
	for i := 0; i < len(line); {
		switch c := line[i]; {
		case c == '\\' && i+1 < len(line) && strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", line[i+1]) >= 0:
			i += 2
		case c == '`':
			n := runLen(line[i:], '`')
			span := false
			for j := i + n; j < len(line); {
				if line[j] != '`' {
					j++
					continue
				}
				m := runLen(line[j:], '`')
				if m == n {
					bad = append(bad, codeBreaks(line[i+n:j])...)
					i, span = j+m, true
					break
				}
				j += m
			}
			if !span {
				add("a backtick that is not escaped", i)
				i += n
			}
		case c == '~':
			n := runLen(line[i:], '~')
			if n >= 3 {
				add("a run of tildes that is not escaped", i)
			}
			i += n
		case c == '<' || c == '>' || c == '[' || c == ']':
			add(fmt.Sprintf("a raw %q", c), i)
			i++
		case c == '&':
			if rest := line[i:]; !strings.HasPrefix(rest, "&amp;") && !strings.HasPrefix(rest, "&lt;") && !strings.HasPrefix(rest, "&gt;") {
				add("a raw &", i)
			}
			i++
		default:
			i++
		}
	}
	return bad
}

// The model reads what a person would write, so that it can be trusted to find what breaks.
func TestSlackReadingFindsWhatBreaksTheInvariant(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		bad       bool
	}{
		{"a clean document", "a &lt;b&gt; \\[x\\] `<c>`\n```\n<d> [e]\n```", false},
		{"a raw angle bracket", "a <b", true},
		{"a raw bracket", "a [b", true},
		{"an escaped bracket", "a \\[b", false},
		{"a raw ampersand", "a & b", true},
		{"an entity", "a &amp; &lt; &gt;", false},
		{"a stray backtick", "a ` b", true},
		{"an escaped backtick", "a \\` b", false},
		{"a span is code", "`<c> [d]`", false},
		{"a mention in a span", "`<@U1>`", true},
		{"a mention in a block", "```\n<!here>\n```", true},
		{"a mention in prose, raw", "<@U1>", true},
		{"three tildes", "~~~ x", true},
		{"escaped tildes", "\\~\\~\\~ x", false},
		{"a fence indented", "  ```\nx\n```", true},
		{"a closer with a tail is code, and the block goes on", "```\n``` x\n<!here>\n```", true},
		{"a block's line that looks like a fence closes it", "```\n   ```\n[x]", true},
		{"a fence in a quote is prose", "> ```", true},
		{"a fence with text after it does not close the block", "```\n``` ```\n<!here>\n```", true},
	} {
		if bad := slackReading(tc.doc); (len(bad) > 0) != tc.bad {
			t.Errorf("%s: found %v; want a break: %v", tc.name, bad, tc.bad)
		}
	}
}

// markdownSeeds are the texts the fuzzers start from: the cases above, and some whose parts meet.
func markdownSeeds() []string {
	seeds := []string{
		"", "`", "``", "```", "~~~", "~~", "\\", "<", "<!", "&", "```\n", "\n```", "`a`\n```\n`b`",
		"a\n```\nb <!here>\n```\n<@U1> [x](y) `<#C1>`",
		"- a\n  ```\n  b\n  ```\n  ```\n",
		"````\n```\nx\n````\n```` ````",
		"```a``` ``` b ```` `c` ``d`e`` ~~~ ~~~x~~~",
		"> ```\n    ```\n\t~~~\n```\n\t```\n   x",
	}
	for _, tc := range markdownCases {
		seeds = append(seeds, tc.in)
	}
	return seeds
}

// runeBoundary is the nearest place at or before i in s at which a character starts.
func runeBoundary(s string, i int) int {
	i = min(i, len(s))
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// FuzzMarkdownReading is the invariant: whatever the agent writes, in one piece or in two with the first cut short,
// Slack sees only the code Agent Kourier writes. Run it for longer than the seeds with
// go test -fuzz=FuzzMarkdownReading -fuzztime=2m ./internal/chat/slack/
func FuzzMarkdownReading(f *testing.F) {
	for _, s := range markdownSeeds() {
		f.Add(s, uint16(len(s)/2), uint16(len(s)))
	}
	f.Fuzz(func(t *testing.T, in string, at, limit uint16) {
		whole := markdown(in, math.MaxInt)
		if bad := slackReading(whole); len(bad) > 0 {
			t.Fatalf("%v\nin  %q\nout %q", bad, in, whole)
		}
		clean := chat.Clean(in, false)
		cut := runeBoundary(clean, int(at))
		var m mdText
		first := m.text(clean[:cut], int(limit)%400, false)
		if chat.Units(first) > int(limit)%400 {
			t.Fatalf("%d units for a limit of %d: %q", chat.Units(first), int(limit)%400, first)
		}
		rest := first + m.text(clean[cut:], math.MaxInt, true)
		if bad := slackReading(rest); len(bad) > 0 {
			t.Fatalf("%v\nin  %q cut at %d, limit %d\nout %q", bad, in, cut, int(limit)%400, rest)
		}
	})
}

// FuzzMarkdownChunks is a stream: the same text in pieces cut anywhere comes out as the same text as in one piece.
func FuzzMarkdownChunks(f *testing.F) {
	for i, s := range markdownSeeds() {
		f.Add(s, uint64(i))
	}
	f.Fuzz(func(t *testing.T, in string, seed uint64) {
		clean := chat.Clean(in, false)
		want := markdown(clean, math.MaxInt)
		rnd := rand.New(rand.NewPCG(seed, 7))
		var m mdText
		var got strings.Builder
		var pieces []string
		for rest := clean; rest != ""; {
			n := runeBoundary(rest, 1+rnd.IntN(len(rest)))
			if n == 0 {
				_, n = utf8.DecodeRuneInString(rest)
			}
			pieces = append(pieces, rest[:n])
			got.WriteString(m.text(rest[:n], math.MaxInt, false))
			rest = rest[n:]
		}
		got.WriteString(m.text("", math.MaxInt, true))
		if got.String() != want {
			t.Fatalf("in pieces %q\n got %q\nwant %q", pieces, got.String(), want)
		}
	})
}
