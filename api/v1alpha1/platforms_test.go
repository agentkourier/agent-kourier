package v1alpha1

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The spec of a connection on each platform that Validate accepts.
var slackSpec = ChatConnectionSpec{Platform: PlatformSlack}

// platformOf is the rules of the connection with spec, which the tests below cannot do without.
func platformOf(t *testing.T, spec ChatConnectionSpec) ChatPlatform {
	t.Helper()
	p, ok := ChatPlatformOf(spec)
	if !ok {
		t.Fatalf("ChatPlatformOf(%+v) has no rules", spec)
	}
	return p
}

// slack is Slack's rules.
func slack(t *testing.T) ChatPlatform { t.Helper(); return platformOf(t, slackSpec) }

// shapes returns each kind of chat ID that p takes, by the name of its field in the schema, with the schema's cap of the field.
func shapes(p ChatPlatform) map[string]struct {
	shape IDShape
	cap   int
} {
	out := map[string]struct {
		shape IDShape
		cap   int
	}{}
	for name, f := range map[string]struct {
		shape IDShape
		cap   int
	}{"channel": {p.Channel, maxWebhookChannelLen}, "sender": {p.Sender, maxChatIDLen}, "user": {p.User, maxMentionUserLen}, "group": {p.Group, maxChatIDLen}} {
		if f.shape.Taken() {
			out[name] = f
		}
	}
	return out
}

// An ID of each kind that each platform takes, from which TestEveryPlatformsShapesAreWithinTheSchema makes others.
var exampleIDs = map[string]map[string]string{
	"Slack":   {"channel": "C0123456789", "sender": "B0123456789", "user": "U0123456789", "group": "S0123456789"},
	"webhook": {"channel": "acme/widgets"},
}

// setID puts id in a Binding's field of the kind of chat ID that field names, as shapes names it.
var setID = map[string]func(b *Binding, id string){
	"channel": func(b *Binding, id string) { b.Spec.Chat.Channel = id },
	"sender":  func(b *Binding, id string) { bot(b).From.ID = id },
	"user":    func(b *Binding, id string) { b.Spec.Chat.Mentions = []ChatMention{{Name: "sam", User: id}} },
	"group": func(b *Binding, id string) {
		b.Spec.Interactions.ToolApprovals = true
		b.Spec.Interactions.ApproverGroups = []string{id}
	},
}

// Every ID that a platform takes, Validate takes too, and so the schema, whose patterns and caps are Validate's
// (TestSchemaPatternsEqualTheGoLiterals, TestSchemaCapsEqualTheBrokersCaps): no ID that resolution takes on its platform is refused
// when it is applied or loaded. Each shape's cap is within its field's, and IDs near the shape's example are tried: every byte of
// ASCII and a few runes outside it (a letter, a space that is not ASCII's, a mark that turns the text and one that is not seen), at
// every place of the example, and the example made as long as the shape's cap. Each platform's IDs go in a Binding fixture of
// its own, since a webhook connection's Bindings take webhookEvent triggers alone.
func TestEveryPlatformsShapesAreWithinTheSchema(t *testing.T) {
	var chars []string
	for c := range 128 {
		chars = append(chars, string(rune(c)))
	}
	chars = append(chars, "é", "\u00a0", "\u202e", "\u200b", "\u0430")
	for _, tc := range []struct {
		spec    ChatConnectionSpec
		binding string
	}{
		{slackSpec, "binding.yaml"},
		{load[ChatConnection](t, "webhook/connection.yaml").Spec, "webhook/binding.yaml"},
	} {
		p := platformOf(t, tc.spec)
		for field, f := range shapes(p) {
			if f.shape.maxLen > f.cap {
				t.Errorf("%s: %s's cap %d is over the schema's %d", field, p.Name, f.shape.maxLen, f.cap)
			}
			example, ok := exampleIDs[p.Name][field]
			if !ok || !f.shape.Matches(example) {
				t.Fatalf("%s: %s has no example of its shape, or %q is not one", field, p.Name, example)
			}
			ids := []string{example, example + strings.Repeat("0", f.shape.maxLen-len(example))}
			for _, c := range chars {
				for i := 0; i <= len(example); i++ {
					ids = append(ids, example[:i]+c+example[i:])
				}
			}
			for _, id := range ids {
				if !f.shape.Matches(id) {
					continue
				}
				b := load[Binding](t, tc.binding)
				setID[field](b, id)
				if err := b.Validate(); err != nil {
					t.Errorf("%s: %s's shape takes %q, which Validate refuses: %v", field, p.Name, id, err)
				}
			}
		}
	}
}

func TestSlackIDShapes(t *testing.T) {
	p := slack(t)
	for _, tc := range []struct {
		name  string
		shape IDShape
		in    string
		want  bool
	}{
		{"channel", p.Channel, "C0123456789", true},
		{"channel", p.Channel, "G0123456789", true},
		{"channel", p.Channel, "C" + strings.Repeat("0", maxChatIDLen-1), true},
		{"channel", p.Channel, "C" + strings.Repeat("0", maxChatIDLen), false},
		{"channel", p.Channel, "C", false},
		{"channel", p.Channel, "", false},
		{"channel", p.Channel, "general", false},
		{"channel", p.Channel, "#general", false},
		{"channel", p.Channel, "D0123456789", false},
		{"channel", p.Channel, "c0123456789", false},
		{"channel", p.Channel, "C012345678a", false},
		{"channel", p.Channel, "C0123456789\n", false},
		{"channel", p.Channel, "octo-org/hello-world", false},
		{"sender", p.Sender, "B0123456789", true},
		{"sender", p.Sender, "B" + strings.Repeat("0", maxChatIDLen), false},
		{"sender", p.Sender, "U0123456789", false},
		{"sender", p.Sender, "alertmanager", false},
		{"sender", p.Sender, "dependabot[bot]", false},
		{"user", p.User, "U0123456789", true},
		{"user", p.User, "W" + strings.Repeat("A", 20), true},
		{"user", p.User, "U" + strings.Repeat("A", 21), false},
		{"user", p.User, "S0123456789", false},
		{"user", p.User, "octocat", false},
		{"group", p.Group, "S0123456789", true},
		{"group", p.Group, "S" + strings.Repeat("0", maxChatIDLen), false},
		{"group", p.Group, "octo-org/sre", false},
	} {
		if got := tc.shape.Matches(tc.in); got != tc.want {
			t.Errorf("Slack's %s shape Matches(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// Every platform that a ChatConnection may name has rules for its Bindings, and no other has: a platform added to Validate without
// a case in ChatPlatformOf fails here, before resolution can meet a connection whose Bindings it cannot check.
func TestChatPlatformOfAnswersThePlatformsValidateAccepts(t *testing.T) {
	for _, fixture := range []string{"chatconnection.yaml", "webhook/connection.yaml"} {
		for _, platform := range append(platforms(), "web", "github", "teams", "Slack", "") {
			c := load[ChatConnection](t, fixture)
			c.Spec.Platform = platform
			if platform == PlatformSlack && c.Spec.Webhook != nil {
				continue // Validate refuses a webhook block on Slack, and resolution asks no rules of a refused connection
			}
			accepted := c.Validate() == nil
			if _, ok := ChatPlatformOf(c.Spec); ok != accepted {
				t.Errorf("%s, platform %q: ChatPlatformOf answers %v, and Validate accepts the connection: %v", fixture, platform, ok, accepted)
			}
		}
	}
	for _, platform := range platforms() {
		fixture := map[string]string{PlatformSlack: "chatconnection.yaml", PlatformWebhook: "webhook/connection.yaml"}[platform]
		if fixture == "" {
			t.Fatalf("platform %q has no fixture connection", platform)
		}
		if err := load[ChatConnection](t, fixture).Validate(); err != nil {
			t.Errorf("%s: the fixture of %s is refused: %v", fixture, platform, err)
		}
	}
}

// What each platform serves of a Binding: Slack everything (the webhook platform, TestTheWebhookPlatform).
func TestWhatEachPlatformServes(t *testing.T) {
	for _, tc := range []struct {
		spec                                      ChatConnectionSpec
		name, actor                               string
		replies, askUser, toolApprovals, feedback bool
	}{
		{slackSpec, "Slack", "Slack", true, true, true, true},
	} {
		p := platformOf(t, tc.spec)
		if want := []TriggerType{TriggerMention, TriggerBotMessage, TriggerPersonMessage}; !slices.Equal(p.Triggers, want) {
			t.Errorf("%s takes the trigger types %v, want %v", tc.name, p.Triggers, want)
		}
		if !p.Channel.Taken() || !p.Sender.Taken() || !p.User.Taken() || !p.Group.Taken() {
			t.Errorf("%s does not take every kind of chat ID: %+v", tc.name, p)
		}
		if p.Name != tc.name || p.ActorName != tc.actor || p.Replies != tc.replies || p.AskUser != tc.askUser || p.ToolApprovals != tc.toolApprovals || p.Feedback != tc.feedback {
			t.Errorf("%s: %+v; want actor %q, replies %v, askUser %v, toolApprovals %v, feedback %v", tc.name, p, tc.actor, tc.replies, tc.askUser, tc.toolApprovals, tc.feedback)
		}
	}
}

func TestPlatformMismatches(t *testing.T) {
	noApprovals := slack(t)
	noApprovals.ToolApprovals = false
	noChannels := slack(t)
	noChannels.Channel = IDShape{}
	noButtons := slack(t)
	noButtons.AskUser, noButtons.ToolApprovals, noButtons.Feedback = false, false, false
	for _, tc := range []struct {
		name     string
		platform ChatPlatform
		mutate   func(*Binding)
		want     []PlatformMismatch
	}{
		{"a Slack Binding on Slack", slack(t), func(*Binding) {}, nil},
		{"a Slack Binding that names everything on Slack", slack(t), func(b *Binding) {
			b.Spec.Chat.Mentions = []ChatMention{{Name: "sam", User: "U0123456789"}, {Name: "kim", User: "W0123456789"}}
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Interactions.ApproverGroups = []string{"S0123456789"}
		}, nil},
		{"every ID of another platform's shape, in the order of the fields", slack(t), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			bot(b).From.ID = "dependabot[bot]"
			b.Spec.Chat.Mentions = []ChatMention{{Name: "sam", User: "U0123456789"}, {Name: "oct", User: "octocat"}}
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Interactions.ApproverGroups = []string{"S0123456789", "octo-org/sre"}
		}, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: `"octo-org/hello-world" is not a channel ID`, Fix: "use the channel's ID, such as C0123456789 (G… for an older private channel), not its name"},
			{Path: "spec.chat.triggers[1].from.id", Problem: `"dependabot[bot]" is not a bot ID`, Fix: "use the bot's ID, such as B0123456789, not its name"},
			{Path: "spec.chat.mentions[1].user", Problem: `"octocat" is not a user ID`, Fix: "use the person's user ID, such as U0123456789"},
			{Path: "spec.interactions.approverGroups[1]", Problem: `"octo-org/sre" is not a user group ID`, Fix: "use the user group's ID, such as S0123456789"},
		}},
		{"a Slack ID typo that has a login's shape", slack(t), func(b *Binding) { bot(b).From.ID = "alertmanager" }, []PlatformMismatch{
			{Path: "spec.chat.triggers[1].from.id", Problem: `"alertmanager" is not a bot ID`, Fix: "use the bot's ID, such as B0123456789, not its name"},
		}},
		{"a Slack channel over Slack's length: the length, and not the value", slack(t), func(b *Binding) {
			b.Spec.Chat.Channel = "C" + strings.Repeat("0", maxChatIDLen)
		}, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: "is 33 bytes, longer than the 32 byte limit of a channel ID",
				Fix: "use the channel's ID, such as C0123456789 (G… for an older private channel), not its name"},
		}},
		{"a Slack channel at Slack's length", slack(t), func(b *Binding) { b.Spec.Chat.Channel = "C" + strings.Repeat("0", maxChatIDLen-1) }, nil},
		{"a sender and a group over Slack's length", slack(t), func(b *Binding) {
			bot(b).From.ID = "B" + strings.Repeat("0", maxChatIDLen)
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Interactions.ApproverGroups = []string{"S" + strings.Repeat("0", maxChatIDLen)}
		}, []PlatformMismatch{
			{Path: "spec.chat.triggers[1].from.id", Problem: "is 33 bytes, longer than the 32 byte limit of a bot ID", Fix: "use the bot's ID, such as B0123456789, not its name"},
			{Path: "spec.interactions.approverGroups[0]", Problem: "is 33 bytes, longer than the 32 byte limit of a user group ID", Fix: "use the user group's ID, such as S0123456789"},
		}},
		{"a mention trigger names no sender", slack(t), func(b *Binding) { b.Spec.Chat.Triggers = b.Spec.Chat.Triggers[:1] }, nil},
		{"tool approvals on a platform without them", noApprovals, func(b *Binding) { b.Spec.Interactions.ToolApprovals = true }, []PlatformMismatch{
			{Path: "spec.interactions.toolApprovals", Unsupported: true, Problem: "tool approvals are not supported", Fix: "set it to false"},
		}},
		{"tool approvals from groups on a platform without them", noApprovals, func(b *Binding) {
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Interactions.ApproverGroups = []string{"S0123456789"}
		}, []PlatformMismatch{
			{Path: "spec.interactions.toolApprovals", Unsupported: true, Problem: "tool approvals are not supported",
				Fix: "set it to false and remove spec.interactions.approverGroups, which requires it"},
		}},
		{"no tool approvals on a platform without them", noApprovals, func(*Binding) {}, nil},
		{"a channel on a platform that takes no channel ID: unsupported, and no panic", noChannels, func(*Binding) {}, []PlatformMismatch{
			{Path: "spec.chat.channel", Unsupported: true, Problem: "an ID of this kind is not supported", Fix: "remove it: the platform takes no such ID"},
		}},
		{"questions, tool approvals and feedback on a platform without them, in the order of the fields", noButtons, func(b *Binding) {
			b.Spec.Interactions.AskUser, b.Spec.Interactions.ToolApprovals, b.Spec.Feedback.Enabled = true, true, true
		}, []PlatformMismatch{
			{Path: "spec.interactions.askUser", Unsupported: true, Problem: "questions to a person are not supported", Fix: "set it to false"},
			{Path: "spec.interactions.toolApprovals", Unsupported: true, Problem: "tool approvals are not supported", Fix: "set it to false"},
			{Path: "spec.feedback.enabled", Unsupported: true, Problem: "feedback buttons are not supported", Fix: "set it to false"},
		}},
		{"output final on Slack", slack(t), func(b *Binding) { b.Spec.Chat.Output = OutputFinal }, nil},
		{"a Slack channel's name on Slack", slack(t), func(b *Binding) { b.Spec.Chat.Channel = "general" }, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: `"general" is not a channel ID`, Fix: "use the channel's ID, such as C0123456789 (G… for an older private channel), not its name"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := load[Binding](t, "binding.yaml")
			tc.mutate(b)
			// The schema and Validate hold Slack's shapes, so every chat ID that resolution refuses on Slack, Validate refused first;
			// a feature a platform does not serve is resolution's alone.
			wrongID := slices.ContainsFunc(tc.want, func(m PlatformMismatch) bool { return !m.Unsupported })
			if err := b.Validate(); (err != nil) != wrongID {
				t.Fatalf("Validate = %v; want an error exactly when an ID is not Slack's (%v)", err, wrongID)
			}
			if got := b.Spec.PlatformMismatches(tc.platform); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PlatformMismatches =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}
