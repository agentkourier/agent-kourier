package chattest

import (
	"regexp"
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
	KindLink        Kind = "link"         // one whose label the author chooses: <https://x|y>, [y](https://x)
	KindImage       Kind = "image"        // ![alt](https://x)
	KindHTML        Kind = "html"         // a tag, or markup the platform's own parser would take
	KindDate        Kind = "date"         // <!date^...>
	KindAutolink    Kind = "autolink"     // a bare URL the platform makes into a link showing its own address

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
// has to say what that markup does.
type Shown struct {
	Displayed string
	Active    []Active
}

// Scan is the reference reading of text on the wire in a Slack-and-CommonMark style, for an adapter
// whose wire format is text with markup (and for a test double that has one): entities (&amp; &lt; &gt;)
// are the characters themselves, and a raw "<" starts a token; a backslash escapes the ASCII punctuation
// after it; and *, _, ~ and ` pairs, [text](url) links, ![alt](url) images, bare URLs, and a line that
// starts with #, > or a list marker are constructs. The displayed text is what is left once the markup is
// taken out and the entities are read; a token shows its label.
//
// It reads what a careful platform would, and no less than the first reading of text it can: an adapter
// is checked against it, so it errs towards finding a construct. The adapter's own tests, against its
// platform, are the last word.
func Scan(wire string) Shown {
	var out strings.Builder
	var active []Active
	atLineStart := true
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
		case r == '\\' && i+1 < len(wire) && isPunct(wire[i+1]):
			out.WriteByte(wire[i+1])
			i += 2
		case r == '&':
			i += entity(wire[i:], &out)
		case r == '<':
			n := token(wire[i:], &out, &active)
			i += n
		case r == '`':
			i += code(wire[i:], &out, &active)
		case r == '!' && strings.HasPrefix(wire[i:], "!["):
			if text, url, n, ok := link(wire[i+1:]); ok {
				active = append(active, Active{KindImage, "![" + text + "](" + url + ")"})
				out.WriteString(text)
				i += 1 + n
			} else {
				out.WriteRune(r)
				i += size
			}
		case r == '[':
			if text, url, n, ok := link(wire[i:]); ok {
				active = append(active, Active{KindLink, "[" + text + "](" + url + ")"})
				out.WriteString(text)
				i += n
			} else {
				out.WriteRune(r)
				i += size
			}
		case r == '*' || r == '_' || r == '~':
			if n, inner := emphasis(wire, i); n > 0 {
				active = append(active, Active{KindEmphasis, wire[i : i+n]})
				out.WriteString(Scan(inner).Displayed)
				i += n
			} else {
				out.WriteRune(r)
				i += size
			}
		case (r == 'h' || r == 'w') && wordStart(wire, i) && autolink.MatchString(wire[i:]):
			url := autolink.FindString(wire[i:])
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
	autolink      = regexp.MustCompile(`^(?:https?://|www\.)[^\s<>]+`)
	headingMarker = regexp.MustCompile(`^#{1,6}(?: |$)`)
	listMarker    = regexp.MustCompile(`^(?:[-*+]|\d{1,9}[.)])(?: |$)`)
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

// lineMarker returns the length of a heading, quote or list marker at the start of s, and its kind.
func lineMarker(s string) (int, Kind) {
	j := 0
	for j < len(s) && s[j] == ' ' && j < 3 {
		j++
	}
	rest := s[j:]
	switch {
	case strings.HasPrefix(rest, "> ") || rest == ">":
		return j + 1, KindQuote
	case headingMarker.MatchString(rest):
		return j + strings.IndexByte(rest+" ", ' '), KindHeading
	case listMarker.MatchString(rest):
		return j + strings.IndexByte(rest+" ", ' '), KindList
	}
	return 0, ""
}

// entity writes the character that the entity at the start of s stands for, or "&" if it is not one, and
// returns how much of s it used.
func entity(s string, out *strings.Builder) int {
	for ent, ch := range map[string]string{"&amp;": "&", "&lt;": "<", "&gt;": ">"} {
		if strings.HasPrefix(s, ent) {
			out.WriteString(ch)
			return len(ent)
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

// code reads a code span or a fenced block at the start of s, and shows its text as it is.
func code(s string, out *strings.Builder, active *[]Active) int {
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
	out.WriteString(s[ticks : ticks+end])
	return ticks + end + ticks
}

// link reads "[text](url)" at the start of s.
func link(s string) (text, url string, n int, ok bool) {
	mid := strings.Index(s, "](")
	if !strings.HasPrefix(s, "[") || mid < 0 || strings.ContainsAny(s[1:mid], "[\n") {
		return "", "", 0, false
	}
	end := strings.IndexByte(s[mid:], ')')
	if end < 0 {
		return "", "", 0, false
	}
	return s[1:mid], s[mid+2 : mid+end], mid + end + 1, true
}

// emphasis reads a pair of the delimiter at s[i] (one or two of it) that opens at i, and returns the length
// of the whole span and its inner text. An opening delimiter is followed by a non-space and, for "_",
// preceded by a non-letter; a closing one is preceded by a non-space.
func emphasis(s string, i int) (n int, inner string) {
	d := s[i]
	width := 1
	if i+1 < len(s) && s[i+1] == d {
		width = 2
	}
	if i+width >= len(s) || s[i+width] == ' ' || s[i+width] == '\n' || (d == '_' && !wordStart(s, i)) {
		return 0, ""
	}
	delim := strings.Repeat(string(d), width)
	for j := i + width + 1; j+width <= len(s); j++ {
		if s[j] == '\n' {
			return 0, ""
		}
		if strings.HasPrefix(s[j:], delim) && s[j-1] != ' ' && s[j-1] != '\\' {
			after := j + width
			if d == '_' && after < len(s) {
				if r, _ := utf8.DecodeRuneInString(s[after:]); unicode.IsLetter(r) || unicode.IsDigit(r) {
					continue
				}
			}
			return after - i, s[i+width : j]
		}
	}
	return 0, ""
}
