package v1alpha1

import (
	"fmt"
	"regexp"
)

// The chat IDs of a Binding (spec D42) are checked in two places. The schema and Validate hold each ID field to the union of the
// shapes of the platforms the API knows, so that a value that is no platform's ID, such as the channel name general, is refused when
// it is applied. Which of those shapes it must have depends on the platform of the ChatConnection it names, and on a web connection
// on its profile, which neither the schema nor Validate reads. The broker checks it (PlatformMismatches, with the ChatPlatform of the
// connection's spec) in two places where it has the connection: when the custom resource source reads the Binding
// (config.RefusedOnPlatform, which refuses that generation as a failed Validate does), and when it resolves the Binding.
//
// The union holds three platforms' shapes, Slack's first:
//
//   - GitHub's, the web platform's github profile: a repository (owner/name) for a channel, a login (octocat, or an app's
//     dependabot[bot]) for a sender or a user, and a team (org/team-slug) for an approver group. They follow GitHub's rules as
//     GitHub documents them, which are not read in this repository: an owner of at most 39 letters, digits and single hyphens that
//     neither starts nor ends with a hyphen, and a repository name of at most 100 letters, digits, '.', '_' or '-'. The team slug
//     takes the repository name's characters.
//   - The kourier-v1 profile's: an opaque ID, which the sender chooses and Agent Kourier reads as it is, of letters, digits, '.',
//     '_' and '-' that starts with a letter or a digit. A channel or an approver group must have a digit as well, so that a name
//     such as general is still no platform's channel and is refused when it is applied, while every machine ID that we know of (a
//     number, a UUID, a ULID, a Slack ID) has one. A sender and a user need none: the login shape already lets a word through.
//     ':' is left out because chat.MessageID joins the parts of a message's ID with it, and '/' so that the ID is never a
//     repository's or a team's.
//
// The patterns bound the characters and not the length: the length caps of the union are maxChatIDLen and maxMentionUserLen, and
// each platform's are its IDShape's.
//
// Each literal must equal its field's Pattern marker (TestSchemaPatternsEqualTheGoLiterals), and each alternative must be one
// platform's shape (TestEveryPlatformsShapesAreTheAlternativesOfTheUnion).
var (
	// channelID is ChatBinding.Channel's: a Slack channel, a repository or a kourier-v1 channel.
	channelID = regexp.MustCompile(`^(?:[CG][A-Z0-9]+|[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}/[A-Za-z0-9._-]{1,100}|(?:[0-9]|[A-Za-z][A-Za-z._-]*[0-9])[A-Za-z0-9._-]*)$`)
	// senderID is BotSource.ID's: a Slack bot, a login or a kourier-v1 ID.
	senderID = regexp.MustCompile(`^(?:B[A-Z0-9]+|[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}(?:\[bot\])?|[A-Za-z0-9][A-Za-z0-9._-]*)$`)
	// mentionUser is ChatMention.User's: a Slack user, a login or a kourier-v1 ID.
	mentionUser = regexp.MustCompile(`^(?:[UW][A-Z0-9]{2,20}|[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}(?:\[bot\])?|[A-Za-z0-9][A-Za-z0-9._-]*)$`)
	// approverGroupID is an entry of Interactions.ApproverGroups: a Slack user group, a team or a kourier-v1 group.
	approverGroupID = regexp.MustCompile(`^(?:S[A-Z0-9]+|[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}/[A-Za-z0-9._-]{1,100}|(?:[0-9]|[A-Za-z][A-Za-z._-]*[0-9])[A-Za-z0-9._-]*)$`)
)

// Slack's shapes, which a Binding of a Slack connection is held to when the broker reads it with its connection. Each is one
// alternative of the union above, and a test holds it to that. maxSlackIDLen is the length cap that the schema had for a Slack
// channel, sender and user group before it took other platforms' IDs; Slack's IDs are 9 to 11 characters (unverified: not readable
// in this repository). A Slack user ID is bounded by its pattern, to 21 characters.
var (
	slackChannelID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)
	slackBotID     = regexp.MustCompile(`^B[A-Z0-9]+$`)
	slackUserID    = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)
	slackGroupID   = regexp.MustCompile(`^S[A-Z0-9]+$`)
)

// GitHub's shapes, the github profile's, as the union describes them. Their patterns bound their length: a repository or a team
// is at most 39 + 1 + 100 bytes, and a login 39, or 44 with an app's "[bot]".
var (
	githubRepo  = regexp.MustCompile(`^[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}/[A-Za-z0-9._-]{1,100}$`)
	githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}(?:\[bot\])?$`)
)

// The kourier-v1 profile's shapes, as the union describes them: a channel or a group has a digit, a sender or a user need not.
// maxKourierV1IDLen is the cap of each: 64 bytes holds a UUID (36), a ULID (26) or a SHA-256 in hex (64), and keeps an ID short
// enough for the messages that quote it.
var (
	kourierV1ChannelID = regexp.MustCompile(`^(?:[0-9]|[A-Za-z][A-Za-z._-]*[0-9])[A-Za-z0-9._-]*$`)
	kourierV1ID        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

const (
	maxSlackIDLen     = 32
	maxSlackUserIDLen = 21
	maxGitHubRepoLen  = 39 + 1 + 100
	maxGitHubLoginLen = 39 + len("[bot]")
	maxKourierV1IDLen = 64
)

// IDShape is the form that one kind of chat ID has on one platform: a pattern, a length cap in bytes, and what to write instead.
type IDShape struct {
	pattern *regexp.Regexp
	maxLen  int
	// What names the ID, for a message: "a channel ID".
	What string
	// Fix says what to write instead, for a message: "use the channel's ID, such as C0123456789".
	Fix string
}

// Matches reports whether id has the shape.
func (s IDShape) Matches(id string) bool { return len(id) <= s.maxLen && s.pattern.MatchString(id) }

// ChatPlatform is what a Binding may name and use on one chat platform: the shape of each chat ID, and the features of a Binding
// that the platform serves. ChatPlatformOf returns it. A feature is a field that is false here when the platform cannot serve it,
// so that a platform added without a word on a feature does not serve it.
type ChatPlatform struct {
	// Name is the platform's display name, as a message writes it: Slack.
	Name string
	// Channel is the shape of spec.chat.channel, Sender of a botMessage trigger's from.id, User of a mention's user, and Group of an
	// entry of spec.interactions.approverGroups.
	Channel, Sender, User, Group IDShape
	// Replies says whether Agent Kourier posts the agent's replies on the platform: then it serves output live and final, and live
	// is the default. Without it the agent replies itself, and output none, which posts nothing, is the only output and the default
	// (spec.chat.output). Only a platform without replies serves none, as it is the one output that needs it.
	Replies bool
	// AskUser says whether the platform can put an agent's question to a person and take the answer (spec.interactions.askUser).
	// Slack asks with buttons.
	AskUser bool
	// ToolApprovals says whether the platform can ask a person to approve a tool call and take the answer
	// (spec.interactions.toolApprovals). Slack asks with buttons.
	ToolApprovals bool
	// Feedback says whether the platform can show the useful and not-useful buttons and take a press (spec.feedback.enabled).
	Feedback bool
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

// ChatPlatformOf returns what a Binding may name and use on the platform of a ChatConnection with spec: spec.platform, and on a
// web connection spec.web.profile. It returns false for a platform or a profile that ChatConnection's Validate refuses. It is the
// one table of what a Binding may name and use on each platform: a platform or a profile is one more case here. It is a func and
// not a map, so that there is no state to change.
func ChatPlatformOf(spec ChatConnectionSpec) (ChatPlatform, bool) {
	switch spec.Platform {
	case PlatformSlack:
		return ChatPlatform{
			Name: "Slack",
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
			// feedback are not served. Each can be loosened later, which no stored Binding notices.
			const opaque = "letters, digits, '.', '_' and '-'"
			return ChatPlatform{
				Name: "web (kourier-v1 profile)",
				Channel: IDShape{pattern: kourierV1ChannelID, maxLen: maxKourierV1IDLen, What: "a channel ID",
					Fix: "use the channel's ID as the sender sends it: " + opaque + ", with a digit, such as ch-1"},
				Sender: IDShape{pattern: kourierV1ID, maxLen: maxKourierV1IDLen, What: "a sender ID",
					Fix: "use the sender's ID as the sender sends it: " + opaque},
				User: IDShape{pattern: kourierV1ID, maxLen: maxKourierV1IDLen, What: "a user ID",
					Fix: "use the person's ID as the sender sends it: " + opaque},
				Group: IDShape{pattern: kourierV1ChannelID, maxLen: maxKourierV1IDLen, What: "a group ID",
					Fix: "use the group's ID as the sender sends it: " + opaque + ", with a digit"},
				Replies: true,
			}, true
		case WebProfileGitHub:
			// The agent posts on GitHub itself, and Agent Kourier posts nothing: no output but none, and nothing that a post
			// carries (a question, a tool approval, the feedback buttons).
			return ChatPlatform{
				Name: "web (github profile)",
				Channel: IDShape{pattern: githubRepo, maxLen: maxGitHubRepoLen, What: "a repository",
					Fix: "use the repository's owner/name, such as octo-org/hello-world"},
				Sender: IDShape{pattern: githubLogin, maxLen: maxGitHubLoginLen, What: "a login",
					Fix: "use the account's login, such as dependabot[bot] for an app"},
				User: IDShape{pattern: githubLogin, maxLen: maxGitHubLoginLen, What: "a login",
					Fix: "use the person's login, such as octocat"},
				Group: IDShape{pattern: githubRepo, maxLen: maxGitHubRepoLen, What: "a team",
					Fix: "use the team's org/team-slug, such as octo-org/sre"},
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
// does not have p's shape, and each feature that p does not serve. Call it on a spec that passed Validate, which has held each ID
// to the union of the platforms' shapes and their length caps. An output left out is p's default, which p serves.
func (s *BindingSpec) PlatformMismatches(p ChatPlatform) []PlatformMismatch {
	var out []PlatformMismatch
	id := func(path, value string, shape IDShape) {
		switch {
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
