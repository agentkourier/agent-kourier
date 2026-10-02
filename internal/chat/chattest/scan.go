package chattest

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind is a construct a platform acts on in text: something other than the characters themselves.
type Kind string

// The constructs. The first group is dangerous in text that an agent or a person wrote: it notifies
// someone, links somewhere, or runs. The second is style, which is cosmetic.
const (
	KindMention     Kind = "mention"      // notifies one person: <@U1>
	KindBroadcast   Kind = "broadcast"    // notifies many: <!channel>, <!here>, <!everyone>, <!subteam^S1>
	KindChannelLink Kind = "channel link" // <#C1>
	KindLink        Kind = "link"         // one whose label the author chooses: <https://x|y>, [y](https://x), [y][1] with a definition
	KindImage       Kind = "image"        // ![alt](https://x)
	KindHTML        Kind = "html"         // a tag, or markup the platform's own parser would take
	KindDate        Kind = "date"         // <!date^...>
	KindAutolink    Kind = "autolink"     // a bare URL or email address the platform makes into a link showing itself

	KindEmphasis Kind = "emphasis" // *b*, _i_, ~s~
	KindCode     Kind = "code"     // `c`, and a fenced block
	KindHeading  Kind = "heading"  // a line that starts with #
	KindQuote    Kind = "quote"    // a line that starts with >
	KindList     Kind = "list"     // a line that starts with - or 1.
)

// Style says whether k is cosmetic: it changes how text looks and notifies and links to nothing.
func (k Kind) Style() bool {
	switch k {
	case KindEmphasis, KindCode, KindHeading, KindQuote, KindList:
		return true
	}
	return false
}

// Active is one construct a platform would act on, and the text that makes it.
type Active struct {
	Kind Kind
	Text string
}

// Shown is a piece of text as a person sees it, and the constructs the platform acted on in it. It is
// what a platform's client makes of what an adapter sent, so an adapter that sends text with markup in it
// has to say what that markup does, in the way the platform reads that text: with the reader of the
// platform's dialect (ScanSlack, ScanSlackPlain or ScanCommonMark), and never with another's.
type Shown struct {
	Displayed string
	Active    []Active
}

// dialect is how a platform reads one kind of text. The differences are what a careful adapter gets
// wrong: escaping for a dialect that does not honour the escape (a backslash before "<" in Slack's mrkdwn
// is a backslash, and the "<" is a token). CommonMark is read by goldmark (commonmark.go), which uses this only
// for the text between constructs.
type dialect struct {
	numeric bool // &#60; &#x3C; &quot; and &apos; are characters

	codeTokens bool // a token inside a code span is reported: it is not known to be left alone
}

var (
	slackDialect      = dialect{codeTokens: true}
	commonMarkDialect = dialect{numeric: true}
)

// ScanSlack reads Slack's mrkdwn: a message's top-level text, and a mrkdwn text object. Only &amp; &lt; and
// &gt; are entities, and there is no other escape: a backslash is a backslash, and what follows it is read as
// if it were not there, so every "<" is the start of a token, a mention, a broadcast, a date, a channel
// link or a link, or a tag. "[text](url)" is text there (a link is "<url|text>"). Bare URLs and email
// addresses are autolinked. A quote is the one line marker.
func ScanSlack(wire string) Shown { return scan(wire, slackDialect) }

// ScanSlackPlain reads a Slack plain_text object, which has no emphasis, code or list: the text is shown as
// it is, with the three entities read. A token or a link in it is still reported, because that a
// plain_text object parses none is not known (UNCONFIRMED(WP-04)), and an adapter must not rely on it.
func ScanSlackPlain(wire string) Shown {
	var active []Active
	for _, a := range scan(wire, slackDialect).Active {
		if !a.Kind.Style() {
			active = append(active, a)
		}
	}
	return Shown{Displayed: slackEntities.Replace(wire), Active: active}
}

var slackEntities = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")

func scan(wire string, d dialect) Shown { return scanText(wire, d, true) }

// scanText reads wire, which may be a part of the whole text, and atLineStart is whether wire starts a line. What
// is inside an emphasis span is read as the whole text is, and what it holds is active as if it stood alone.
func scanText(wire string, d dialect, atLineStart bool) Shown {
	var out strings.Builder
	var active []Active
	for i := 0; i < len(wire); {
		if atLineStart {
			if n, kind := lineMarker(wire[i:]); n > 0 {
				active = append(active, Active{kind, wire[i : i+n]})
				i += n
				continue // a marker is followed by more text on the same line
			}
		}
		atLineStart = false
		r, size := utf8.DecodeRuneInString(wire[i:])
		switch {
		case r == '\n':
			out.WriteByte('\n')
			atLineStart = true
			i += size
		case r == '&':
			i += entity(wire[i:], d, &out)
		case r == '<':
			i += token(wire[i:], &out, &active)
		case r == '`':
			i += code(wire[i:], d, &out, &active)
		case r == '*' || r == '_' || r == '~':
			if n, inner := emphasis(wire, i); n > 0 {
				active = append(active, Active{KindEmphasis, wire[i : i+n]})
				in := scanText(inner, d, false)
				active = append(active, in.Active...) // emphasis hides nothing: what is inside it acts
				out.WriteString(in.Displayed)
				i += n
			} else {
				out.WriteRune(r)
				i += size
			}
		case wordStart(wire, i) && (autolink.MatchString(wire[i:]) || email.MatchString(wire[i:])):
			url := autolink.FindString(wire[i:])
			if url == "" {
				url = email.FindString(wire[i:])
			}
			active = append(active, Active{KindAutolink, url})
			out.WriteString(url)
			i += len(url)
		default:
			out.WriteRune(r)
			i += size
		}
	}
	return Shown{Displayed: out.String(), Active: active}
}

var (
	autolink      = regexp.MustCompile(`(?i)^(?:(?:https?|ftp)://|www\.)[^\s<>]+`)
	email         = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+`)
	numericEntity = regexp.MustCompile(`^&#(?:[xX]([0-9a-fA-F]{1,6})|([0-9]{1,7}));`)
)

func isPunct(b byte) bool {
	return b < utf8.RuneSelf && (unicode.IsPunct(rune(b)) || unicode.IsSymbol(rune(b)))
}

func wordStart(s string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

// lineMarker returns the length of what the line at the start of s starts with, and its kind: in Slack's mrkdwn
// a quote is the one marker.
func lineMarker(s string) (int, Kind) {
	j := 0
	for j < len(s) && s[j] == ' ' && j < 3 {
		j++
	}
	rest := s[j:]
	if strings.HasPrefix(rest, "> ") || rest == ">" {
		return j + 1, KindQuote
	}
	return 0, ""
}

// entity writes the character that the entity at the start of s stands for, or "&" if it is not one, and
// returns how much of s it used. Slack has three entities; CommonMark has numeric ones and more.
func entity(s string, d dialect, out *strings.Builder) int {
	for _, e := range [][2]string{{"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"}} {
		if strings.HasPrefix(s, e[0]) {
			out.WriteString(e[1])
			return len(e[0])
		}
	}
	if d.numeric {
		for _, e := range [][2]string{{"&quot;", `"`}, {"&apos;", "'"}} {
			if strings.HasPrefix(s, e[0]) {
				out.WriteString(e[1])
				return len(e[0])
			}
		}
		if m := numericEntity.FindStringSubmatch(s); m != nil {
			base, digits := 10, m[2]
			if m[1] != "" {
				base, digits = 16, m[1]
			}
			if cp, err := strconv.ParseInt(digits, base, 32); err == nil && cp > 0 && cp <= unicode.MaxRune && utf8.ValidRune(rune(cp)) {
				out.WriteRune(rune(cp))
				return len(m[0])
			}
		}
	}
	out.WriteByte('&')
	return 1
}

// token reads the "<...>" at the start of s: a mention, a broadcast, a channel link, a date, a link, or
// a tag. A "<" with no closing ">" is a tag that is never closed, and takes the rest of the text.
func token(s string, out *strings.Builder, active *[]Active) int {
	end := strings.IndexByte(s, '>')
	if end < 0 {
		*active = append(*active, Active{KindHTML, s})
		return len(s)
	}
	body := s[1:end]
	label := body
	if before, after, ok := strings.Cut(body, "|"); ok {
		label = after
		body = before
	}
	kind := KindHTML
	switch {
	case strings.HasPrefix(body, "!date"):
		kind = KindDate
	case strings.HasPrefix(body, "!"):
		kind = KindBroadcast
	case strings.HasPrefix(body, "@"):
		kind = KindMention
	case strings.HasPrefix(body, "#"):
		kind = KindChannelLink
	case strings.Contains(body, "://") || strings.HasPrefix(body, "mailto:") || strings.HasPrefix(body, "tel:") || strings.HasPrefix(body, "javascript:"):
		kind = KindLink
	}
	*active = append(*active, Active{kind, s[:end+1]})
	out.WriteString(label)
	return end + 1
}

// code reads a code span or a fenced block at the start of s, and shows its text as it is. In Slack's mrkdwn
// a token in it is reported too: that Slack leaves "<@U1>" in a code span alone is not known
// (UNCONFIRMED(WP-04)), and an adapter must not rely on it. CommonMark's code is literal.
func code(s string, d dialect, out *strings.Builder, active *[]Active) int {
	ticks := 1
	for ticks < len(s) && s[ticks] == '`' {
		ticks++
	}
	fence := strings.Repeat("`", ticks)
	end := strings.Index(s[ticks:], fence)
	if end < 0 {
		out.WriteString(s[:ticks]) // no closing fence: the ticks are text
		return ticks
	}
	*active = append(*active, Active{KindCode, s[:ticks+end+ticks]})
	if d.codeTokens {
		for _, a := range scan(s[ticks:ticks+end], d).Active {
			if !a.Kind.Style() {
				*active = append(*active, a)
			}
		}
	}
	out.WriteString(s[ticks : ticks+end])
	return ticks + end + ticks
}

// emphasis reads a pair of the delimiter at s[i] (one or two of it) that opens at i, and returns the length
// of the whole span and its inner text. An opening delimiter is followed by a non-space and, for "_",
// preceded by a non-letter; a closing one is preceded by a non-space.
func emphasis(s string, i int) (n int, inner string) {
	delim := s[i]
	width := 1
	if i+1 < len(s) && s[i+1] == delim {
		width = 2
	}
	if i+width >= len(s) || s[i+width] == ' ' || s[i+width] == '\n' || (delim == '_' && !wordStart(s, i)) {
		return 0, ""
	}
	closer := strings.Repeat(string(delim), width)
	for j := i + width + 1; j+width <= len(s); j++ {
		if s[j] == '\n' {
			return 0, ""
		}
		if strings.HasPrefix(s[j:], closer) && s[j-1] != ' ' && s[j-1] != '\\' {
			after := j + width
			if delim == '_' && after < len(s) {
				if r, _ := utf8.DecodeRuneInString(s[after:]); unicode.IsLetter(r) || unicode.IsDigit(r) {
					continue
				}
			}
			return after - i, s[i+width : j]
		}
	}
	return 0, ""
}
