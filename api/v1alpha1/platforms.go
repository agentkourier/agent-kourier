package v1alpha1

import (
	"fmt"
	"regexp"
)

// The chat IDs of a Binding (spec D42) are checked in two places. The schema and Validate hold each ID field to the union of the
// shapes of the platforms the API knows, so that a value that is no platform's ID, such as the channel name general, is refused when
// it is applied. Which of those shapes it must have depends on the platform of the ChatConnection it names, which neither the schema
// nor Validate reads. The broker checks it (PlatformMismatches, with the platform's ChatPlatform) in two places where it has the
// connection: when the custom resource source reads the Binding (config.RefusedOnPlatform, which refuses that generation as a
// failed Validate does), and when it resolves the Binding.
//
// The union holds Slack's shapes and GitHub's: a repository (owner/name) for a channel, a login (octocat, or an app's
// dependabot[bot]) for a sender or a user, and a team (org/team-slug) for an approver group. GitHub's shapes are in the union before
// a GitHub platform is (spec.platform takes only slack), so that the platform can be added without loosening the schema again. They
// follow GitHub's rules as GitHub documents them, which are not read in this repository: an owner of at most 39 letters, digits and
// single hyphens that neither starts nor ends with a hyphen, and a repository name of at most 100 letters, digits, '.', '_' or '-'.
// The team slug takes the repository name's characters. The patterns bound the characters, not the owner's length: the length
// caps are maxChatIDLen and maxMentionUserLen.
//
// Each literal must equal its field's Pattern marker (TestSchemaPatternsEqualTheGoLiterals).
var (
	// channelID is ChatBinding.Channel's: a Slack channel or a repository.
	channelID = regexp.MustCompile(`^(?:[CG][A-Z0-9]+|[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}/[A-Za-z0-9._-]{1,100})$`)
	// senderID is BotSource.ID's: a Slack bot or a login.
	senderID = regexp.MustCompile(`^(?:B[A-Z0-9]+|[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}(?:\[bot\])?)$`)
	// mentionUser is ChatMention.User's: a Slack user or a login.
	mentionUser = regexp.MustCompile(`^(?:[UW][A-Z0-9]{2,20}|[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}(?:\[bot\])?)$`)
	// approverGroupID is an entry of Interactions.ApproverGroups: a Slack user group or a team.
	approverGroupID = regexp.MustCompile(`^(?:S[A-Z0-9]+|[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}/[A-Za-z0-9._-]{1,100})$`)
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

const (
	maxSlackIDLen     = 32
	maxSlackUserIDLen = 21
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
	// ToolApprovals says whether the platform can ask a person to approve a tool call and take the answer
	// (spec.interactions.toolApprovals). Slack asks with buttons.
	ToolApprovals bool
}

// ChatPlatformOf returns what a Binding may name and use on the platform that a ChatConnection's spec.platform names, and false
// for a platform that ChatConnection's Validate refuses. It is the one table of what a Binding may name and use on each platform:
// a second platform is one more case here. It is a func and not a map, so that there is no state to change.
func ChatPlatformOf(platform string) (ChatPlatform, bool) {
	if platform != PlatformSlack {
		return ChatPlatform{}, false
	}
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
		ToolApprovals: true,
	}, true
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
// to the union of the platforms' shapes and their length caps.
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
	id("spec.chat.channel", s.Chat.Channel, p.Channel)
	for i, t := range s.Chat.Triggers {
		if t.Type == TriggerBotMessage && t.From != nil {
			id(fmt.Sprintf("spec.chat.triggers[%d].from.id", i), t.From.ID, p.Sender)
		}
	}
	for i, m := range s.Chat.Mentions {
		id(fmt.Sprintf("spec.chat.mentions[%d].user", i), m.User, p.User)
	}
	if s.Interactions.ToolApprovals && !p.ToolApprovals {
		fix := "set it to false"
		if len(s.Interactions.ApproverGroups) > 0 {
			fix += " and remove spec.interactions.approverGroups, which requires it"
		}
		out = append(out, PlatformMismatch{Path: "spec.interactions.toolApprovals", Unsupported: true, Problem: "tool approvals are not supported", Fix: fix})
	}
	for i, g := range s.Interactions.ApproverGroups {
		id(fmt.Sprintf("spec.interactions.approverGroups[%d]", i), g, p.Group)
	}
	return out
}
