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
// name, Agent Kourier posts a message of its own that mentions the person config names (spec D48). The reply itself is not
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

	past []string // the users already counted in Capped
}

// Mentions reads body, the text of a turn as it is shown, for the names in declared, and notifies at most limit people. A
// name is an "@" that follows no word, path, query value or other "@", then letters, digits and hyphens, in any case, and then
// no more of them and none of what goes on as a package scope, a host, an address or a port: a "/", an "@", a "." and a name
// character, a ":" and a digit. A letter of any alphabet is a letter here, so "@josé" does not name "jos". Code is left out:
// what is in a fence or a span is quoted, not said, so a log line that holds a name notifies nobody. What is code is read as the
// Slack adapter writes it (internal/chat/slack/mdtext.go): any fence line opens or closes a block, whatever its marks, a
// block that never closes runs to the end, and a span closes at the next run of backticks or not at all. A pipe table, which
// that adapter shows as code, is read as prose: a name in a table is said.
// A place where the two readings still differ can only notify a declared person or not, within the cap. With nothing
// declared there is nothing to scan for, and nothing is counted.
func Mentions(body string, declared []v1alpha1.ChatMention, limit int) MentionScan {
	var scan MentionScan
	if len(declared) == 0 {
		return scan
	}
	code := false
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\r\n")
		marks, rest := fenceLine(line)
		switch {
		case marks != "" && !code && closesOnItsLine(rest, marks):
		case marks != "":
			code = !code
		case !code:
			scan.line(line, declared, limit)
		}
	}
	return scan
}

// fenceLine is the marks of a fence, if line is a fence line (three or more backticks or tildes after its white space), and
// what follows them. marks is "" if it is not one.
func fenceLine(line string) (marks, rest string) {
	s := strings.TrimLeft(line, " \t")
	if s == "" || (s[0] != '`' && s[0] != '~') {
		return "", ""
	}
	n := runOf(s, s[0])
	if n < 3 {
		return "", ""
	}
	return s[:n], s[n:]
}

// closesOnItsLine says whether what follows an opening fence holds its closing marks: a block of one line.
func closesOnItsLine(rest, marks string) bool { return strings.Contains(rest, marks) }

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
			word, end := mentionWord(line, i)
			if word != "" {
				scan.name(word, declared, limit)
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
// to read on. Only A to Z are lowered: a declared name is ASCII, and a letter of another alphabet that lowers into ASCII is not
// the name it looks like.
func mentionWord(line string, at int) (word string, next int) {
	if before, _ := utf8.DecodeLastRuneInString(line[:at]); at > 0 && (isNameRune(before) || strings.ContainsRune("_./@=", before)) {
		return "", at + 1
	}
	end := at + 1
	for end < len(line) {
		r, size := utf8.DecodeRuneInString(line[end:])
		if !isNameRune(r) && r != '_' {
			break
		}
		end += size
	}
	word = line[at+1 : end]
	if word == "" || word[0] == '-' || (word[0] >= '0' && word[0] <= '9') || goesOn(line[end:]) {
		return "", at + 1
	}
	return strings.Map(lowerASCII, word), end
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
	next, _ := utf8.DecodeRuneInString(rest[1:])
	switch rest[0] {
	case '/', '@':
		return true
	case '.':
		return len(rest) > 1 && isNameRune(next)
	case ':':
		return len(rest) > 1 && next >= '0' && next <= '9'
	}
	return false
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

// MentionMessage is the message that notifies the people made: one line for each. It holds no text of the agent's, and only
// mentions, because a platform may not notify anyone of a mention beside a body (chat.Mention).
func MentionMessage(made []v1alpha1.ChatMention) chat.Message {
	parts := make([]chat.Part, len(made))
	names := make([]string, len(made))
	for i, m := range made {
		parts[i] = chat.Mention{User: m.User, Text: "was named as " + m.Name + " in the reply above."}
		names[i] = m.Name
	}
	return chat.Message{Fallback: "The agent named " + strings.Join(names, ", ") + " in this thread.", Parts: parts}
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
