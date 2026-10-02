package chattest

// Case is one piece of hostile text: what an agent, a person, a tool or an alert might write to make a
// platform do something it was not asked to.
type Case struct {
	Name string
	Text string
}

// Corpus is the hostile text every adapter is checked with, in every place text can go (chat.Where). It is
// grouped by what the text tries; an adapter that shows any of it as anything but the characters it is
// made of, or loses it, fails Conformance. Add to it, never remove: a new attack is one more line here
// and is held against every adapter at once.
var Corpus = concat(
	broadcast, mentions, links, html, markdown, invisible, lines, benign,
)

func concat(groups ...[]Case) []Case {
	var all []Case
	for _, g := range groups {
		all = append(all, g...)
	}
	return all
}

// Broadcasts and channel links: notify everyone, or point at a channel.
var broadcast = []Case{
	{"channel", "<!channel>"},
	{"here", "<!here>"},
	{"everyone", "<!everyone>"},
	{"channel with a label", "<!channel|channel>"},
	{"user group", "<!subteam^S0123456|@oncall>"},
	{"date", "<!date^1700000000^{date_short_pretty} at {time}|then>"},
	{"channel link", "<#C0123456789>"},
	{"channel link with a name", "<#C0123456789|general>"},
	{"plain @channel", "@channel"},
	{"plain @here", "@here"},
	{"plain @everyone", "@everyone"},
	{"entity-encoded broadcast", "&lt;!channel&gt;"},
	{"numeric entity broadcast", "&#60;!channel&#62;"},
	{"hex entity broadcast", "&#x3C;!channel&#x3E;"},
}

// Mentions: notify one person.
var mentions = []Case{
	{"user", "<@U0123456789>"},
	{"user with a name", "<@U0123456789|ada>"},
	{"workspace user", "<@W0123456789>"},
	{"plain @name", "@ada"},
	{"user text in a sentence", "ping <@U0123456789> now"},
}

// Links, with a label the author chooses or without, and the schemes that run or phone.
var links = []Case{
	{"autolink", "<https://evil.test/login>"},
	{"link with a label", "<https://evil.test/login|your bank>"},
	{"mailto", "<mailto:a@evil.test|mail us>"},
	{"tel", "<tel:+15551234567|call>"},
	{"javascript", "<javascript:alert(1)>"},
	{"markdown link", "[your bank](https://evil.test/login)"},
	{"markdown link, javascript", "[x](javascript:alert(1))"},
	{"markdown image", "![tracker](https://evil.test/pixel.png)"},
	{"markdown reference link", "[x][1]\n\n[1]: https://evil.test"},
	{"bare URL", "https://evil.test/login"},
	{"bare www", "www.evil.test"},
	{"URL in a sentence", "see https://evil.test/login now"},
}

// HTML and entities.
var html = []Case{
	{"script", "<script>alert(1)</script>"},
	{"bold tag", "<b>bold</b>"},
	{"image tag", "<img src=x onerror=alert(1)>"},
	{"anchor", `<a href="https://evil.test">click</a>`},
	{"comment", "<!-- hidden -->"},
	{"unclosed tag", "<b never closed"},
	{"ampersand entity", "&amp;"},
	{"lt entity", "&lt;b&gt;"},
	{"bare ampersand", "a & b"},
	{"angle brackets", "a < b > c"},
}

// Markdown: emphasis, code, headings, quotes, lists, tables, and the escapes that try to end an escape.
var markdown = []Case{
	{"bold", "*bold*"},
	{"italic", "_italic_"},
	{"strike", "~strike~"},
	{"double star", "**bold**"},
	{"double underscore", "__bold__"},
	{"code", "`code`"},
	{"fence", "```\nblock\n```"},
	{"heading", "# heading"},
	{"quote", "> quote"},
	{"dash list", "- item"},
	{"star list", "* item"},
	{"numbered list", "1. item"},
	{"table", "| a | b |\n|---|---|\n| 1 | 2 |"},
	{"backslash", `\*not bold\*`},
	{"trailing backslash", `end\`},
	{"escaped ampersand", `\&lt;`},
	{"emoji code", ":skull:"},
	{"emphasis around a mention", "*<@U0123456789>*"},
	{"code around a link", "`<https://evil.test|x>`"},
	{"snake_case_name", "snake_case_name and file*name*here"},
}

// Invisible and reordering characters, alone and splitting a construct apart so that a filter that
// looks for "<!channel>" does not see it but the platform's reader joins it.
var invisible = []Case{
	{"zero-width space", "a\u200bb"},
	{"zero-width joiner", "a\u200db"},
	{"zero-width non-joiner", "a\u200cb"},
	{"word joiner", "a\u2060b"},
	{"byte order mark", "a\ufeffb"},
	{"soft hyphen", "a\u00adb"},
	{"left-to-right override", "a\u202db"},
	{"right-to-left override", "gpj.exe\u202e"},
	{"bidi isolates", "a\u2066b\u2069c"},
	{"invisible tag characters", "a\U000E0001\U000E0041\U000E007Fb"},
	{"control characters", "a\x00b\x07c\x1b[31md\x7f"},
	{"NEL", "a\u0085b"},
	{"split broadcast", "<\u200b!channel>"},
	{"split broadcast, inside", "<!chan\u200bnel>"},
	{"split mention", "<@U\u200b0123456789>"},
	{"split entity", "&\u200blt;!channel&gt;"},
	{"split link", "<https://evil.test|x>\u200b"},
	{"split markdown link", "[x\u200b](https://evil.test)"},
	{"split emphasis", "*\u2060bold*"},
	{"private use", "a\ue000b"},
	{"invalid UTF-8", "a\xffb\xc3"},
}

// Line breaks, in places that are one line, and white space.
var lines = []Case{
	{"newline", "line one\nline two"},
	{"CRLF", "line one\r\nline two"},
	{"CR", "line one\rline two"},
	{"line separator", "line one\u2028line two"},
	{"paragraph separator", "line one\u2029line two"},
	{"blank lines", "a\n\n\n\nb"},
	{"newline then a mention", "text\n<@U0123456789>"},
	{"newline then a heading", "text\n# heading"},
	{"newline then a quote", "text\n> quote"},
	{"tabs and spaces", "a\t\t  b   c"},
	{"only white space", " \n\t "},
}

// Benign text that must come out as written, so that a filter that drops or mangles everything does not
// pass for a safe one.
var benign = []Case{
	{"plain", "The deploy finished."},
	{"unicode", "日本語, ünïcödé, Ελληνικά, עברית"},
	{"astral", "party 🎉 time 🚀"},
	{"punctuation", "Is it 5 > 3? Yes (really); that costs $5, 50% off."},
	{"empty", ""},
}
