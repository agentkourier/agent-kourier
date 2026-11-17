package v1alpha1

import (
	"reflect"
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
	}{"channel": {p.Channel, maxChatIDLen}, "sender": {p.Sender, maxChatIDLen}, "user": {p.User, maxMentionUserLen}, "group": {p.Group, maxChatIDLen}} {
		if f.shape.Taken() {
			out[name] = f
		}
	}
	return out
}

// An ID of each kind that each platform takes, from which TestEveryPlatformsShapesAreWithinTheSchema makes others.
var exampleIDs = map[string]map[string]string{
	"Slack": {"channel": "C0123456789", "sender": "B0123456789", "user": "U0123456789", "group": "S0123456789"},
}

// Every ID that a platform takes, the schema and Validate take too, so that no ID that resolution takes on its platform is refused
// when it is applied or loaded: each shape's cap is within its field's, and no ID of a shape holds a character outside chatIDChars.
// The characters are tried one at a time, every byte of ASCII and a few runes outside it (a letter, a space that is not ASCII's, a
// mark that turns the text and one that is not seen), at every place of an ID that the shape takes.
func TestEveryPlatformsShapesAreWithinTheSchema(t *testing.T) {
	var chars []string
	for c := range 128 {
		chars = append(chars, string(rune(c)))
	}
	chars = append(chars, "é", "\u00a0", "\u202e", "\u200b", "\u0430")
	for _, spec := range []ChatConnectionSpec{slackSpec} {
		p := platformOf(t, spec)
		for field, f := range shapes(p) {
			if f.shape.maxLen > f.cap {
				t.Errorf("%s: %s's cap %d is over the schema's %d", field, p.Name, f.shape.maxLen, f.cap)
			}
			example, ok := exampleIDs[p.Name][field]
			if !ok || !f.shape.Matches(example) {
				t.Fatalf("%s: %s has no example of its shape, or %q is not one", field, p.Name, example)
			}
			for _, c := range chars {
				for i := 0; i <= len(example); i++ {
					if id := example[:i] + c + example[i:]; f.shape.Matches(id) && !chatIDChars.MatchString(id) {
						t.Errorf("%s: %s's shape takes %q, which the schema refuses", field, p.Name, id)
					}
				}
			}
		}
	}
}

// What the schema refuses of a chat ID, and what it takes, which resolution refuses on a platform whose shape it is not: a
// channel's name such as general, which is no Slack channel ID.
func TestTheChatIDCharacters(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"C0123456789", true}, {"octo-org/hello-world", true}, {"dependabot[bot]", true}, {"general", true}, {"#general", true},
		{"sam@example.com", true}, {"{557058-uuid}", true}, {"a!\"#$%&'()*+,-./9;<=>?@Z[\\]^_`z{|}~", true},
		{"", false}, {"ch:1", false}, {"ch 1", false}, {"ch\t1", false}, {"ch\n", false}, {"\x00", false}, {"ch\x7f", false},
		{"ché", false}, {"ch\u00a01", false}, {"ch\u202e1", false}, {"ch\u200b1", false},
	} {
		if got := chatIDChars.MatchString(tc.id); got != tc.want {
			t.Errorf("chatIDChars.MatchString(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
	if slack(t).Channel.Matches("general") {
		t.Error("Slack's channel shape takes the name general")
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
		{"channel", p.Channel, "C" + strings.Repeat("0", maxSlackIDLen-1), true},
		{"channel", p.Channel, "C" + strings.Repeat("0", maxSlackIDLen), false},
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
		{"sender", p.Sender, "B" + strings.Repeat("0", maxSlackIDLen), false},
		{"sender", p.Sender, "U0123456789", false},
		{"sender", p.Sender, "alertmanager", false},
		{"sender", p.Sender, "dependabot[bot]", false},
		{"user", p.User, "U0123456789", true},
		{"user", p.User, "W" + strings.Repeat("A", 20), true},
		{"user", p.User, "U" + strings.Repeat("A", 21), false},
		{"user", p.User, "S0123456789", false},
		{"user", p.User, "octocat", false},
		{"group", p.Group, "S0123456789", true},
		{"group", p.Group, "S" + strings.Repeat("0", maxSlackIDLen), false},
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
	for _, platform := range []string{PlatformSlack, "web", "github", "teams", "Slack", ""} {
		c := load[ChatConnection](t, "chatconnection.yaml")
		c.Spec.Platform = platform
		accepted := c.Validate() == nil
		if _, ok := ChatPlatformOf(c.Spec); ok != accepted {
			t.Errorf("platform %q: ChatPlatformOf answers %v, and Validate accepts the connection: %v", platform, ok, accepted)
		}
	}
}

// What each platform serves of a Binding: Slack everything.
func TestWhatEachPlatformServes(t *testing.T) {
	for _, tc := range []struct {
		spec                             ChatConnectionSpec
		name, actor                      string
		askUser, toolApprovals, feedback bool
	}{
		{slackSpec, "Slack", "Slack", true, true, true},
	} {
		p := platformOf(t, tc.spec)
		if !p.Channel.Taken() || !p.Sender.Taken() || !p.User.Taken() || !p.Group.Taken() {
			t.Errorf("%s does not take every kind of chat ID: %+v", tc.name, p)
		}
		if p.Name != tc.name || p.ActorName != tc.actor || p.AskUser != tc.askUser || p.ToolApprovals != tc.toolApprovals || p.Feedback != tc.feedback {
			t.Errorf("%s: %+v; want actor %q, askUser %v, toolApprovals %v, feedback %v", tc.name, p, tc.actor, tc.askUser, tc.toolApprovals, tc.feedback)
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
			b.Spec.Chat.Channel = "C" + strings.Repeat("0", maxSlackIDLen)
		}, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: "is 33 bytes, longer than the 32 byte limit of a channel ID",
				Fix: "use the channel's ID, such as C0123456789 (G… for an older private channel), not its name"},
		}},
		{"a Slack channel at Slack's length", slack(t), func(b *Binding) { b.Spec.Chat.Channel = "C" + strings.Repeat("0", maxSlackIDLen-1) }, nil},
		{"a sender and a group over Slack's length", slack(t), func(b *Binding) {
			bot(b).From.ID = "B" + strings.Repeat("0", maxSlackIDLen)
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Interactions.ApproverGroups = []string{"S" + strings.Repeat("0", maxSlackIDLen)}
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
		{"a Slack channel's name on Slack, which the schema takes", slack(t), func(b *Binding) { b.Spec.Chat.Channel = "general" }, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: `"general" is not a channel ID`, Fix: "use the channel's ID, such as C0123456789 (G… for an older private channel), not its name"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := load[Binding](t, "binding.yaml")
			tc.mutate(b)
			if err := b.Validate(); err != nil {
				t.Fatalf("the Binding does not pass Validate, which PlatformMismatches expects: %v", err)
			}
			if got := b.Spec.PlatformMismatches(tc.platform); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PlatformMismatches =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}
