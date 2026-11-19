package v1alpha1

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The chat IDs of a Binding (spec D42) are checked in two places. The schema and Validate hold each ID field to a shape and its
// length cap: Slack's, and on a Binding whose triggers are all webhookEvent, the channel to a webhook channel's (webhookChannel), so
// that a Slack channel's name, such as general, is refused when it is applied or loaded. The broker checks the shape again
// (PlatformMismatches, with the ChatPlatform of the connection's spec) where it has the connection: when the custom resource source
// reads the Binding (config.RefusedOnPlatform, which refuses that generation as a failed Validate does), and when it resolves the
// Binding, with the reason InvalidPlatformID. That second check is where a Binding is held to its own connection's platform: a
// webhook channel such as acme/widgets on a Slack connection.
//
// Slack's shapes. The channel's literal must equal the one that the CEL rule of ChatBinding copies, and each other literal the Pattern
// marker of its field (TestCELLiteralsEqualTheBrokersConstants, TestSchemaPatternsEqualTheGoLiterals); maxChatIDLen and
// maxMentionUserLen (validate.go) are their caps. Slack's IDs are 9 to 11 characters (unverified: not readable in this repository).
// slackUserID matches what the Slack adapter makes a mention of.
var (
	slackChannelID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)
	slackBotID     = regexp.MustCompile(`^B[A-Z0-9]+$`)
	slackUserID    = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)
	slackGroupID   = regexp.MustCompile(`^S[A-Z0-9]+$`)
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
	// Name is the platform's display name, as a message writes it: Slack.
	Name string
	// ActorName is the name of the platform that the people who write in its conversations are on, as the agent reads it in front of
	// each message (the dialects' attribution): Slack.
	ActorName string
	// Channel is the shape of spec.chat.channel, Sender of a botMessage trigger's from.id, User of a mention's user, and Group of an
	// entry of spec.interactions.approverGroups. Every platform takes a channel. A platform without replies takes no sender, user or
	// group, since nothing would use one: it hands on no bot's message, and Agent Kourier posts no mention and asks for no approval
	// there.
	Channel, Sender, User, Group IDShape
	// Replies says whether Agent Kourier posts the agent's replies on the platform: then it serves output live and final. Without it
	// the agent posts with its own tools, and output none, which posts nothing, is the only output it serves (spec.chat.output).
	Replies bool
	// AskUser says whether the platform can put an agent's question to a person and take the answer (spec.interactions.askUser).
	// Slack asks with buttons.
	AskUser bool
	// ToolApprovals says whether the platform can ask a person to approve a tool call and take the answer
	// (spec.interactions.toolApprovals). Slack asks with buttons.
	ToolApprovals bool
	// Feedback says whether the platform can show the useful and not-useful buttons and take a press (spec.feedback.enabled).
	Feedback bool
	// Triggers are the trigger types that the platform's Bindings take (spec.chat.triggers[].type), in the order a refusal lists
	// them: Slack takes mention, botMessage and personMessage, and a webhook connection webhookEvent alone. A trigger of any other type
	// is refused with UnsupportedOnPlatform, so a platform added without them takes none.
	Triggers []TriggerType
	// WebhookFields are the fields of the webhook connection (spec.webhook.fields), a copy: what a webhookEvent trigger may match and
	// exclude on, and whose id fields its template may read. Nil on a platform that is not a webhook connection.
	WebhookFields []WebhookField
}

// servesOutput reports whether the platform serves the output o of a Binding. One left out is live, which Default fills in.
func (p ChatPlatform) servesOutput(o OutputMode) bool {
	switch o {
	case "", OutputLive, OutputFinal:
		return p.Replies
	case OutputNone:
		return !p.Replies
	}
	return false
}

// ChatPlatformOf returns what a Binding may name and use on the platform of a ChatConnection with spec, which spec.platform names, and
// on a webhook connection spec.webhook. It returns false for a platform that ChatConnection's Validate refuses, and for a webhook
// connection with no spec.webhook, which it refuses too. It is the one table of what each platform takes: a platform is one more case
// here. It is a func and not a map, so that there is no state to change.
func ChatPlatformOf(spec ChatConnectionSpec) (ChatPlatform, bool) {
	switch spec.Platform {
	case PlatformSlack:
		return ChatPlatform{
			Name: "Slack", ActorName: "Slack",
			Channel: IDShape{pattern: slackChannelID, maxLen: maxChatIDLen, What: "a channel ID",
				Fix: "use the channel's ID, such as C0123456789 (G… for an older private channel), not its name"},
			Sender: IDShape{pattern: slackBotID, maxLen: maxChatIDLen, What: "a bot ID",
				Fix: "use the bot's ID, such as B0123456789, not its name"},
			User: IDShape{pattern: slackUserID, maxLen: maxMentionUserLen, What: "a user ID",
				Fix: "use the person's user ID, such as U0123456789"},
			Group: IDShape{pattern: slackGroupID, maxLen: maxChatIDLen, What: "a user group ID",
				Fix: "use the user group's ID, such as S0123456789"},
			Replies: true, AskUser: true, ToolApprovals: true, Feedback: true,
			Triggers: []TriggerType{TriggerMention, TriggerBotMessage, TriggerPersonMessage},
		}, true
	case PlatformWebhook:
		if spec.Webhook == nil {
			return ChatPlatform{}, false
		}
		// The agent posts on the service with its own tools, and Agent Kourier posts nothing: no output but none, nothing that a
		// post carries (a question, a tool approval, the feedback buttons, a mention), and no sender, user or group to name.
		return ChatPlatform{
			Name: "webhook", ActorName: "webhook",
			Channel: IDShape{pattern: webhookChannel, maxLen: MaxWebhookChannelLen, What: "a webhook channel",
				Fix: "use the channel as the connection reads it from an event, such as acme/widgets or OPS"},
			Triggers: []TriggerType{TriggerWebhookEvent}, WebhookFields: slices.Clone(spec.Webhook.Fields),
		}, true
	}
	return ChatPlatform{}, false
}

// orList writes the trigger types as a message lists them: "webhookEvent", or "mention, botMessage or personMessage".
func orList(types []TriggerType) string {
	s := make([]string, len(types))
	for i, t := range types {
		s[i] = string(t)
	}
	if len(s) < 2 {
		return strings.Join(s, "")
	}
	return strings.Join(s[:len(s)-1], ", ") + " or " + s[len(s)-1]
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
// does not have p's shape, each kind of chat ID that p takes none of, each feature that p does not serve, each trigger of a type that
// p does not take, and on a webhook connection, each field that a webhookEvent trigger names and the connection does not have. Call it
// on a spec that passed Validate, which has held each ID to Slack's shape and its length cap, and the channel of a Binding whose
// triggers are all webhookEvent to a webhook channel's: with Slack's own rules it returns nothing for a Slack Binding.
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
		fix := "set it to live or final, or leave it out for live"
		if !p.Replies {
			fix = "set it to none: on this platform the agent posts with its own tools and Agent Kourier posts nothing (left out, it is live)"
		}
		unsupported("spec.chat.output", fmt.Sprintf("%q is not supported", cmp.Or(o, OutputLive)), fix)
	}
	for i, t := range s.Chat.Triggers {
		at := fmt.Sprintf("spec.chat.triggers[%d]", i)
		switch {
		case !slices.Contains(p.Triggers, t.Type):
			fix := "use a type the platform takes: " + orList(p.Triggers)
			if t.Type == TriggerMention {
				fix += " (a Binding that lists no triggers has [{type: mention}])"
			}
			if len(p.Triggers) == 0 {
				fix = "remove it: the platform takes no triggers"
			}
			unsupported(at+".type", fmt.Sprintf("%q is not supported", t.Type), fix)
		case t.Type == TriggerWebhookEvent:
			out = append(out, webhookTriggerMismatches(at, t, p.WebhookFields)...)
		case t.Type == TriggerBotMessage && t.From != nil:
			id(at+".from.id", t.From.ID, p.Sender)
		}
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
