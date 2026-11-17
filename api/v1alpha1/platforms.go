package v1alpha1

import (
	"fmt"
	"regexp"
)

// The chat IDs of a Binding (spec D42) are checked in two places. The schema and Validate hold each ID field to a length cap and
// to chatIDChars, a character set that every platform's IDs are written in, and nothing more: which shape an ID must have depends on
// the platform of the ChatConnection it names, and on a web connection on its profile, which neither the schema nor Validate reads.
// The broker checks the shape (PlatformMismatches, with the ChatPlatform of the connection's spec) in two places where it has the
// connection: when the custom resource source reads the Binding (config.RefusedOnPlatform, which refuses that generation as a failed
// Validate does), and when it resolves the Binding. A Slack channel's name, such as general, is refused there, with
// InvalidPlatformID, and not when it is applied.
//
// chatIDChars is printable ASCII but the space and ':'. ':' is left out because chat.MessageID joins the parts of a message's ID
// with it; white space and control characters because no platform's ID holds one and a person cannot see one; and every character
// outside ASCII, so that no ID holds an invisible character, a character that turns the direction of the text, or a letter that
// looks like an ASCII one, which a pattern of white space and control characters would let through. The rest of ASCII's punctuation
// is kept, since each platform's shape is checked at resolution.
// ':' is kept out for now, and Teams will need it: its channel IDs look like 19:…@thread.tacv2 and its Bot Framework
// user IDs like 29:… (Microsoft's documentation, unverified here). Taking it then needs the part of a message's ID that the ID
// gives to stay free of ':', or chat.MessageID to escape it; and widening the set then is a loosening, which D42 does not count as
// breaking. Its literal must equal the Pattern marker of each of the four fields
// (TestSchemaPatternsEqualTheGoLiterals), and each platform's shape takes only IDs that it takes
// (TestEveryPlatformsShapesAreWithinTheSchema).
var chatIDChars = regexp.MustCompile(`^[!-9;-~]+$`)

// Slack's shapes, which a Binding of a Slack connection is held to when the broker reads it with its connection. maxSlackIDLen is the
// length cap that the schema had for a Slack channel, sender and user group before it took other platforms' IDs; Slack's IDs are 9 to
// 11 characters (unverified: not readable in this repository). A Slack user ID is bounded by its pattern, to 21 characters.
var (
	slackChannelID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)
	slackBotID     = regexp.MustCompile(`^B[A-Z0-9]+$`)
	slackUserID    = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)
	slackGroupID   = regexp.MustCompile(`^S[A-Z0-9]+$`)
)

// The kourier-v1 profile's shape, of every ID it carries: an opaque ID, which the sender chooses and Agent Kourier reads as it is, of
// letters, digits, '.', '_' and '-', that starts with a letter or a digit. ':' is left out, as chatIDChars leaves it out.
// maxKourierV1IDLen is its cap: 64 bytes holds a UUID (36), a ULID (26) or a SHA-256 in hex (64),
// and keeps an ID short enough for the messages that quote it.
var kourierV1ID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const (
	maxSlackIDLen     = 32
	maxSlackUserIDLen = 21
	maxKourierV1IDLen = 64
)

// IDShape is the form that one kind of chat ID has on one platform: a pattern, a length cap in bytes, and what to write instead. The
// zero IDShape is a kind of ID that the platform does not take: it matches nothing, and a Binding that names one is refused with
// UnsupportedOnPlatform (PlatformMismatches).
type IDShape struct {
	pattern *regexp.Regexp
	maxLen  int
	// What names the ID, for a message: "a channel ID".
	What string
	// Fix says what to write instead, for a message: "use the channel's ID, such as C0123456789".
	Fix string
}

// Matches reports whether id has the shape. Nothing has the zero shape.
func (s IDShape) Matches(id string) bool {
	return s.pattern != nil && len(id) <= s.maxLen && s.pattern.MatchString(id)
}

// Taken reports whether the platform takes the kind of ID at all: false for the zero shape.
func (s IDShape) Taken() bool { return s.pattern != nil }

// ChatPlatform is what a Binding may name and use on one chat platform: the shape of each chat ID, and the features of a Binding that
// the platform serves. ChatPlatformOf returns it. A feature is a field that is false here when the platform cannot serve it, and an ID
// a shape that is zero when the platform takes none, so that a platform added without a word on either does not serve it.
type ChatPlatform struct {
	// Name is the platform's display name, as a message writes it: Slack, or web (kourier-v1 profile).
	Name string
	// ActorName is the name of the platform that the people who write in its conversations are on, as the agent reads it in front of
	// each message (the dialects' attribution): Slack, or web.
	ActorName string
	// Channel is the shape of spec.chat.channel, Sender of a botMessage trigger's from.id, User of a mention's user, and Group of an
	// entry of spec.interactions.approverGroups. Every platform takes a channel.
	Channel, Sender, User, Group IDShape
	// AskUser says whether the platform can put an agent's question to a person and take the answer (spec.interactions.askUser).
	// Slack asks with buttons.
	AskUser bool
	// ToolApprovals says whether the platform can ask a person to approve a tool call and take the answer
	// (spec.interactions.toolApprovals). Slack asks with buttons.
	ToolApprovals bool
	// Feedback says whether the platform can show the useful and not-useful buttons and take a press (spec.feedback.enabled).
	Feedback bool
}

// webProfiles returns the profiles of the web platform, in the order of the Enum marker of spec.web.profile, which
// TestWebProfilesFollowTheEnum holds it to.
func webProfiles() []WebProfile { return []WebProfile{WebProfileKourierV1} }

// ChatPlatformOf returns what a Binding may name and use on the platform of a ChatConnection with spec, and what the connection names:
// spec.platform, and on a web connection spec.web.profile. It returns false for a platform or a profile that ChatConnection's Validate
// refuses. It is the one table of what each platform takes: a platform or a profile is one more case here. It is a func and not a map,
// so that there is no state to change.
func ChatPlatformOf(spec ChatConnectionSpec) (ChatPlatform, bool) {
	switch spec.Platform {
	case PlatformSlack:
		return ChatPlatform{
			Name: "Slack", ActorName: "Slack",
			Channel: IDShape{pattern: slackChannelID, maxLen: maxSlackIDLen, What: "a channel ID",
				Fix: "use the channel's ID, such as C0123456789 (G… for an older private channel), not its name"},
			Sender: IDShape{pattern: slackBotID, maxLen: maxSlackIDLen, What: "a bot ID",
				Fix: "use the bot's ID, such as B0123456789, not its name"},
			User: IDShape{pattern: slackUserID, maxLen: maxSlackUserIDLen, What: "a user ID",
				Fix: "use the person's user ID, such as U0123456789"},
			Group: IDShape{pattern: slackGroupID, maxLen: maxSlackIDLen, What: "a user group ID",
				Fix: "use the user group's ID, such as S0123456789"},
			AskUser: true, ToolApprovals: true, Feedback: true,
		}, true
	case PlatformWeb:
		if spec.Web == nil {
			return ChatPlatform{}, false
		}
		switch spec.Web.Profile { //nolint:gocritic // one case for each web profile: another profile is one more case
		case WebProfileKourierV1:
			// The profile posts the replies, and has no buttons and no way to take an answer: questions, tool approvals and
			// feedback are not served. Each can be loosened later, which no stored Binding notices.
			const opaque = "letters, digits, '.', '_' and '-', starting with a letter or a digit"
			return ChatPlatform{
				Name: "web (kourier-v1 profile)", ActorName: "web",
				Channel: IDShape{pattern: kourierV1ID, maxLen: maxKourierV1IDLen, What: "a channel ID",
					Fix: "use the channel's ID as the sender sends it: " + opaque + ", such as ch-1"},
				Sender: IDShape{pattern: kourierV1ID, maxLen: maxKourierV1IDLen, What: "a sender ID",
					Fix: "use the sender's ID as the sender sends it: " + opaque},
				User: IDShape{pattern: kourierV1ID, maxLen: maxKourierV1IDLen, What: "a user ID",
					Fix: "use the person's ID as the sender sends it: " + opaque},
				Group: IDShape{pattern: kourierV1ID, maxLen: maxKourierV1IDLen, What: "a group ID",
					Fix: "use the group's ID as the sender sends it: " + opaque},
			}, true
		}
	}
	return ChatPlatform{}, false
}

// PlatformMismatch is one thing a Binding names or uses that the platform of its connection does not take.
type PlatformMismatch struct {
	// Path is the field, as Validate names it: spec.chat.channel.
	Path string
	// Unsupported says that the field asks for a feature that the platform does not serve. Otherwise the field holds an ID that
	// does not have the platform's shape.
	Unsupported bool
	// Problem says what is wrong, without the platform: `"general" is not a channel ID`, "is 33 bytes, longer than the 32 byte
	// limit of a channel ID", or "tool approvals are not supported".
	Problem string
	// Fix says what to do about it.
	Fix string
}

// PlatformMismatches returns what s names or uses that the platform p does not take, in the order of the fields: each chat ID that
// does not have p's shape, each kind of chat ID that p takes none of, and each feature that p does not serve. Call it on a spec that
// passed Validate, which has held each ID to chatIDChars and its length cap.
func (s *BindingSpec) PlatformMismatches(p ChatPlatform) []PlatformMismatch {
	var out []PlatformMismatch
	id := func(path, value string, shape IDShape) {
		switch {
		case !shape.Taken():
			out = append(out, PlatformMismatch{Path: path, Unsupported: true, Problem: "an ID of this kind is not supported",
				Fix: "remove it: the platform takes no such ID"})
		case !shape.pattern.MatchString(value):
			out = append(out, PlatformMismatch{Path: path, Problem: fmt.Sprintf("%q is not %s", value, shape.What), Fix: shape.Fix})
		case len(value) > shape.maxLen:
			// The shape is the platform's and only the length is not: the message says the length, as Validate's cap does, and
			// does not quote a value that may be long.
			out = append(out, PlatformMismatch{Path: path, Fix: shape.Fix,
				Problem: fmt.Sprintf("is %d bytes, longer than the %d byte limit of %s", len(value), shape.maxLen, shape.What)})
		}
	}
	unsupported := func(path, problem, fix string) {
		out = append(out, PlatformMismatch{Path: path, Unsupported: true, Problem: problem, Fix: fix})
	}
	id("spec.chat.channel", s.Chat.Channel, p.Channel)
	for i, t := range s.Chat.Triggers {
		if t.Type == TriggerBotMessage && t.From != nil {
			id(fmt.Sprintf("spec.chat.triggers[%d].from.id", i), t.From.ID, p.Sender)
		}
	}
	for i, m := range s.Chat.Mentions {
		id(fmt.Sprintf("spec.chat.mentions[%d].user", i), m.User, p.User)
	}
	if s.Interactions.AskUser && !p.AskUser {
		unsupported("spec.interactions.askUser", "questions to a person are not supported", "set it to false")
	}
	if s.Interactions.ToolApprovals && !p.ToolApprovals {
		fix := "set it to false"
		if len(s.Interactions.ApproverGroups) > 0 {
			fix += " and remove spec.interactions.approverGroups, which requires it"
		}
		unsupported("spec.interactions.toolApprovals", "tool approvals are not supported", fix)
	}
	for i, g := range s.Interactions.ApproverGroups {
		id(fmt.Sprintf("spec.interactions.approverGroups[%d]", i), g, p.Group)
	}
	if s.Feedback.Enabled && !p.Feedback {
		unsupported("spec.feedback.enabled", "feedback buttons are not supported", "set it to false")
	}
	return out
}
