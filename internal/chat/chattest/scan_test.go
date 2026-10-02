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

func TestScanReadsWhatAPlatformWouldMakeOfTheWire(t *testing.T) {
	for _, tc := range []struct {
		name, wire, displayed string
		want                  []Kind
	}{
		{"plain text", "The deploy finished.", "The deploy finished.", nil},
		{"entities are characters", "a &amp; b &lt;!channel&gt;", "a & b <!channel>", nil},
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
		{"a markdown link", "[bank](https://evil.test)", "bank", []Kind{KindLink}},
		{"a markdown image", "![alt](https://evil.test/x.png)", "alt", []Kind{KindImage}},
		{"a bare URL", "see https://evil.test now", "see https://evil.test now", []Kind{KindAutolink}},
		{"a bare www", "www.evil.test", "www.evil.test", []Kind{KindAutolink}},
		{"emphasis", "*b* and _i_ and ~s~", "b and i and s", []Kind{KindEmphasis, KindEmphasis, KindEmphasis}},
		{"double emphasis", "**b** __b__", "b b", []Kind{KindEmphasis, KindEmphasis}},
		{"code", "`<!channel>`", "<!channel>", []Kind{KindCode}},
		{"a fence", "```\nx\n```", "\nx\n", []Kind{KindCode}},
		{"a heading", "# title", " title", []Kind{KindHeading}},
		{"a quote", "> said", " said", []Kind{KindQuote}},
		{"a list", "- one\n1. two", " one\n two", []Kind{KindList, KindList}},
		{"a marker after a newline", "text\n# h", "text\n h", []Kind{KindHeading}},
		{"snake_case is not emphasis", "snake_case_name", "snake_case_name", nil},
		{"a star in a sum is not emphasis", "2 * 3 * 4", "2 * 3 * 4", nil},
		{"an escaped star", `\*not\*`, "*not*", nil},
		{"every punctuation escaped", `\<\!channel\> \[a\]\(b\) \#h \> q \- l 1\. x`, "<!channel> [a](b) #h > q - l 1. x", nil},
		{"an escaped URL is no link", `https\://evil.test www\.evil.test`, "https://evil.test www.evil.test", nil},
		{"entities are not a tag", "&lt;b&gt;x&lt;/b&gt;", "<b>x</b>", nil},
		{"emoji is not a construct", ":skull:", ":skull:", nil},
	} {
		got := Scan(tc.wire)
		if got.Displayed != tc.displayed || !slices.Equal(kinds(got), tc.want) {
			t.Errorf("%s: Scan(%q) = %q %v, want %q %v", tc.name, tc.wire, got.Displayed, kinds(got), tc.displayed, tc.want)
		}
	}
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
