// Package chat is Agent Kourier's chat-neutral model: what a conversation platform has to be for Agent Kourier to
// run on it. A platform (Slack today) is one implementation of Platform, in its own package, and no
// session, interact, render, outbox or store code names it (spec 13: adding a platform requires
// implementing one interface).
//
// # The security contract (spec 8.4, R2)
//
// Everything the agent, a person, a tool or an alert wrote is untrusted, and reaches a platform through
// this model. The contract has three rules, and every adapter must keep all of them and pass
// chattest.Conformance before it ships.
//
//  1. The model carries literal text and nothing else. A Header, Context, Note, button label, hint,
//     question, choice, sub-agent name, tool name, argument, step title, step detail, answer or Fallback
//     holds characters, never markup: "<!channel>", "<@U1>", "<https://x|y>", "[a](b)", "*b*" and "<b>"
//     are text there, and an adapter must show them as text. There is no field in which a caller can put a
//     platform's markup (no Blocks, no Native, no raw JSON), so nothing can bypass an adapter's escaping.
//  2. Body.Markdown is the one rich field, and it is untrusted too: a CommonMark subset in which an
//     adapter may allow emphasis, code and lists, and must not allow a mention, a broadcast, a link, an
//     image, raw HTML or a date, whatever the platform's own syntax for them is.
//  3. Each adapter neutralises markup for its own platform, and makes what it sends inert: no parse, no
//     unfurl, no link names, no hidden or reordering characters (Clean), every field cut at the platform's
//     limit (Caps.Limit) with Truncated, and a single-line field on one line.
//
// What is left to this package's callers is what no platform differs on: redaction of secrets,
// truncation policy for what to show, and the fixed texts Agent Kourier wrote itself.
package chat

// Ref locates a message. Only the adapter that made it reads its parts: Channel and ID mean what the
// platform makes of them (for Slack, the channel ID and the message's ts). A Ref is comparable, and is
// kept in the store as a string by MessageID or by the fields.
type Ref struct {
	// Platform names the platform that made the Ref, such as "slack".
	Platform string
	// Conn is the ChatConnection it was made on: "namespace/name".
	Conn string
	// Channel is the conversation, and ID the message in it.
	Channel, ID string
	// Thread is the message's thread, or "" if it is not in one.
	Thread string
}

// Target is where a post goes: a conversation, and a thread in it if there is one.
type Target struct {
	// Conn is the ChatConnection, "namespace/name".
	Conn string
	// Channel is the conversation. Thread is the thread's root message, or "".
	Channel, Thread string
}

// User is a person as the platform knows them.
type User struct {
	ID, Name, Email string
}

// Where is the place in a message that a piece of text sits, which decides how an adapter must treat it:
// what limit it is cut at, and whether it may be styled.
type Where int

// The places text can be. A Line place is one line, with no newline in it; a Text place may have several
// lines; Rich is Body, whose markdown may be styled.
const (
	WhereFallback   Where = iota // Message.Fallback, and the fixed text of a notice
	WhereHeader                  // Header.Text
	WhereBody                    // Body.Markdown: the one rich place
	WhereContext                 // Context.Text
	WhereNote                    // Note.Text
	WhereToolName                // ToolLine.Name, and Tool.Name
	WhereToolArg                 // ToolLine.Summary
	WhereButton                  // Button.Label
	WhereHint                    // Prompt.Hint
	WhereSubagent                // Prompt.Subagent
	WhereQuestion                // Question.Text
	WhereChoice                  // one of Question.Choices
	WhereAnswer                  // one of an Ending's answers
	WhereStream                  // the markdown a Stream appends: rich, like Body
	WhereStepTitle               // Step.Title
	WhereStepDetail              // Step.Detail
	WhereUser                    // Ending.By, a user ID Agent Kourier verified, shown as a mention only if it is one
	WhereToolArgs                // Tool.Args, the arguments of a tool awaiting approval: text of several lines
	wherePast                    // one past the last, for loops
)

// AllWheres lists every place, for adapters and tests that visit each.
func AllWheres() []Where {
	w := make([]Where, 0, wherePast)
	for i := Where(0); i < wherePast; i++ {
		w = append(w, i)
	}
	return w
}

// Rich says whether text in w may be styled markdown.
func (w Where) Rich() bool { return w == WhereBody || w == WhereStream }

// Line says whether text in w is one line.
func (w Where) Line() bool {
	switch w {
	case WhereHeader, WhereContext, WhereToolName, WhereToolArg, WhereStepTitle, WhereStepDetail, WhereButton, WhereHint, WhereSubagent, WhereQuestion, WhereChoice, WhereAnswer, WhereUser:
		return true
	}
	return false
}

func (w Where) String() string {
	names := [...]string{"fallback", "header", "body", "context", "note", "tool name", "tool argument", "button", "hint", "sub-agent", "question", "choice", "answer", "stream", "step title", "step detail", "user", "tool arguments"}
	if w < 0 || int(w) >= len(names) {
		return "unknown"
	}
	return names[w]
}

// Caps is what a platform can do and how much it can show. A feature a platform lacks is false, and the
// caller degrades (no Streams: a turn is posted once when it ends).
type Caps struct {
	Streams   bool // Stream works
	Steps     bool // Stream.Step shows progress steps in a stream; needs Streams
	Ephemeral bool // Ephemeral shows a message to one person alone
	Edits     bool // Update works
	Inputs    bool // a Prompt's questions are answered inline; otherwise by a reply
	// MaxText is the most text, in UTF-16 units, that one place shows, and Limits is the same for the
	// places that differ. A place cut to its limit ends in Truncated, which counts.
	MaxText int
	Limits  map[Where]int
}

// Limit is the most UTF-16 units of text the place w shows.
func (c Caps) Limit(w Where) int {
	if n, ok := c.Limits[w]; ok {
		return n
	}
	return c.MaxText
}
