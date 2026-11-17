package v1alpha1

import (
	"fmt"
	"regexp"
	"slices"
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
// is kept, '/' of a repository and '[' and ']' of a GitHub App's login among it, since each platform's shape is checked at
// resolution. ':' is kept out for now, and Teams will need it: its channel IDs look like 19:…@thread.tacv2 and its Bot Framework
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

// GitHub's shapes, the github profile's: a repository (owner/name) for a channel, and a login (octocat, or an app's
// dependabot[bot]) for the connection's identity (spec.web.identity). They follow GitHub's rules as GitHub documents them, which are
// not read in this repository: an owner of letters, digits and single hyphens that neither starts nor ends with a hyphen, and a
// repository name of at most 100 letters, digits, '.', '_' or '-'. The patterns bound the characters and not the length: an owner or
// a login takes at most 39 letters and digits, with a single hyphen between any two of them, so up to 77 bytes. The caps bound the
// length: a repository is at most maxGitHubRepoLen (39 + 1 + 100) bytes, and a login maxGitHubLoginLen (39, and 5 more for an app's
// "[bot]"). GitHub's 39 for the whole owner is not checked, and the caps are looser.
var (
	githubRepo  = regexp.MustCompile(`^[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}/[A-Za-z0-9._-]{1,100}$`)
	githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}(?:\[bot\])?$`)
)

// The kourier-v1 profile's shape, of every ID it carries: an opaque ID, which the sender chooses and Agent Kourier reads as it is, of
// letters, digits, '.', '_' and '-', that starts with a letter or a digit. ':' is left out, as chatIDChars leaves it out, and '/' so
// that the ID is never a repository's. maxKourierV1IDLen is its cap: 64 bytes holds a UUID (36), a ULID (26) or a SHA-256 in hex (64),
// and keeps an ID short enough for the messages that quote it.
var kourierV1ID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const (
	maxSlackIDLen     = 32
	maxSlackUserIDLen = 21
	maxGitHubRepoLen  = 39 + 1 + 100
	maxGitHubLoginLen = 39 + len("[bot]")
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

// ChatPlatform is what a Binding may name and use on one chat platform, and what its ChatConnection names: the shape of each chat ID,
// the features of a Binding that the platform serves, and the shape of the connection's identity. ChatPlatformOf returns it. A feature
// is a field that is false here when the platform cannot serve it, and an ID a shape that is zero when the platform takes none, so that
// a platform added without a word on either does not serve it.
type ChatPlatform struct {
	// Name is the platform's display name, as a message writes it: Slack, or web (github profile).
	Name string
	// ActorName is the name of the platform that the people who write in its conversations are on, as the agent reads it in front of
	// each message ("[GitHub: octocat]", the dialects' attribution): Slack, web, or GitHub.
	ActorName string
	// Channel is the shape of spec.chat.channel, Sender of a botMessage trigger's from.id, User of a mention's user, and Group of an
	// entry of spec.interactions.approverGroups. Every platform takes a channel. A platform without replies takes no sender, user or
	// group, since nothing would use one: it hands on no bot's message, and Agent Kourier posts no mention and asks for no approval
	// there.
	Channel, Sender, User, Group IDShape
	// Identity is the shape of the connection's spec.web.identity, the account the agent works as, which a profile needs that finds
	// the agent's mentions or drops the agent's own events by it: zero on a platform that takes none. A ChatConnection's Validate
	// requires it where it is not zero, and refuses it where it is.
	Identity IDShape
	// Replies says whether Agent Kourier posts the agent's replies on the platform: then it serves output live and final, and live
	// is the default, and a web connection's spec.web.url says where they go. Without it the agent replies itself, and output none,
	// which posts nothing, is the only output and the default (spec.chat.output), and a web connection takes no url. Only a platform
	// without replies serves none, as it is the one output that needs it.
	Replies bool
	// AskUser says whether the platform can put an agent's question to a person and take the answer (spec.interactions.askUser).
	// Slack asks with buttons.
	AskUser bool
	// ToolApprovals says whether the platform can ask a person to approve a tool call and take the answer
	// (spec.interactions.toolApprovals). Slack asks with buttons.
	ToolApprovals bool
	// Feedback says whether the platform can show the useful and not-useful buttons and take a press (spec.feedback.enabled).
	Feedback bool
	// MentionsOnly says that every message the platform hands on is a mention of Agent Kourier in a thread, and none is a bot's
	// message or opens a thread: only the mention trigger starts a turn, so a Binding that lists its triggers lists it
	// (spec.chat.triggers). The other triggers are taken, and match nothing there.
	MentionsOnly bool
}

// DefaultOutput is the output of a Binding that names none: live where Agent Kourier posts the replies, and none where the agent
// replies itself.
func (p ChatPlatform) DefaultOutput() OutputMode {
	if p.Replies {
		return OutputLive
	}
	return OutputNone
}

// servesOutput reports whether the platform serves the output o of a Binding; one left out is the platform's default.
func (p ChatPlatform) servesOutput(o OutputMode) bool {
	switch o {
	case "":
		return true
	case OutputLive, OutputFinal:
		return p.Replies
	case OutputNone:
		return !p.Replies
	}
	return false
}

// webProfiles returns the profiles of the web platform, in the order of the Enum marker of spec.web.profile, which
// TestTheWebRulesFollowThePlatformTable holds it to.
func webProfiles() []WebProfile { return []WebProfile{WebProfileKourierV1, WebProfileGitHub} }

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
			Replies: true, AskUser: true, ToolApprovals: true, Feedback: true,
		}, true
	case PlatformWeb:
		if spec.Web == nil {
			return ChatPlatform{}, false
		}
		switch spec.Web.Profile {
		case WebProfileKourierV1:
			// The profile posts the replies, and has no buttons and no way to take an answer: questions, tool approvals and
			// feedback are not served. Each can be loosened later, which no stored Binding notices. It knows no account of the
			// agent's: a sender sends only what is addressed to Agent Kourier, and never the agent's own replies.
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
				Replies: true,
			}, true
		case WebProfileGitHub:
			// The agent posts on GitHub itself, and Agent Kourier posts nothing: no output but none, nothing that a post carries (a
			// question, a tool approval, the feedback buttons, a mention), and no sender, user or group to name. The connection
			// names the agent's login, by which the profile finds the agent's mentions and drops its own events.
			return ChatPlatform{
				Name: "web (github profile)", ActorName: "GitHub",
				Channel: IDShape{pattern: githubRepo, maxLen: maxGitHubRepoLen, What: "a repository",
					Fix: "use the repository's owner/name, such as octo-org/hello-world"},
				Identity: IDShape{pattern: githubLogin, maxLen: maxGitHubLoginLen, What: "a GitHub login",
					Fix: "use the login the agent posts as: a user's, such as pr-reviewer, or a GitHub App's, such as pr-reviewer[bot]"},
				// Every event the profile hands on is a pull request's, in the pull request's thread, and is a mention.
				MentionsOnly: true,
			}, true
		}
	}
	return ChatPlatform{}, false
}

// DefaultOn fills the fields of s whose default depends on the platform p, which Default leaves unset: spec.chat.output, with
// p.DefaultOutput. A field that is set is kept, whether p serves it or not: PlatformMismatches says which it does not. Resolution
// calls it on the Binding it serves, so that what runs never sees an output left out.
func (s *BindingSpec) DefaultOn(p ChatPlatform) {
	if s.Chat.Output == "" {
		s.Chat.Output = p.DefaultOutput()
	}
}

// PlatformMismatch is one thing a Binding names or uses that the platform of its connection does not take.
type PlatformMismatch struct {
	// Path is the field, as Validate names it: spec.chat.channel.
	Path string
	// Unsupported says that the field asks for a feature that the platform does not serve. Otherwise the field holds an ID that
	// does not have the platform's shape.
	Unsupported bool
	// Problem says what is wrong, without the platform: `"octo/repo" is not a channel ID`, "is 33 bytes, longer than the 32 byte
	// limit of a channel ID", or "tool approvals are not supported".
	Problem string
	// Fix says what to do about it.
	Fix string
}

// PlatformMismatches returns what s names or uses that the platform p does not take, in the order of the fields: each chat ID that
// does not have p's shape, each kind of chat ID that p takes none of, and each feature that p does not serve. Call it on a spec that
// passed Validate, which has held each ID to chatIDChars and its length cap. An output left out is p's default, which p serves.
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
	if o := s.Chat.Output; !p.servesOutput(o) {
		fix := "set it to none, or leave it out: the agent replies itself, and Agent Kourier posts nothing"
		if p.Replies {
			fix = "set it to live or final, or leave it out for live"
		}
		unsupported("spec.chat.output", fmt.Sprintf("%q is not supported", o), fix)
	}
	if p.MentionsOnly && len(s.Chat.Triggers) > 0 && !slices.ContainsFunc(s.Chat.Triggers, func(t Trigger) bool { return t.Type == TriggerMention }) {
		unsupported("spec.chat.triggers", "a list with no mention trigger is not supported",
			"add {type: mention} to it, or leave it out, which is [{type: mention}]: every event the platform hands on is a mention, and no other trigger starts a turn")
	}
	for i, t := range s.Chat.Triggers {
		if t.Type != TriggerBotMessage || t.From == nil {
			continue
		}
		path := fmt.Sprintf("spec.chat.triggers[%d].from.id", i)
		if !p.Sender.Taken() {
			unsupported(path, "a bot's message is not supported", "remove the botMessage trigger: the platform hands on no bot's message")
			continue
		}
		id(path, t.From.ID, p.Sender)
	}
	if len(s.Chat.Mentions) > 0 && !p.User.Taken() {
		unsupported("spec.chat.mentions", "mentions are not supported", "remove it: Agent Kourier posts nothing on the platform, so it notifies nobody")
	} else {
		for i, m := range s.Chat.Mentions {
			id(fmt.Sprintf("spec.chat.mentions[%d].user", i), m.User, p.User)
		}
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
	if len(s.Interactions.ApproverGroups) > 0 && !p.Group.Taken() {
		unsupported("spec.interactions.approverGroups", "approver groups are not supported", "remove it: the platform asks for no tool approvals")
	} else {
		for i, g := range s.Interactions.ApproverGroups {
			id(fmt.Sprintf("spec.interactions.approverGroups[%d]", i), g, p.Group)
		}
	}
	if s.Feedback.Enabled && !p.Feedback {
		unsupported("spec.feedback.enabled", "feedback buttons are not supported", "set it to false")
	}
	return out
}
