package chattest

import (
	"slices"
	"testing"
)

func kinds(s Shown) []Kind {
	var out []Kind
	for _, a := range s.Active {
		out = append(out, a.Kind)
	}
	return out
}

type scanCase struct {
	name, wire, displayed string
	want                  []Kind
}

func runScans(t *testing.T, scan func(string) Shown, cases []scanCase) {
	t.Helper()
	for _, tc := range cases {
		got := scan(tc.wire)
		if got.Displayed != tc.displayed || !slices.Equal(kinds(got), tc.want) {
			t.Errorf("%s: scan(%q) = %q %v, want %q %v", tc.name, tc.wire, got.Displayed, kinds(got), tc.displayed, tc.want)
		}
	}
}

// What Slack's mrkdwn makes of the wire. There is no escape but the three entities, so a backslash does not
// stop anything, which is how an adapter that escaped as for CommonMark once passed the suite.
func TestScanSlackReadsWhatSlackMakesOfMrkdwn(t *testing.T) {
	runScans(t, ScanSlack, []scanCase{
		{"plain text", "The deploy finished.", "The deploy finished.", nil},
		{"entities are characters", "a &amp; b &lt;!channel&gt;", "a & b <!channel>", nil},
		{"only three entities", "&#60;!channel&#62; &quot;", "&#60;!channel&#62; &quot;", nil},
		{"a raw broadcast", "<!channel>", "!channel", []Kind{KindBroadcast}},
		{"a broadcast with a label", "<!here|here>", "here", []Kind{KindBroadcast}},
		{"a user group", "<!subteam^S1|@oncall>", "@oncall", []Kind{KindBroadcast}},
		{"a date", "<!date^1700000000^{date}|then>", "then", []Kind{KindDate}},
		{"a mention", "hi <@U123>", "hi @U123", []Kind{KindMention}},
		{"a channel link", "<#C123|general>", "general", []Kind{KindChannelLink}},
		{"a link with a label", "<https://evil.test|your bank>", "your bank", []Kind{KindLink}},
		{"mailto", "<mailto:a@b.c|mail>", "mail", []Kind{KindLink}},
		{"a tag", "<b>x</b>", "bx/b", []Kind{KindHTML, KindHTML}},
		{"a tag never closed", "x <b never closed", "x ", []Kind{KindHTML}},
		{"a backslash is a backslash: the < is still a token", `\<!channel> \<@U1> \<https://evil.test|your bank>`, `\!channel \@U1 \your bank`, []Kind{KindBroadcast, KindMention, KindLink}},
		{"a backslash before an ampersand is a backslash", `\&lt;!channel\&gt;`, `\<!channel\>`, nil},
		{"markdown links are text", "[bank](https://evil.test)", "[bank](https://evil.test)", []Kind{KindAutolink}},
		{"a reference is text", "[x][1]\n\n[1]: https://evil.test", "[x][1]\n\n[1]: https://evil.test", []Kind{KindAutolink}},
		{"a bare URL", "see https://evil.test now", "see https://evil.test now", []Kind{KindAutolink}},
		{"a bare www", "www.evil.test", "www.evil.test", []Kind{KindAutolink}},
		{"an email address", "write to ada@evil.test now", "write to ada@evil.test now", []Kind{KindAutolink}},
		{"emphasis", "*b* and _i_ and ~s~", "b and i and s", []Kind{KindEmphasis, KindEmphasis, KindEmphasis}},
		{"code", "`x`", "x", []Kind{KindCode}},
		{"a token in code is still reported", "`<!channel>` ```<@U1>```", "<!channel> <@U1>", []Kind{KindCode, KindBroadcast, KindCode, KindMention}},
		{"emphasis hides nothing: a mention", "*<@U1>*", "@U1", []Kind{KindEmphasis, KindMention}},
		{"emphasis hides nothing: a broadcast", "_<!channel>_", "!channel", []Kind{KindEmphasis, KindBroadcast}},
		{"emphasis hides nothing: a link", "~<https://evil.test|your bank>~", "your bank", []Kind{KindEmphasis, KindLink}},
		{"emphasis hides nothing: a bare URL", "*see https://evil.test*", "see https://evil.test", []Kind{KindEmphasis, KindAutolink}},
		{"emphasis inside emphasis", "*_<@U1>_*", "@U1", []Kind{KindEmphasis, KindEmphasis, KindMention}},
		{"a quote", "> said", " said", []Kind{KindQuote}},
		{"a heading and a list are text", "# title\n- one\n1. two", "# title\n- one\n1. two", nil},
		{"snake_case is not emphasis", "snake_case_name", "snake_case_name", nil},
		{"a star in a sum is not emphasis", "2 * 3 * 4", "2 * 3 * 4", nil},
		{"emoji is not a construct", ":skull:", ":skull:", nil},
	})
}

// A plain_text object shows its text as it is, so style is not a construct there, and the three entities
// are read; a token or a link in it is still reported, because that it parses none is not known.
func TestScanSlackPlainShowsTheTextAsItIsAndStillFindsTokens(t *testing.T) {
	runScans(t, ScanSlackPlain, []scanCase{
		{"entities", "a &amp; b &lt;!channel&gt;", "a & b <!channel>", nil},
		{"style is text", "*b* _i_ `c` # h > q", "*b* _i_ `c` # h > q", nil},
		{"an escaped markdown link", `\[a\]\(b\)`, `\[a\]\(b\)`, nil},
		{"a raw broadcast", "<!channel>", "<!channel>", []Kind{KindBroadcast}},
		{"a raw mention behind a backslash", `\<@U1>`, `\<@U1>`, []Kind{KindMention}},
		{"a raw link", "<https://evil.test|x>", "<https://evil.test|x>", []Kind{KindLink}},
		{"a bare URL", "https://evil.test", "https://evil.test", []Kind{KindAutolink}},
	})
}

// What CommonMark makes of the wire: a backslash before punctuation is the character, entities are
// characters, and links come inline, by reference, in angle brackets, and as images. The structure is goldmark's, so
// what is shown is the text of its tree: a link reference definition shows nothing, white space at the ends of a
// line and the marker of a heading, quote or list item are gone, blocks are a blank line apart (a list's items a
// line break), a link's destination is balanced as CommonMark balances it, and "~" is no emphasis, which is an
// extension of CommonMark and not in it.
func TestScanCommonMarkReadsWhatCommonMarkMakesOfTheWire(t *testing.T) {
	runScans(t, ScanCommonMark, []scanCase{
		{"plain text", "The deploy finished.", "The deploy finished.", nil},
		{"entities are characters", "a &amp; b &lt;!channel&gt;", "a & b <!channel>", nil},
		{"numeric and named entities are characters", "&#60;!channel&#x3E; &quot;", `<!channel> "`, nil},
		{"a raw broadcast", "<!channel>", "!channel", []Kind{KindBroadcast}},
		{"an angle autolink", "<https://evil.test|x>", "x", []Kind{KindLink}},
		{"raw HTML", "<b>x</b>", "bx/b", []Kind{KindHTML, KindHTML}},
		{"an inline link", "[bank](https://evil.test)", "bank", []Kind{KindLink}},
		{"an image", "![alt](https://evil.test/x.png)", "alt", []Kind{KindImage}},
		{"a reference link and its definition", "[x][1]\n\n[1]: https://evil.test", "x", []Kind{KindLink, KindLink}},
		{"a definition survives its escapes", "[x][1]\n\n[1]: https\\:\\/\\/evil\\.test", "x", []Kind{KindLink, KindLink}},
		{"a shortcut reference", "[evil]\n\n[evil]: https://evil.test", "evil", []Kind{KindLink, KindLink}},
		{"a collapsed reference", "[evil][]\n\n[Evil]: https://evil.test", "evil", []Kind{KindLink, KindLink}},
		{"a definition with its destination on the next line", "[x][1]\n\n[1]:\n  https://evil.test", "x", []Kind{KindLink, KindLink}},
		{"a definition whose label runs over two lines", "[x\n]\n\n[x\n]: https://evil.test", "x\n", []Kind{KindLink, KindLink}},
		{"a definition whose label starts and ends with a line ending", "[\nfoo\n]: https://evil.test\n\n[foo]", "foo", []Kind{KindLink, KindLink}},
		{"a label that holds a blank line is no definition", "[a\n\nb]: https://evil.test\n\n[a]", "[a\n\nb]: https://evil.test\n\n[a]", []Kind{KindAutolink}},
		{"a label of white space is no definition", "[ \n ]: https://evil.test", "[\n]: https://evil.test", []Kind{KindAutolink}},
		{"a definition after a label that holds a blank line is still one", "[a\n\n[b]: https://evil.test\n\n[b]", "[a\n\nb", []Kind{KindLink, KindLink}},
		{"escaped brackets in a reference label", "[Verify \\[your\\] account]\n\n[verify \\[your\\] account]: https://evil.test", "Verify [your] account", []Kind{KindLink, KindLink}},
		{"escaped brackets in a reference label, over two lines", "[Verify \\[your\\]\naccount]\n\n[verify \\[your\\]\naccount]: https://evil.test", "Verify [your]\naccount", []Kind{KindLink, KindLink}},
		{"an escaped closing bracket in a full reference's label", "[x][a\\]b]\n\n[a\\]b]: https://evil.test", "x", []Kind{KindLink, KindLink}},
		{"an inline link whose text holds escaped brackets", "[Verify \\[your\\] account](https://evil.test)", "Verify [your] account", []Kind{KindLink}},
		{"an inline link whose text holds an escaped closing bracket", "[a\\](b](https://evil.test)", "a](b", []Kind{KindLink}},
		{"an inline link whose text holds balanced brackets", "[a [b] c](https://evil.test)", "a [b] c", []Kind{KindLink}},
		{"an inline link whose destination holds an escaped parenthesis", "[x](https://evil.test/a\\)b)", "x", []Kind{KindLink}},
		{"a reference link whose text holds balanced brackets", "[a [b] c][1]\n\n[1]: https://evil.test", "a [b] c", []Kind{KindLink, KindLink}},
		{"a label with an unescaped bracket in it is no label", "[a [b]\n\n[a [b]: https://evil.test", "[a [b]\n\n[a [b]: https://evil.test", []Kind{KindAutolink}},
		{"escaped brackets are text, and the definition is still one", "\\[x\\]\n\n[x]: https://evil.test", "[x]", []Kind{KindLink}},
		{"a URL in link text is no second link", "[https://evil.test/a](https://evil.test/b)", "https://evil.test/a", []Kind{KindLink}},
		{"a destination in angle brackets, with a space", "[x](<https://evil.test/a b>)", "x", []Kind{KindLink}},
		{"a destination with a title", "[x]( https://evil.test \"title\" )", "x", []Kind{KindLink}},
		{"an image by reference", "![x][1]\n\n[1]: https://evil.test/p.png", "x", []Kind{KindImage, KindLink}},
		{"an escaped parenthesis ends nothing", "[x]\\(https://evil.test)", "[x](https://evil.test)", []Kind{KindAutolink}},
		{"a line break between the text and the destination is no link", "[x]\n(https://evil.test)", "[x]\n(https://evil.test)", []Kind{KindAutolink}},
		{"a definition may be indented by three spaces", " [x]: https://evil.test\n\n[x]", "x", []Kind{KindLink, KindLink}},
		{"four spaces make code, and no definition", "    [x]: https://evil.test\n\n[x]", "[x]: https://evil.test\n\n[x]", []Kind{KindCode, KindAutolink}},
		{"a definition with nothing after it is text", "[1]:\n\nhello", "[1]:\n\nhello", nil},
		{"emphasis hides nothing: a link", "*[your bank](https://evil.test)*", "your bank", []Kind{KindEmphasis, KindLink}},
		{"emphasis hides nothing: a reference link", "_[x][1]_\n\n[1]: https://evil.test", "x", []Kind{KindEmphasis, KindLink, KindLink}},
		{"emphasis hides nothing: raw", "**<@U1>** ~~<!here>~~", "@U1 ~~!here~~", []Kind{KindEmphasis, KindMention, KindBroadcast}},
		{"a link whose text runs over two lines", "[your\nbank](https://evil.test/login)", "your\nbank", []Kind{KindLink}},
		{"a javascript link whose text runs over two lines", "[your\nbank](javascript:alert(1))", "your\nbank", []Kind{KindLink}},
		{"an image whose alt text runs over two lines", "![a\nb](https://evil.test/p.png)", "a\nb", []Kind{KindImage}},
		{"escaped link text over two lines is text", "\\[your\nbank\\]\\(https\\://evil.test/login\\)", "[your\nbank](https://evil.test/login)", nil},
		{"a blank line ends what can be link text", "[a\n\nb](javascript:alert(1))", "[a\n\nb](javascript:alert(1))", nil},
		{"a reference link whose label runs over two lines", "[a\nb][1]\n\n[1]: https://evil.test", "a\nb", []Kind{KindLink, KindLink}},
		{"a reference label is matched whatever its white space", "[a  b][A\nB]\n\n[a b]: https://evil.test", "a  b", []Kind{KindLink, KindLink}},
		{"a link binds tighter than emphasis: underscore", "_[x_](javascript:alert(1))", "_x_", []Kind{KindLink}},
		{"a link binds tighter than emphasis: stars", "**[x](https://evil.test**)", "**x", []Kind{KindLink}},
		{"a link inside emphasis that closes after it", "*a [b*](javascript:alert(1)) c*", "a b* c", []Kind{KindEmphasis, KindLink}},
		{"a bracket with no definition is text", "[x][1] and [y]", "[x][1] and [y]", nil},
		{"an escaped definition is text", `\[1\]: https\://evil.test`, "[1]: https://evil.test", nil},
		{"a bare URL", "see https://evil.test now", "see https://evil.test now", []Kind{KindAutolink}},
		{"a bare URL in capitals", "HTTPS://EVIL.TEST/x", "HTTPS://EVIL.TEST/x", []Kind{KindAutolink}},
		{"a bare ftp URL", "ftp://evil.test/x", "ftp://evil.test/x", []Kind{KindAutolink}},
		{"an email address", "ada@evil.test", "ada@evil.test", []Kind{KindAutolink}},
		{"emphasis", "*b* and _i_ and ~s~ and **b** __b__", "b and i and ~s~ and b b", []Kind{KindEmphasis, KindEmphasis, KindEmphasis, KindEmphasis}},
		{"code", "`plain`", "plain", []Kind{KindCode}},
		{"a token in a code span is reported", "`<!channel>`", "<!channel>", []Kind{KindCode, KindBroadcast}},
		{"a token in a fence is reported", "```\n<@U1>\n```", "<@U1>", []Kind{KindCode, KindMention}},
		{"an indented block is code, and a token in it is reported", "    <@U1>", "<@U1>", []Kind{KindCode, KindMention}},
		{"a bare URL in code is reported", "`https://evil.test`", "https://evil.test", []Kind{KindCode, KindAutolink}},
		{"an indented block with no token is code", "    plain", "plain", []Kind{KindCode}},
		{"a fence", "```\nx\n```", "x", []Kind{KindCode}},
		{"a heading", "# title", "title", []Kind{KindHeading}},
		{"a quote", "> said", "said", []Kind{KindQuote}},
		{"a list", "- one\n1. two", "one\n\ntwo", []Kind{KindList, KindList}},
		{"a marker after a newline", "text\n# h", "text\n\nh", []Kind{KindHeading}},
		{"snake_case is not emphasis", "snake_case_name", "snake_case_name", nil},
		{"a star in a sum is not emphasis", "2 * 3 * 4", "2 * 3 * 4", nil},
		{"an escaped star", `\*not\*`, "*not*", nil},
		{"every punctuation escaped", `\<\!channel\> \[a\]\(b\) \#h \> q \- l 1\. x`, "<!channel> [a](b) #h > q - l 1. x", nil},
		{"an escaped URL is no link", `https\://evil.test www\.evil.test`, "https://evil.test www.evil.test", nil},
		{"entities are not a tag", "&lt;b&gt;x&lt;/b&gt;", "<b>x</b>", nil},
		{"emoji is not a construct", ":skull:", ":skull:", nil},
	})
}

func TestStyleKindsAreExactlyTheCosmeticOnes(t *testing.T) {
	for _, k := range []Kind{KindEmphasis, KindCode, KindHeading, KindQuote, KindList} {
		if !k.Style() {
			t.Errorf("%s is cosmetic and is not Style", k)
		}
	}
	for _, k := range []Kind{KindMention, KindBroadcast, KindChannelLink, KindLink, KindImage, KindHTML, KindDate, KindAutolink} {
		if k.Style() {
			t.Errorf("%s notifies or links and is Style", k)
		}
	}
}
