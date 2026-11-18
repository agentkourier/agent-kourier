package v1alpha1

import (
	"reflect"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
)

// The spec of a connection on each platform that Validate accepts.
var (
	slackSpec = ChatConnectionSpec{Platform: PlatformSlack}
	teamsSpec = ChatConnectionSpec{Platform: PlatformTeams, Teams: &TeamsConnectionSpec{TenantID: testTenant, AppID: testApp}}
	// teamsAlertsSpec is a teams connection with a notification URL, on which a botMessage trigger is taken.
	teamsAlertsSpec = ChatConnectionSpec{Platform: PlatformTeams, Teams: &TeamsConnectionSpec{TenantID: testTenant, AppID: testApp,
		NotificationURL: "https://kourier.example.com" + teamsRouteOfTheFixture}}
)

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
	}{"channel": {p.Channel, maxChannelIDLen}, "sender": {p.Sender, maxSenderIDLen}, "user": {p.User, maxMentionUserLen}, "group": {p.Group, maxApproverGroupIDLen}} {
		if f.shape.Taken() {
			out[name] = f
		}
	}
	return out
}

// An ID of each kind that each platform takes, from which TestEveryPlatformsShapesAreWithinTheSchema makes others.
var exampleIDs = map[string]map[string]string{
	"Slack":           {"channel": "C0123456789", "sender": "B0123456789", "user": "U0123456789", "group": "S0123456789"},
	"Microsoft Teams": {"channel": testTeamsChannel, "sender": testApp, "user": testTenant},
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
// every place of the example, and the example made as long as the shape's cap.
func TestEveryPlatformsShapesAreWithinTheSchema(t *testing.T) {
	var chars []string
	for c := range 128 {
		chars = append(chars, string(rune(c)))
	}
	chars = append(chars, "é", "\u00a0", "\u202e", "\u200b", "\u0430")
	for _, spec := range []ChatConnectionSpec{slackSpec, teamsSpec} {
		p := platformOf(t, spec)
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
				b := load[Binding](t, "binding.yaml")
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
		{"channel", p.Channel, testTeamsChannel, false},
		{"sender", p.Sender, testApp, false},
		{"user", p.User, testTenant, false},
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
// a case in ChatPlatformOf fails here, before resolution can meet a connection whose Bindings it cannot check. A teams connection
// has rules only with its teams block, which Validate requires.
func TestChatPlatformOfAnswersThePlatformsValidateAccepts(t *testing.T) {
	for _, platform := range []string{PlatformSlack, PlatformTeams, "web", "github", "Teams", "discord", "Slack", ""} {
		for _, teams := range []*TeamsConnectionSpec{nil, teamsSpec.Teams, teamsAlertsSpec.Teams} {
			if teams != nil && platform != PlatformTeams {
				continue // refused for the block, which TestTeamsConnectionValidate covers, and not for the platform
			}
			c := load[ChatConnection](t, "chatconnection.yaml")
			c.Spec.Platform, c.Spec.Teams = platform, teams
			accepted := c.Validate() == nil
			if _, ok := ChatPlatformOf(c.Spec); ok != accepted {
				t.Errorf("platform %q, teams %+v: ChatPlatformOf answers %v, and Validate accepts the connection: %v", platform, teams, ok, accepted)
			}
		}
	}
}

// alternatives returns the alternatives of a union pattern ^(?:a|b|…)$, as regexp/syntax parses it.
func alternatives(t *testing.T, union string) []string {
	t.Helper()
	inner, ok := strings.CutPrefix(union, "^(?:")
	if !ok {
		return []string{strings.TrimSuffix(strings.TrimPrefix(union, "^"), "$")}
	}
	inner, ok = strings.CutSuffix(inner, ")$")
	if !ok {
		t.Fatalf("the union %s is not ^(?:…)$", union)
	}
	if _, err := syntax.Parse(inner, syntax.Perl); err != nil {
		t.Fatalf("the union %s: %v", union, err)
	}
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '\\':
			i++
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case '|':
			if depth == 0 {
				out = append(out, inner[start:i])
				start = i + 1
			}
		}
	}
	return append(out, inner[start:])
}

// Each platform's shape is an alternative of the union that the schema and Validate hold its field to, so that no ID that
// resolution takes on its platform is refused at admission; and each alternative of the union is some platform's shape, so that
// the union takes nothing that no platform does. Slack's comes first, as it always has. A platform with no approver groups has no
// Group shape (Microsoft Teams).
func TestEveryPlatformsShapesAreTheAlternativesOfTheUnion(t *testing.T) {
	platforms := []ChatPlatform{slack(t), platformOf(t, teamsSpec)}
	for _, tc := range []struct {
		field string
		union string
		shape func(ChatPlatform) IDShape
	}{
		{"channel", channelID.String(), func(p ChatPlatform) IDShape { return p.Channel }},
		{"sender", senderID.String(), func(p ChatPlatform) IDShape { return p.Sender }},
		{"user", mentionUser.String(), func(p ChatPlatform) IDShape { return p.User }},
		{"group", slackGroupID.String(), func(p ChatPlatform) IDShape { return p.Group }},
	} {
		alts := alternatives(t, tc.union)
		var shapes []string
		for _, p := range platforms {
			if !tc.shape(p).Taken() {
				continue
			}
			inner := strings.TrimSuffix(strings.TrimPrefix(tc.shape(p).pattern.String(), "^"), "$")
			if !slices.Contains(alts, inner) {
				t.Errorf("%s: %s's shape %s is not an alternative of the union %s", tc.field, p.Name, inner, tc.union)
			}
			if !slices.Contains(shapes, inner) {
				shapes = append(shapes, inner)
			}
		}
		if !slices.Equal(alts, shapes) {
			t.Errorf("%s: the union's alternatives are %q; want the platforms' shapes %q, Slack's first", tc.field, alts, shapes)
		}
	}
}

// What each platform serves of a Binding: Slack everything; Microsoft Teams questions, tool approvals and feedback, with or without
// a notification URL, and no approver groups (TestTheGroupsAndBotTriggersOfEachPlatform).
func TestWhatEachPlatformServes(t *testing.T) {
	for _, tc := range []struct {
		spec                             ChatConnectionSpec
		name, actor                      string
		askUser, toolApprovals, feedback bool
	}{
		{slackSpec, "Slack", "Slack", true, true, true},
		{teamsSpec, "Microsoft Teams", "Microsoft Teams", true, true, true},
		{teamsAlertsSpec, "Microsoft Teams", "Microsoft Teams", true, true, true},
	} {
		p := platformOf(t, tc.spec)
		if !p.Channel.Taken() || !p.Sender.Taken() || !p.User.Taken() || p.Group.Taken() != p.Groups {
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
	noBots := slack(t)
	noBots.BotTriggers = false
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
		{"a botMessage trigger on a platform without them: the trigger, and not its sender", noBots, func(*Binding) {}, []PlatformMismatch{
			{Path: "spec.chat.triggers[1].type", Unsupported: true, Problem: `"botMessage" is not supported`, Fix: "remove the trigger"},
		}},
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
			// Validate holds each ID to the union of the platforms' shapes, so every chat ID that it refuses, resolution refuses on Slack
			// too; resolution refuses more (a Teams ID, a Slack ID past Slack's cap), and a feature a platform does not serve is
			// resolution's alone.
			wrongID := slices.ContainsFunc(tc.want, func(m PlatformMismatch) bool { return !m.Unsupported })
			if err := b.Validate(); err != nil && !wrongID {
				t.Fatalf("Validate = %v; want no error where resolution refuses no ID", err)
			}
			if got := b.Spec.PlatformMismatches(tc.platform); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PlatformMismatches =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}
