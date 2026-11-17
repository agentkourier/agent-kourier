package v1alpha1

import (
	"reflect"
	"strings"
	"testing"
)

// The specs of a connection on each platform and profile that Validate accepts.
var (
	slackSpec     = ChatConnectionSpec{Platform: PlatformSlack}
	kourierV1Spec = ChatConnectionSpec{Platform: PlatformWeb, Web: &WebConnectionSpec{Profile: WebProfileKourierV1, URL: "https://receiver.example.com/replies"}}
	githubSpec    = ChatConnectionSpec{Platform: PlatformWeb, Web: &WebConnectionSpec{Profile: WebProfileGitHub, Identity: testLogin}}
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
	}{"channel": {p.Channel, maxChatIDLen}, "sender": {p.Sender, maxChatIDLen}, "user": {p.User, maxMentionUserLen}, "group": {p.Group, maxChatIDLen}} {
		if f.shape.Taken() {
			out[name] = f
		}
	}
	return out
}

// An ID of each kind that each platform takes, from which TestEveryPlatformsShapesAreWithinTheSchema makes others.
var exampleIDs = map[string]map[string]string{
	"Slack":                    {"channel": "C0123456789", "sender": "B0123456789", "user": "U0123456789", "group": "S0123456789"},
	"web (kourier-v1 profile)": {"channel": "ch-1", "sender": "billing-bot", "user": "alice", "group": "team-7"},
	"web (github profile)":     {"channel": "octo-org/hello-world"},
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
	for _, spec := range []ChatConnectionSpec{slackSpec, kourierV1Spec, githubSpec} {
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

// GitHub's shapes, the web platform's github profile's: a repository as a channel, and a login as the connection's identity. It takes
// no sender, user or group: it has no replies.
func TestGitHubIDShapes(t *testing.T) {
	p := platformOf(t, githubSpec)
	owner, name := strings.Repeat("a", 39), strings.Repeat("r", 100)
	for _, tc := range []struct {
		name  string
		shape IDShape
		in    string
		want  bool
	}{
		{"channel", p.Channel, "octo-org/hello-world", true},
		{"channel", p.Channel, owner + "/" + name, true},
		{"channel", p.Channel, owner + "a/" + name, false},
		{"channel", p.Channel, "octo-org/" + name + "r", false},
		{"channel", p.Channel, "-octo/repo", false},
		{"channel", p.Channel, "octo--org/repo", false},
		{"channel", p.Channel, "octo-org", false},
		{"channel", p.Channel, "C0123456789", false},
		{"channel", p.Channel, "general1", false},
		{"identity", p.Identity, "dependabot[bot]", true},
		{"identity", p.Identity, "octocat", true},
		{"identity", p.Identity, owner + "[bot]", true},
		{"identity", p.Identity, owner + "a", false},
		{"identity", p.Identity, "octo.cat", false},
		{"identity", p.Identity, "octo_cat", false},
		{"identity", p.Identity, "@octocat", false},
		{"identity", p.Identity, "pr-reviewer[app]", false},
		// The pattern counts letters and digits, at most 39, with single hyphens between them, and so does not bound the
		// length: the caps do, 44 bytes for a login and 140 for a repository.
		{"login past 39 with its hyphens", p.Identity, hyphenated(22), true},
		{"login at the cap", p.Identity, hyphenated(22) + "x", true},
		{"login past the cap", p.Identity, hyphenated(23), false},
		{"login of the pattern's most", p.Identity, hyphenated(39), false},
		{"repository at the cap", p.Channel, hyphenated(39) + "/" + strings.Repeat("r", 62), true},
		{"repository past the cap", p.Channel, hyphenated(39) + "/" + strings.Repeat("r", 63), false},
		{"no sender", p.Sender, "dependabot[bot]", false},
		{"no user", p.User, "octocat", false},
		{"no group", p.Group, "octo-org/sre", false},
	} {
		if got := tc.shape.Matches(tc.in); got != tc.want {
			t.Errorf("github's %s shape Matches(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}

	// Past the cap, the broker says the length, which the reference pages give.
	b := &BindingSpec{Chat: ChatBinding{Channel: hyphenated(39) + "/" + strings.Repeat("r", 63), Output: OutputNone}}
	want := []PlatformMismatch{{Path: "spec.chat.channel", Problem: "is 141 bytes, longer than the 140 byte limit of a repository",
		Fix: "use the repository's owner/name, such as octo-org/hello-world"}}
	if got := b.PlatformMismatches(p); !reflect.DeepEqual(got, want) {
		t.Errorf("a repository of 141 bytes: PlatformMismatches = %+v, want %+v", got, want)
	}
}

// hyphenated returns n letters with a hyphen between each two: 2n-1 bytes, a login's shape whatever n is.
func hyphenated(n int) string { return strings.TrimSuffix(strings.Repeat("a-", n), "-") }

// The kourier-v1 profile's shape, of every kind of ID: an opaque ID of letters, digits, '.', '_' and '-', starting with a letter or a
// digit, at most maxKourierV1IDLen bytes. A channel or a group needs no digit: a name such as general is one.
func TestKourierV1IDShapes(t *testing.T) {
	p := platformOf(t, kourierV1Spec)
	for _, tc := range []struct {
		name  string
		shape IDShape
		in    string
		want  bool
	}{
		{"channel", p.Channel, "ch-1", true},
		{"channel", p.Channel, "0b4a6f2e-6a1f-4c1e-9d1a-2f1c3b4d5e6f", true},
		{"channel", p.Channel, "01HZX3Y4ABCDEF", true},
		{"channel", p.Channel, "C0123456789", true},
		{"channel", p.Channel, "7", true},
		{"channel", p.Channel, "a" + strings.Repeat("1", maxKourierV1IDLen-1), true},
		{"channel", p.Channel, "a" + strings.Repeat("1", maxKourierV1IDLen), false},
		{"channel", p.Channel, "general", true},
		{"channel", p.Channel, "ops.alerts_x-y", true},
		{"channel", p.Channel, "-1", false},
		{"channel", p.Channel, ".1", false},
		{"channel", p.Channel, "a:1", false},
		{"channel", p.Channel, "octo-org/repo1", false},
		{"channel", p.Channel, "a 1", false},
		{"channel", p.Channel, "", false},
		{"sender", p.Sender, "billing-bot", true},
		{"sender", p.Sender, "svc.ci_runner-2", true},
		{"sender", p.Sender, "dependabot[bot]", false},
		{"sender", p.Sender, strings.Repeat("b", maxKourierV1IDLen+1), false},
		{"user", p.User, "alice", true},
		{"user", p.User, "u_42", true},
		{"user", p.User, strings.Repeat("u", maxKourierV1IDLen), true},
		{"user", p.User, strings.Repeat("u", maxKourierV1IDLen+1), false},
		{"user", p.User, "_alice", false},
		{"user", p.User, "alice@example.com", false},
		{"group", p.Group, "team-7", true},
		{"group", p.Group, "sre", true},
		{"group", p.Group, "octo-org/sre", false},
		{"identity", p.Identity, "pr-reviewer", false},
	} {
		if got := tc.shape.Matches(tc.in); got != tc.want {
			t.Errorf("kourier-v1's %s shape Matches(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// Every platform and profile that a ChatConnection may name has rules for its Bindings, and no other has: a platform or profile
// added to Validate without a case in ChatPlatformOf fails here, before resolution can meet a connection whose Bindings it cannot
// check.
func TestChatPlatformOfAnswersThePlatformsValidateAccepts(t *testing.T) {
	webs := []*WebConnectionSpec{nil, {}, {Profile: WebProfileKourierV1, URL: "https://r.example.com"}, {Profile: WebProfileGitHub, Identity: testLogin},
		{Profile: "gitlab"}, {Profile: "GitHub"}}
	for _, platform := range []string{PlatformSlack, PlatformWeb, "github", "teams", "Slack", ""} {
		for _, web := range webs {
			if web != nil && platform != PlatformWeb {
				continue // refused for the block, which TestChatConnectionValidate covers, and not for the platform
			}
			c := load[ChatConnection](t, "chatconnection.yaml")
			c.Spec.Platform, c.Spec.Web = platform, web
			accepted := c.Validate() == nil
			if _, ok := ChatPlatformOf(c.Spec); ok != accepted {
				t.Errorf("platform %q, web %+v: ChatPlatformOf answers %v, and Validate accepts the connection: %v", platform, web, ok, accepted)
			}
		}
	}
}

// What each platform serves of a Binding: Slack everything; the kourier-v1 profile posts replies, and has no buttons and no way to
// take an answer; the github profile posts nothing, so none is its only output and its default, and hands on mentions alone.
func TestWhatEachPlatformServes(t *testing.T) {
	for _, tc := range []struct {
		spec                                                    ChatConnectionSpec
		name, actor                                             string
		replies, askUser, toolApprovals, feedback, mentionsOnly bool
		identity                                                bool
		output                                                  OutputMode
	}{
		{slackSpec, "Slack", "Slack", true, true, true, true, false, false, OutputLive},
		{kourierV1Spec, "web (kourier-v1 profile)", "web", true, false, false, false, false, false, OutputLive},
		{githubSpec, "web (github profile)", "GitHub", false, false, false, false, true, true, OutputNone},
	} {
		p := platformOf(t, tc.spec)
		if !p.Channel.Taken() {
			t.Errorf("%s takes no channel", tc.name)
		}
		// Nothing would use a sender, a user or a group where Agent Kourier posts nothing: no bot's message is handed on, and no
		// mention is posted and no approval asked for.
		if !p.Replies && (p.Sender.Taken() || p.User.Taken() || p.Group.Taken()) {
			t.Errorf("%s has no replies and takes a sender, a user or a group: %+v", tc.name, p)
		}
		if p.ActorName != tc.actor || p.Identity.Taken() != tc.identity {
			t.Errorf("%s: the actor name is %q and an identity is taken: %v; want %q and %v", tc.name, p.ActorName, p.Identity.Taken(), tc.actor, tc.identity)
		}
		if p.Name != tc.name || p.Replies != tc.replies || p.AskUser != tc.askUser || p.ToolApprovals != tc.toolApprovals || p.Feedback != tc.feedback ||
			p.MentionsOnly != tc.mentionsOnly {
			t.Errorf("%s: %+v; want replies %v, askUser %v, toolApprovals %v, feedback %v, mentionsOnly %v", tc.name, p, tc.replies, tc.askUser,
				tc.toolApprovals, tc.feedback, tc.mentionsOnly)
		}
		if got := p.DefaultOutput(); got != tc.output {
			t.Errorf("%s: DefaultOutput() = %q, want %q", tc.name, got, tc.output)
		}
	}
}

// DefaultOn fills an output left out with the platform's default and keeps one that is set, on every platform: a stored Slack
// Binding that says live keeps it, and the check, not the default, refuses an output the platform does not serve.
func TestDefaultOnFillsTheOutputOfThePlatform(t *testing.T) {
	for _, tc := range []struct {
		spec     ChatConnectionSpec
		set      OutputMode
		want     OutputMode
		platform string
	}{
		{slackSpec, "", OutputLive, "slack"},
		{slackSpec, OutputFinal, OutputFinal, "slack"},
		{slackSpec, OutputLive, OutputLive, "slack"},
		{kourierV1Spec, "", OutputLive, "kourier-v1"},
		{githubSpec, "", OutputNone, "github"},
		{githubSpec, OutputLive, OutputLive, "github"},
	} {
		b := load[Binding](t, "binding-minimal.yaml")
		b.Spec.Chat.Output = tc.set
		b.Spec.DefaultOn(platformOf(t, tc.spec))
		if b.Spec.Chat.Output != tc.want {
			t.Errorf("%s, output %q: DefaultOn gives %q, want %q", tc.platform, tc.set, b.Spec.Chat.Output, tc.want)
		}
	}
}

func TestPlatformMismatches(t *testing.T) {
	noApprovals := slack(t)
	noApprovals.ToolApprovals = false
	noChannels := slack(t)
	noChannels.Channel = IDShape{}
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
		{"output none on Slack, which posts the replies", slack(t), func(b *Binding) { b.Spec.Chat.Output = OutputNone }, []PlatformMismatch{
			{Path: "spec.chat.output", Unsupported: true, Problem: `"none" is not supported`, Fix: "set it to live or final, or leave it out for live"},
		}},
		{"output left out on Slack", slack(t), func(b *Binding) { b.Spec.Chat.Output = "" }, nil},
		{"output final on Slack", slack(t), func(b *Binding) { b.Spec.Chat.Output = OutputFinal }, nil},
		{"a kourier-v1 Binding on kourier-v1", platformOf(t, kourierV1Spec), func(b *Binding) {
			b.Spec.Chat.Channel = "ch-7"
			bot(b).From.ID = "billing-bot"
			b.Spec.Chat.Mentions = []ChatMention{{Name: "sam", User: "alice"}}
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, nil},
		{"what kourier-v1 does not serve, in the order of the fields", platformOf(t, kourierV1Spec), func(b *Binding) {
			b.Spec.Chat.Channel = "C0123456789"
			b.Spec.Chat.Output = OutputNone
			b.Spec.Interactions.ToolApprovals = true
		}, []PlatformMismatch{
			{Path: "spec.chat.output", Unsupported: true, Problem: `"none" is not supported`, Fix: "set it to live or final, or leave it out for live"},
			{Path: "spec.interactions.askUser", Unsupported: true, Problem: "questions to a person are not supported", Fix: "set it to false"},
			{Path: "spec.interactions.toolApprovals", Unsupported: true, Problem: "tool approvals are not supported", Fix: "set it to false"},
			{Path: "spec.feedback.enabled", Unsupported: true, Problem: "feedback buttons are not supported", Fix: "set it to false"},
		}},
		{"IDs of GitHub's shapes on kourier-v1", platformOf(t, kourierV1Spec), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			bot(b).From.ID = "dependabot[bot]"
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: `"octo-org/hello-world" is not a channel ID`, Fix: "use the channel's ID as the sender sends it: letters, digits, '.', '_' and '-', starting with a letter or a digit, such as ch-1"},
			{Path: "spec.chat.triggers[1].from.id", Problem: `"dependabot[bot]" is not a sender ID`, Fix: "use the sender's ID as the sender sends it: letters, digits, '.', '_' and '-', starting with a letter or a digit"},
		}},
		{"a kourier-v1 ID over its length", platformOf(t, kourierV1Spec), func(b *Binding) {
			b.Spec.Chat.Channel = "c" + strings.Repeat("1", maxKourierV1IDLen)
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: "is 65 bytes, longer than the 64 byte limit of a channel ID",
				Fix: "use the channel's ID as the sender sends it: letters, digits, '.', '_' and '-', starting with a letter or a digit, such as ch-1"},
		}},
		{"a github Binding on github, its output left out", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			b.Spec.Chat.Output = ""
			b.Spec.Chat.Triggers = b.Spec.Chat.Triggers[:1] // the mention trigger alone
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, nil},
		{"output none on github", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			b.Spec.Chat.Output = OutputNone
			b.Spec.Chat.Triggers = b.Spec.Chat.Triggers[:1]
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, nil},
		{"what github does not serve, in the order of the fields", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "C0123456789"
			b.Spec.Chat.Mentions = []ChatMention{{Name: "oct", User: "octocat"}}
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Interactions.ApproverGroups = []string{"octo-org/sre"}
		}, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: `"C0123456789" is not a repository`, Fix: "use the repository's owner/name, such as octo-org/hello-world"},
			{Path: "spec.chat.output", Unsupported: true, Problem: `"live" is not supported`, Fix: "set it to none, or leave it out: the agent replies itself, and Agent Kourier posts nothing"},
			{Path: "spec.chat.triggers[1].from.id", Unsupported: true, Problem: "a bot's message is not supported",
				Fix: "remove the botMessage trigger: the platform hands on no bot's message"},
			{Path: "spec.chat.mentions", Unsupported: true, Problem: "mentions are not supported",
				Fix: "remove it: Agent Kourier posts nothing on the platform, so it notifies nobody"},
			{Path: "spec.interactions.askUser", Unsupported: true, Problem: "questions to a person are not supported", Fix: "set it to false"},
			{Path: "spec.interactions.toolApprovals", Unsupported: true, Problem: "tool approvals are not supported",
				Fix: "set it to false and remove spec.interactions.approverGroups, which requires it"},
			{Path: "spec.interactions.approverGroups", Unsupported: true, Problem: "approver groups are not supported",
				Fix: "remove it: the platform asks for no tool approvals"},
			{Path: "spec.feedback.enabled", Unsupported: true, Problem: "feedback buttons are not supported", Fix: "set it to false"},
		}},
		{"output final on github", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			b.Spec.Chat.Output = OutputFinal
			b.Spec.Chat.Triggers = b.Spec.Chat.Triggers[:1]
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, []PlatformMismatch{
			{Path: "spec.chat.output", Unsupported: true, Problem: `"final" is not supported`, Fix: "set it to none, or leave it out: the agent replies itself, and Agent Kourier posts nothing"},
		}},
		{"a github Binding whose triggers have no mention trigger", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			b.Spec.Chat.Output = ""
			b.Spec.Chat.Triggers = b.Spec.Chat.Triggers[1:] // the botMessage trigger alone
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, []PlatformMismatch{
			{Path: "spec.chat.triggers", Unsupported: true, Problem: "a list with no mention trigger is not supported",
				Fix: "add {type: mention} to it, or leave it out, which is [{type: mention}]: every event the platform hands on is a mention, and no other trigger starts a turn"},
			{Path: "spec.chat.triggers[0].from.id", Unsupported: true, Problem: "a bot's message is not supported",
				Fix: "remove the botMessage trigger: the platform hands on no bot's message"},
		}},
		{"a github Binding whose triggers are left out", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			b.Spec.Chat.Output = ""
			b.Spec.Chat.Triggers = nil
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, nil},
		{"a Slack Binding whose triggers have no mention trigger", slack(t), func(b *Binding) { b.Spec.Chat.Triggers = b.Spec.Chat.Triggers[1:] }, nil},
		{"a sender, users and a group on github, whatever their shape: nothing would use one", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			b.Spec.Chat.Output = ""
			bot(b).From.ID = "dependabot[bot]"
			b.Spec.Chat.Mentions = []ChatMention{{Name: "oct", User: "octocat"}, {Name: "kim", User: "U0123456789"}}
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, []PlatformMismatch{
			{Path: "spec.chat.triggers[1].from.id", Unsupported: true, Problem: "a bot's message is not supported",
				Fix: "remove the botMessage trigger: the platform hands on no bot's message"},
			{Path: "spec.chat.mentions", Unsupported: true, Problem: "mentions are not supported",
				Fix: "remove it: Agent Kourier posts nothing on the platform, so it notifies nobody"},
		}},
		{"a Slack channel's name on Slack, which the schema takes", slack(t), func(b *Binding) { b.Spec.Chat.Channel = "general" }, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: `"general" is not a channel ID`, Fix: "use the channel's ID, such as C0123456789 (G… for an older private channel), not its name"},
		}},
		{"a channel's name on kourier-v1, which needs no digit", platformOf(t, kourierV1Spec), func(b *Binding) {
			b.Spec.Chat.Channel = "general"
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, nil},
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
