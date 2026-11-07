package render

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// A model picks who to notify only from what config allows: it writes "@name" in its reply, and if the Binding declares the
// name, Agent Kourier posts a message for that person that mentions the user config names (spec D48). The reply itself is not
// changed, so no stream has anything to hold back or take back, and "@name" stays literal text there. A raw user ID or a
// platform's markup in the reply is escaped by the adapter as ever. No broadcast, user group or ID can come of this: what can be
// declared is checked at load (api/v1alpha1), and the only user in a mention is the one config gives.

// MentionScan is what the text of a turn asks of the people a Binding declares.
type MentionScan struct {
	// Made are the people to notify: the declared entries the text names, one for each person however many names the
	// person has, in the order the text first names them, as far as the cap.
	Made []v1alpha1.ChatMention
	// Undeclared counts each "@name" that no entry declares, a typo or a person the model made up, and Capped each declared
	// person past the cap. Each notifies nobody.
	Undeclared, Capped int

	past []string // the users not to count again: those mentioned before, and those past the cap
}

// Mentions reads body, the text of a turn as it is shown, for the names in declared, and notifies at most limit people, of whom
// already, the users the turn has mentioned before, are some. They are not named again, and they count towards limit. A
// name is an "@" that follows no word, path, query value, escape or other "@", then letters, digits and hyphens, in any case, and then
// no more of them and none of what goes on as a package scope, a host, an address or a port: a "/" and more (slashEnds), an "@", a "."
// and a name character, a ":" and a digit. "_" and "~" around a name are emphasis, and a "/" between two names only separates them, as in
// "@sam/@lee". A "/" that goes on makes a path of what follows (pathEnd), and nothing in it is a name. Nothing in what reads as a URL with
// "://" in it is a name (endsURL). A ":" before a name is a label otherwise, as in "Owner:@sam". A letter of any alphabet is a letter here, so
// "@josé" does not name "jos". Code is left out:
// what is in a fence or a span is quoted, not said, so a log line that holds a name notifies nobody. What is code is read as the
// Slack adapter writes it (internal/chat/slack/mdtext.go): a fence line, which has a run of three or more backticks or tildes
// after white space and, outside a block, one list marker or one quote mark (fenceLine has the forms), opens a block, and
// any fence line closes it, whatever else is on it (a mark before a closing run counts only if it is the kind its opener had: fenceLine), what follows a
// closing run is read as a line of its own (a fence line there opens a block), a block that never closes runs to the end, and a span closes at the next run of backticks or not
// at all. A stream whose state the adapter cannot tell (a blind one) is sent with its fences as they came, not as blocks, so a
// name read as code here may show as text there. A pipe table, which that adapter shows as code, is read as prose: a name in a
// table is said. A name that ends where the
// text was cut (chat.Truncated), or in a ".", ":" or "/" just before it, may be the start of something longer, so it is no name.
// A place where the two readings still differ can only notify a declared person or not, within the cap: the adapter counts its limit
// in escaped text, so it can cut sooner than body was. With nothing declared there is nothing to scan for, and nothing is counted.
func Mentions(body string, declared []v1alpha1.ChatMention, limit int, already ...string) MentionScan {
	scan := MentionScan{past: slices.Clone(already)}
	if len(declared) == 0 {
		return scan
	}
	limit -= len(already)
	code, held, opener, reread := false, body, markNone, false
	for held != "" {
		line, _, _ := strings.Cut(held, "\n")
		after := held[len(line):] // the line break, if any, and the lines after it
		ch, run, rest, mk := fenceLine(line, code, opener, reread)
		if run >= 3 {
			held, reread = afterFence(code, ch, run, rest, after)
			opener = mk // what stood before the run of the opener of the block that is open
			if code {
				opener = markNone
			}
			code = !code
			continue
		}
		reread = false
		if !code {
			scan.line(line, declared, limit)
		}
		held = strings.TrimPrefix(after, "\n")
	}
	return scan
}

// mark is what may stand before the run of a fence besides white space.
type mark uint8

const (
	markNone  mark = iota // nothing
	markList              // a list marker: "-", "*" or "+", or one to nine digits and "." or ")", and then white space
	markQuote             // a quote mark: ">" and, if the next character is white space, that one character
)

// fenceLine reads line as the adapter's readFence does: the mark of the run of three or more backticks or tildes after the line's
// white space and, outside a block (code is false), one list marker or one quote mark and white space again, how long the run
// is, what follows it, and what stood before it besides white space. The line is no fence line if run is under 3. So
// "- ```", "1) ```", ">```" and "> ```" are fence lines, and "- - ```", "> > ```", "> - ```" and "-```" are not: a second
// mark, or a marker with no white space after it, makes the line prose. So does a mark, then backticks (not tildes), whose
// next run of backticks is as long as the first, which makes a span: "1. ```make test``` first" is a list item with a code
// span, as a backtick fence's info string has no backtick in CommonMark. That is outside a block only: in one, such a line
// closes it, as any closing line does. The rest of a fence line put back to be read (reread)
// has no marks, as it did not before them.
//
// In a block that a fence line after a list marker opened (opener markList) a line may close it with a list marker before the
// run, and in one that a fence line after a quote mark opened (markQuote) every line first loses a quote mark, if it has one,
// and one character of white space after it. A mark before a closing run counts only if it is the kind its opener had: a block
// opened with white space alone, indented or not, takes only white space there, and a list block takes no quote mark.
func fenceLine(line string, code bool, opener mark, reread bool) (ch byte, run int, rest string, mk mark) {
	if reread {
		opener = markNone // the rest of a fence line put back has no marks: they count at the start of a line of the text only
	}
	if code && opener == markQuote {
		if t := strings.TrimLeftFunc(line, unicode.IsSpace); strings.HasPrefix(t, ">") {
			line = t[1:] // the white space after the mark is trimmed below, as the adapter's readFence drops it
		}
	}
	body := strings.TrimLeftFunc(line, unicode.IsSpace)
	n := 0
	if body != "" && body[0] != '`' && body[0] != '~' {
		switch {
		case body[0] == '>' && !code && !reread:
			n, mk = 1, markQuote
		case (!code && !reread) || opener == markList:
			if n = listMarker(body); n > 0 {
				mk = markList
			}
		}
	}
	body = strings.TrimLeftFunc(body[n:], unicode.IsSpace)
	if body == "" || (body[0] != '`' && body[0] != '~') {
		return 0, 0, body, markNone
	}
	run = runOf(body, body[0])
	rest = body[run:]
	if !code && mk != markNone && body[0] == '`' && run >= 3 && closingRun(rest, run) >= 0 {
		return 0, 0, body, markNone // a code span in a list item or a quote, not a fence line
	}
	return body[0], run, rest, mk
}

// spaceSize is the size of the character of white space s starts with, or 0.
func spaceSize(s string) int {
	if r, size := utf8.DecodeRuneInString(s); size > 0 && unicode.IsSpace(r) {
		return size
	}
	return 0
}

// listMarker is the length of the list marker body starts with, white space after it not counted, or 0: a "-", "*" or "+", or
// one to nine digits and a "." or ")", and then a character of white space.
func listMarker(body string) int {
	n := 0
	switch c := body[0]; {
	case c == '-' || c == '*' || c == '+':
		n = 1
	case c >= '0' && c <= '9':
		for n < len(body) && body[n] >= '0' && body[n] <= '9' {
			n++
		}
		if n > 9 || n == len(body) || (body[n] != '.' && body[n] != ')') {
			return 0
		}
		n++
	default:
		return 0
	}
	if n == len(body) || spaceSize(body[n:]) == 0 {
		return 0
	}
	return n
}

// afterFence is what is left to read after a fence line, with after being the lines that follow it, and it is the adapter's
// mdText.fence: what follows the fence on its line is read as a line of its own. A fence line closes a block whatever else is on
// the line, and the rest is prose. One that opens a block drops a single word as its info string, and otherwise the rest is
// the block's first line, unless it holds a closing run: then it is the block's one line, and a closing fence with what
// followed the run.
//
// It says too whether the first line of what is left is that rest put back, not the start of a line of the text: no mark counts
// in it (fenceLine).
func afterFence(code bool, ch byte, run int, rest, after string) (held string, reread bool) {
	if code {
		return strings.TrimLeftFunc(rest, unicode.IsSpace) + after, true
	}
	if at, n := closingFence(rest, ch, run); at >= 0 {
		return strings.TrimSpace(rest[:at]) + "\n```" + rest[at+n:] + after, true
	}
	if !strings.ContainsFunc(strings.TrimSpace(rest), unicode.IsSpace) {
		return strings.TrimPrefix(after, "\n"), false
	}
	return strings.TrimLeftFunc(rest, unicode.IsSpace) + after, true
}

// closingFence is where in s the first run of at least atLeast of c starts, and how long it is, or -1.
func closingFence(s string, c byte, atLeast int) (at, n int) {
	for i := 0; i < len(s); {
		if s[i] != c {
			i++
			continue
		}
		n := runOf(s[i:], c)
		if n >= atLeast {
			return i, n
		}
		i += n
	}
	return -1, 0
}

// runOf is how many of c s starts with.
func runOf(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// line reads one line of prose: a run of backticks whose next run on the line is the same length is a code span, and is
// skipped.
func (scan *MentionScan) line(line string, declared []v1alpha1.ChatMention, limit int) {
	last := -1 // where the last name ended
	for i := 0; i < len(line); {
		switch line[i] {
		case '`':
			n := runOf(line[i:], '`')
			if end := closingRun(line[i+n:], n); end >= 0 {
				i += n + end + n
			} else {
				i += n
			}
		case '@':
			word, end := mentionWord(line, i, last)
			if word != "" {
				scan.name(word, declared, limit)
				last = end
			}
			i = end
		default:
			i++
		}
	}
}

// closingRun is where in s the next run of backticks starts, if it is n long, or -1.
func closingRun(s string, n int) int {
	at := strings.IndexByte(s, '`')
	if at < 0 || runOf(s[at:], '`') != n {
		return -1
	}
	return at
}

// mentionWord is the name of the "@" at line[at], with A to Z lowered, or "" if it starts none (see Mentions), and where in line
// to read on. last is where the name before it ended. Only A to Z are lowered: a declared name is ASCII, and a letter of another
// alphabet that lowers into ASCII is not the name it looks like.
func mentionWord(line string, at, last int) (word string, next int) {
	before := strings.TrimRight(line[:at], "_~")
	r, _ := utf8.DecodeLastRuneInString(before)
	between := r == '/' && len(before)-1 == last
	if before != "" && !between && (isNameRune(r) || strings.ContainsRune(`./@=?&#%+\`, r) || inURL(before)) {
		return "", at + 1
	}
	end := at + 1
	for end < len(line) {
		r, size := utf8.DecodeRuneInString(line[end:])
		if !isNameRune(r) && r != '_' && r != '~' {
			break
		}
		end += size
	}
	word = strings.TrimRight(line[at+1:end], "_~")
	if word == "" || word[0] == '-' || (word[0] >= '0' && word[0] <= '9') {
		return "", at + 1
	}
	if goesOn(line[end:]) {
		if line[end] == '/' {
			return "", end + pathEnd(line[end:])
		}
		return "", at + 1
	}
	return strings.Map(lowerASCII, word), end
}

// pathEnd is where in s a path ends: at white space, or at a backtick or a "|", so that a code span or a table's next cell after it
// is still read as one.
func pathEnd(s string) int {
	if at := strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '`' || r == '|' }); at >= 0 {
		return at
	}
	return len(s)
}

// inURL says whether s ends inside a URL with "://" in it: whether what follows the last thing that ends a URL holds one.
func inURL(s string) bool {
	return strings.Contains(s[strings.LastIndexFunc(s, endsURL)+1:], "://")
}

// endsURL says whether r ends a URL as text is written: white space, a mark a URL cannot hold, a closing bracket as of a markdown
// link or an aside, or non-ASCII punctuation. A URL can hold a closing bracket, and then what follows it can notify.
func endsURL(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("|<>`)]", r) || (r > unicode.MaxASCII && unicode.IsPunct(r))
}

func lowerASCII(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

// goesOn says whether what follows a name makes it part of something else (see Mentions).
func goesOn(rest string) bool {
	if rest == "" {
		return false
	}
	if strings.TrimLeft(rest, ".:/") == chat.Truncated {
		return true
	}
	next, _ := utf8.DecodeRuneInString(rest[1:])
	switch rest[0] {
	case '/':
		return !slashEnds(rest[1:])
	case '@':
		return true
	case '.':
		return len(rest) > 1 && isNameRune(next)
	case ':':
		return len(rest) > 1 && next >= '0' && next <= '9'
	}
	return false
}

// slashEnds says whether what follows a "/" leaves the name before it whole: nothing, white space, or a second name by the same rule
// that could be declared. Only its characters need checking here, since mentionWord has refused a hyphen or a digit at its start
// (api/v1alpha1 has the whole pattern).
// Anything else makes a package scope, a path or a glob, closing marks too: a name with a "/" after it is rare in prose, and a
// false notification reaches a person.
func slashEnds(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	if s == "" || unicode.IsSpace(r) {
		return true
	}
	s = strings.TrimLeft(s, "_~")
	if !strings.HasPrefix(s, "@") {
		return false
	}
	word, _ := mentionWord(s, 0, -1)
	return word != "" && !strings.ContainsFunc(word, func(r rune) bool {
		return r != '-' && (r < '0' || r > '9') && (r < 'a' || r > 'z')
	})
}

func isNameRune(r rune) bool {
	return r == '-' || unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r)
}

// name counts one name that the text wrote.
func (scan *MentionScan) name(word string, declared []v1alpha1.ChatMention, limit int) {
	at := slices.IndexFunc(declared, func(m v1alpha1.ChatMention) bool { return m.Name == word })
	if at < 0 {
		scan.Undeclared++
		return
	}
	user := declared[at].User
	if slices.Contains(scan.past, user) || slices.ContainsFunc(scan.Made, func(m v1alpha1.ChatMention) bool { return m.User == user }) {
		return
	}
	if len(scan.Made) >= limit {
		scan.Capped++
		scan.past = append(scan.past, user)
		return
	}
	scan.Made = append(scan.Made, declared[at])
}

// MentionMessage is the message that notifies the person made. It holds no text of the agent's, and only a mention, because a
// platform may not notify anyone of a mention beside a body (chat.Mention).
func MentionMessage(m v1alpha1.ChatMention) chat.Message {
	return chat.Message{
		Fallback: "The agent named " + m.Name + " in this thread.",
		Parts:    []chat.Part{chat.Mention{User: m.User, Text: "was named as " + m.Name + " in the reply above."}},
	}
}

// MentionPrompt is the line a session's first prompt ends with on a Binding that declares people, which tells the agent the
// names it may use, or "" on one that declares none. Without it the model would have to guess them. It names no user: the
// agent never supplies an ID.
func MentionPrompt(declared []v1alpha1.ChatMention) string {
	if len(declared) == 0 {
		return ""
	}
	names := make([]string, len(declared))
	for i, m := range declared {
		names[i] = m.Name
	}
	return "To notify someone in this thread, write @name in your reply, outside code. You may use these names: " +
		strings.Join(names, ", ") + ". No other name notifies anyone, and neither does a user ID."
}
