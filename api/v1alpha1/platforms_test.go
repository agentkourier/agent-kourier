package v1alpha1

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The specs of a connection on each platform and profile that Validate accepts.
var (
	slackSpec     = ChatConnectionSpec{Platform: PlatformSlack}
	kourierV1Spec = ChatConnectionSpec{Platform: PlatformWeb, Web: &WebConnectionSpec{Profile: WebProfileKourierV1, URL: "https://receiver.example.com/replies"}}
	githubSpec    = ChatConnectionSpec{Platform: PlatformWeb, Web: &WebConnectionSpec{Profile: WebProfileGitHub}}
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

// alternatives splits a union pattern, ^(?:a|b|c)$, into its top-level alternatives.
func alternatives(t *testing.T, union string) []string {
	t.Helper()
	inner, prefixed := strings.CutPrefix(union, "^(?:")
	inner, suffixed := strings.CutSuffix(inner, ")$")
	if !prefixed || !suffixed {
		t.Fatalf("%s is not of the form ^(?:a|b)$", union)
	}
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '\\':
			i++ // the escaped byte is a literal
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
// the union takes nothing that no platform does. Slack's comes first, as it always has.
func TestEveryPlatformsShapesAreTheAlternativesOfTheUnion(t *testing.T) {
	platforms := []ChatPlatform{slack(t), platformOf(t, githubSpec), platformOf(t, kourierV1Spec)}
	for _, tc := range []struct {
		field string
		union string
		shape func(ChatPlatform) IDShape
	}{
		{"channel", channelID.String(), func(p ChatPlatform) IDShape { return p.Channel }},
		{"sender", senderID.String(), func(p ChatPlatform) IDShape { return p.Sender }},
		{"user", mentionUser.String(), func(p ChatPlatform) IDShape { return p.User }},
		{"group", approverGroupID.String(), func(p ChatPlatform) IDShape { return p.Group }},
	} {
		alts := alternatives(t, tc.union)
		var shapes []string
		for _, p := range platforms {
			inner := strings.TrimSuffix(strings.TrimPrefix(tc.shape(p).pattern.String(), "^"), "$")
			if !slices.Contains(alts, inner) {
				t.Errorf("%s: %s's shape %s is not an alternative of the union %s", tc.field, p.Name, inner, tc.union)
			}
			shapes = append(shapes, inner)
		}
		if alts[0] != strings.TrimSuffix(strings.TrimPrefix(tc.shape(platforms[0]).pattern.String(), "^"), "$") {
			t.Errorf("%s: the union %s does not start with Slack's shape", tc.field, tc.union)
		}
		for _, a := range alts {
			if !slices.Contains(shapes, a) {
				t.Errorf("%s: the alternative %s of the union is no platform's shape", tc.field, a)
			}
		}
	}
	// The lengths too: the union's caps are no less than any platform's.
	for _, p := range platforms {
		for _, tc := range []struct {
			name string
			cap  int
			s    IDShape
		}{{"channel", maxChatIDLen, p.Channel}, {"sender", maxChatIDLen, p.Sender}, {"user", maxMentionUserLen, p.User}, {"group", maxChatIDLen, p.Group}} {
			if tc.s.maxLen > tc.cap {
				t.Errorf("%s: %s's cap %d is over the union's %d", tc.name, p.Name, tc.s.maxLen, tc.cap)
			}
		}
	}
}

// A name such as general is still no platform's channel or group ID, so that the API server refuses it when it is applied.
func TestAChannelNameIsNoPlatformsID(t *testing.T) {
	for _, name := range []string{"general", "incidents", "team-platform", "ops_alerts", "#general", "general.alerts"} {
		if channelID.MatchString(name) {
			t.Errorf("the channel union takes the name %q", name)
		}
		if approverGroupID.MatchString(name) {
			t.Errorf("the approver group union takes the name %q", name)
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

// GitHub's shapes, the web platform's github profile's: a repository as a channel, a login as a sender or a user, a team as a group.
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
		{"sender", p.Sender, "dependabot[bot]", true},
		{"sender", p.Sender, "octocat", true},
		{"sender", p.Sender, owner + "[bot]", true},
		{"sender", p.Sender, owner + "a", false},
		{"sender", p.Sender, "B0123456789", true}, // it has a login's shape
		{"sender", p.Sender, "octo.cat", false},
		{"user", p.User, "octocat", true},
		{"user", p.User, "octo_cat", false},
		{"group", p.Group, "octo-org/sre", true},
		{"group", p.Group, "sre", false},
		{"group", p.Group, "S0123456789", false},
	} {
		if got := tc.shape.Matches(tc.in); got != tc.want {
			t.Errorf("github's %s shape Matches(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// The kourier-v1 profile's shapes: an opaque ID of letters, digits, '.', '_' and '-', starting with a letter or a digit, at most
// maxKourierV1IDLen bytes; a channel or a group has a digit too, so that a name such as general is not one.
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
		{"channel", p.Channel, "general", false},
		{"channel", p.Channel, "ops.alerts_x-y", false},
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
		{"group", p.Group, "sre", false},
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
	webs := []*WebConnectionSpec{nil, {}, {Profile: WebProfileKourierV1, URL: "https://r.example.com"}, {Profile: WebProfileGitHub},
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
// take an answer; the github profile posts nothing, so none is its only output and its default.
func TestWhatEachPlatformServes(t *testing.T) {
	for _, tc := range []struct {
		spec                                      ChatConnectionSpec
		name                                      string
		replies, askUser, toolApprovals, feedback bool
		output                                    OutputMode
	}{
		{slackSpec, "Slack", true, true, true, true, OutputLive},
		{kourierV1Spec, "web (kourier-v1 profile)", true, false, false, false, OutputLive},
		{githubSpec, "web (github profile)", false, false, false, false, OutputNone},
	} {
		p := platformOf(t, tc.spec)
		if p.Name != tc.name || p.Replies != tc.replies || p.AskUser != tc.askUser || p.ToolApprovals != tc.toolApprovals || p.Feedback != tc.feedback {
			t.Errorf("%s: %+v; want replies %v, askUser %v, toolApprovals %v, feedback %v", tc.name, p, tc.replies, tc.askUser, tc.toolApprovals, tc.feedback)
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
			{Path: "spec.chat.channel", Problem: `"octo-org/hello-world" is not a channel ID`, Fix: "use the channel's ID as the sender sends it: letters, digits, '.', '_' and '-', with a digit, such as ch-1"},
			{Path: "spec.chat.triggers[1].from.id", Problem: `"dependabot[bot]" is not a sender ID`, Fix: "use the sender's ID as the sender sends it: letters, digits, '.', '_' and '-'"},
		}},
		{"a kourier-v1 ID over its length", platformOf(t, kourierV1Spec), func(b *Binding) {
			b.Spec.Chat.Channel = "c" + strings.Repeat("1", maxKourierV1IDLen)
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: "is 65 bytes, longer than the 64 byte limit of a channel ID",
				Fix: "use the channel's ID as the sender sends it: letters, digits, '.', '_' and '-', with a digit, such as ch-1"},
		}},
		{"a github Binding on github, its output left out", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			b.Spec.Chat.Output = ""
			bot(b).From.ID = "dependabot[bot]"
			b.Spec.Chat.Mentions = []ChatMention{{Name: "oct", User: "octocat"}}
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, nil},
		{"output none on github", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			b.Spec.Chat.Output = OutputNone
			bot(b).From.ID = "dependabot[bot]"
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, nil},
		{"what github does not serve, in the order of the fields", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "C0123456789"
			bot(b).From.ID = "dependabot[bot]"
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Interactions.ApproverGroups = []string{"octo-org/sre"}
		}, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: `"C0123456789" is not a repository`, Fix: "use the repository's owner/name, such as octo-org/hello-world"},
			{Path: "spec.chat.output", Unsupported: true, Problem: `"live" is not supported`, Fix: "set it to none, or leave it out: the agent replies itself, and Agent Kourier posts nothing"},
			{Path: "spec.interactions.askUser", Unsupported: true, Problem: "questions to a person are not supported", Fix: "set it to false"},
			{Path: "spec.interactions.toolApprovals", Unsupported: true, Problem: "tool approvals are not supported",
				Fix: "set it to false and remove spec.interactions.approverGroups, which requires it"},
			{Path: "spec.feedback.enabled", Unsupported: true, Problem: "feedback buttons are not supported", Fix: "set it to false"},
		}},
		{"output final on github", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			b.Spec.Chat.Output = OutputFinal
			bot(b).From.ID = "dependabot[bot]"
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, []PlatformMismatch{
			{Path: "spec.chat.output", Unsupported: true, Problem: `"final" is not supported`, Fix: "set it to none, or leave it out: the agent replies itself, and Agent Kourier posts nothing"},
		}},
		{"GitHub IDs of the wrong kind", platformOf(t, githubSpec), func(b *Binding) {
			b.Spec.Chat.Channel = "octo-org/hello-world"
			b.Spec.Chat.Output = ""
			bot(b).From.ID = "ci.runner_2"
			b.Spec.Chat.Mentions = []ChatMention{{Name: "oct", User: "U0123456789"}, {Name: "kim", User: "kim.lee"}}
			b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
		}, []PlatformMismatch{
			{Path: "spec.chat.triggers[1].from.id", Problem: `"ci.runner_2" is not a login`, Fix: "use the account's login, such as dependabot[bot] for an app"},
			{Path: "spec.chat.mentions[1].user", Problem: `"kim.lee" is not a login`, Fix: "use the person's login, such as octocat"},
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
