package v1alpha1

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The Entra IDs of the tests' teams connections, fake GUIDs in lowercase hex, and a stand-in channel ID of the shape and length of a real one.
const (
	testTenant       = "11111111-2222-3333-4444-555555555555"
	testApp          = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	testTeamsChannel = "19:KourierStandInChannelForTestsOnly0000000000a@thread.tacv2"
)

// teamsChannelOf is a Teams channel ID of n bytes.
func teamsChannelOf(n int) string {
	return "19:" + strings.Repeat("a", n-len("19:@thread.tacv2")) + "@thread.tacv2"
}

// teamsRouteOfTheFixture is the route of chatconnection.yaml's connection, agent-kourier-system/slack-main, on the chat listener.
const teamsRouteOfTheFixture = "/chat/teams/agent-kourier-system/slack-main"

// teamsConn makes the fixture's connection a teams connection of the tests' tenant and app, with the notification URL url.
func teamsConn(o *ChatConnection, url string) {
	o.Spec.Platform = PlatformTeams
	o.Spec.Teams = &TeamsConnectionSpec{TenantID: testTenant, AppID: testApp, NotificationURL: url}
}

func TestTeamsConnectionValidate(t *testing.T) {
	const guidFix = "must be a GUID in lowercase hex, such as 11111111-2222-3333-4444-555555555555"
	runMutations[ChatConnection](t, "chatconnection.yaml", []mutation[ChatConnection]{
		{"teams ok", func(o *ChatConnection) { teamsConn(o, "") }, ""},
		{"teams with a notification URL ok", func(o *ChatConnection) { teamsConn(o, "https://kourier.example.com"+teamsRouteOfTheFixture) }, ""},
		{"teams with no teams block", func(o *ChatConnection) { o.Spec.Platform = PlatformTeams },
			"spec.teams: is required on a teams connection: it names the tenant and the app"},
		{"a teams block on a slack connection", func(o *ChatConnection) {
			o.Spec.Teams = &TeamsConnectionSpec{TenantID: testTenant, AppID: testApp}
		}, "spec.teams: is set, but only a teams connection has one"},
		{"a slack block on a teams connection", func(o *ChatConnection) {
			teamsConn(o, "")
			o.Spec.Slack = &SlackConnectionSpec{Mode: SlackModeHTTP}
		}, "spec.slack: is set, but only a slack connection has one"},
		{"no tenant", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.TenantID = "" }, "spec.teams.tenantID: required"},
		{"no app", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.AppID = "" }, "spec.teams.appID: required"},
		{"http mode ok", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.Mode = TeamsModeHTTP }, ""},
		{"socket mode ok", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.Mode = TeamsModeSocket }, ""},
		{"socket mode with a notification URL ok", func(o *ChatConnection) {
			teamsConn(o, "https://kourier.example.com"+teamsRouteOfTheFixture)
			o.Spec.Teams.Mode = TeamsModeSocket
		}, ""},
		{"an unknown mode", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.Mode = "webhook" },
			`spec.teams.mode: unsupported value "webhook", must be one of http, socket`},
		{"a mode in capitals", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.Mode = "HTTP" },
			`spec.teams.mode: unsupported value "HTTP", must be one of http, socket`},
		{"a tenant in capitals", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.TenantID = strings.ToUpper(testApp) },
			`spec.teams.tenantID: invalid tenant ID "66666666-7777-4888-9999-AAAAAAAAAAAA": ` + guidFix},
		{"a tenant in braces", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.TenantID = "{" + testTenant + "}" },
			"spec.teams.tenantID: invalid tenant ID: " + guidFix},
		{"a tenant's domain", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.TenantID = "contoso.onmicrosoft.com" },
			`spec.teams.tenantID: invalid tenant ID "contoso.onmicrosoft.com": ` + guidFix},
		{"an app ID with no hyphens", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.AppID = "66666666777748889999aaaaaaaaaaaa" },
			`spec.teams.appID: invalid application ID "66666666777748889999aaaaaaaaaaaa": ` + guidFix},
		{"an app ID one digit short", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.AppID = testApp[:35] },
			`spec.teams.appID: invalid application ID "66666666-7777-4888-9999-aaaaaaaaaaa": ` + guidFix},
		{"a Teams bot ID, 28:<app>, as the app ID", func(o *ChatConnection) { teamsConn(o, ""); o.Spec.Teams.AppID = "28:" + testApp },
			"spec.teams.appID: invalid application ID: " + guidFix},
	})
}

// A teams block on another platform is refused, and what is wrong inside it is told at the same time.
func TestATeamsBlockOnAnotherPlatformIsCheckedToo(t *testing.T) {
	c := load[ChatConnection](t, "chatconnection.yaml")
	c.Spec.Teams = &TeamsConnectionSpec{NotificationURL: "http://kourier.example.com" + teamsRouteOfTheFixture}
	want := []string{"spec.teams: is set, but only a teams connection has one", "spec.teams.tenantID: required", "spec.teams.appID: required",
		`spec.teams.notificationURL: invalid URL "http://kourier.example.com": must be an https URL with a host`}
	if got := problemsOf(c.Validate()); !slices.Equal(got, want) {
		t.Errorf("Validate() problems = %q, want %q", got, want)
	}
}

// The notification URL of a teams connection: https with a host, never http, credentials, a query or a fragment, at most maxURLLen
// bytes, and the path of the connection's own route on the chat listener, exactly. A message shows the scheme and the host at most,
// not a path, where a token may ride.
func TestTeamsNotificationURL(t *testing.T) {
	const https = "spec.teams.notificationURL: must be an https URL with a host"
	notHTTPS := func(shown string) string {
		return fmt.Sprintf("spec.teams.notificationURL: invalid URL %q: must be an https URL with a host", shown)
	}
	wrongPath := "spec.teams.notificationURL: must have the path " + teamsRouteOfTheFixture + ", the connection's route on the chat listener"
	route := teamsRouteOfTheFixture
	for _, tc := range []struct {
		url, want string
	}{
		{"https://kourier.example.com" + route, ""},
		{"HTTPS://Kourier.Example.com:8443" + route, ""},
		{"https://10.0.0.7" + route, ""},
		{"https://[2001:db8::1]:443" + route, ""},
		{"http://kourier.example.com" + route, notHTTPS("http://kourier.example.com")},
		{"http://localhost" + route, notHTTPS("http://localhost")},
		{"ftp://kourier.example.com" + route, notHTTPS("ftp://kourier.example.com")},
		{"kourier.example.com" + route, notHTTPS("")},
		{"https://" + route, notHTTPS("https:")},
		{"https://user:secret@kourier.example.com" + route, "spec.teams.notificationURL: must not contain credentials"},
		{"https://kourier.example.com" + route + "?token=x", "spec.teams.notificationURL: must not have a query or fragment"},
		{"https://kourier.example.com" + route + "#x", "spec.teams.notificationURL: must not have a query or fragment"},
		{"https://kourier.example.com/%zz", https},
		{"https://kourier.example.com", wrongPath},
		{"https://kourier.example.com/", wrongPath},
		{"https://kourier.example.com" + route + "/", wrongPath},
		{"https://kourier.example.com/chat/teams/sre/slack-main", wrongPath},
		{"https://kourier.example.com/chat/teams/agent-kourier-system/other", wrongPath},
		{"https://kourier.example.com/chat/slack/agent-kourier-system/slack-main", wrongPath},
		{"https://kourier.example.com/prefix" + route, wrongPath},
		{"https://kourier.example.com/chat/teams/agent-kourier-system/slack%2Dmain", wrongPath},
		{"https://kourier.example.com/CHAT/teams/agent-kourier-system/slack-main", wrongPath},
		{"https://k.example.com" + route + strings.Repeat("a", maxURLLen-len("https://k.example.com"+route)), wrongPath},
		{"https://k.example.com" + route + strings.Repeat("a", maxURLLen+1-len("https://k.example.com"+route)),
			fmt.Sprintf("spec.teams.notificationURL: is %d bytes, longer than the %d byte limit", maxURLLen+1, maxURLLen)},
	} {
		c := load[ChatConnection](t, "chatconnection.yaml")
		teamsConn(c, tc.url)
		var want []string
		if tc.want != "" {
			want = []string{tc.want}
		}
		if got := problemsOf(c.Validate()); !slices.Equal(got, want) {
			t.Errorf("url %.80q: Validate() problems = %q, want %q", tc.url, got, want)
		}
	}
}

// The path is the route of the connection that names it, whatever its namespace and name, and is not checked on a connection that
// has neither, which meta refuses: it has no route.
func TestTeamsNotificationURLIsTheConnectionsOwnRoute(t *testing.T) {
	for _, tc := range []struct {
		namespace, name, url string
		want                 []string
	}{
		{"team-a", "teams.main", "https://kourier.example.com/chat/teams/team-a/teams.main", nil},
		{"team-a", "teams.main", "https://kourier.example.com" + teamsRouteOfTheFixture,
			[]string{"spec.teams.notificationURL: must have the path /chat/teams/team-a/teams.main, the connection's route on the chat listener"}},
		{"", "", "https://kourier.example.com/anything", []string{"metadata.name: required", "metadata.namespace: required"}},
	} {
		c := load[ChatConnection](t, "chatconnection.yaml")
		teamsConn(c, tc.url)
		c.Namespace, c.Name = tc.namespace, tc.name
		if got := problemsOf(c.Validate()); !slices.Equal(got, tc.want) {
			t.Errorf("%s/%s, url %q: Validate() problems = %q, want %q", tc.namespace, tc.name, tc.url, got, tc.want)
		}
	}
}

// The mode of a teams connection is spec.teams.mode, and http when it is unset or there is no teams block: unlike Slack's, whose
// default is socket.
func TestTeamsMode(t *testing.T) {
	for _, tc := range []struct {
		teams *TeamsConnectionSpec
		want  TeamsMode
	}{
		{nil, TeamsModeHTTP},
		{&TeamsConnectionSpec{}, TeamsModeHTTP},
		{&TeamsConnectionSpec{Mode: TeamsModeHTTP}, TeamsModeHTTP},
		{&TeamsConnectionSpec{Mode: TeamsModeSocket}, TeamsModeSocket},
	} {
		if got := (ChatConnectionSpec{Platform: PlatformTeams, Teams: tc.teams}).TeamsMode(); got != tc.want {
			t.Errorf("TeamsMode(%+v) = %q, want %q", tc.teams, got, tc.want)
		}
	}
}

// A teams connection's Secret holds clientSecret, the Entra application's client secret, and one with no teams block has none, as
// Validate refuses it.
func TestTeamsCredentialKeys(t *testing.T) {
	if got := CredentialKeys(teamsSpec); !slices.Equal(got, []string{"clientSecret"}) {
		t.Errorf("CredentialKeys(teams) = %q, want [clientSecret]", got)
	}
	if got := CredentialKeys(ChatConnectionSpec{Platform: PlatformTeams}); got != nil {
		t.Errorf("CredentialKeys(teams with no teams block) = %q, want none", got)
	}
	keys := CredentialKeys(teamsSpec)
	keys[0] = "changed"
	if got := CredentialKeys(teamsSpec); got[0] != TeamsClientSecretKey {
		t.Errorf("CredentialKeys(teams) = %q after a caller changed its slice, want the table unchanged", got)
	}
	socket := &TeamsConnectionSpec{TenantID: testTenant, AppID: testApp, Mode: TeamsModeSocket}
	if got := CredentialKeys(ChatConnectionSpec{Platform: PlatformTeams, Teams: socket}); !slices.Equal(got, []string{TeamsClientSecretKey}) {
		t.Errorf("CredentialKeys(teams in socket mode) = %q, want [clientSecret]: the mode does not change the keys", got)
	}
	for _, teams := range []*TeamsConnectionSpec{nil, {}, teamsSpec.Teams, teamsAlertsSpec.Teams, socket, {TenantID: testTenant}} {
		c := load[ChatConnection](t, "chatconnection.yaml")
		c.Spec.Platform, c.Spec.Teams = PlatformTeams, teams
		if valid, keyed := c.Validate() == nil, CredentialKeys(c.Spec) != nil; valid && !keyed {
			t.Errorf("teams %+v: Validate accepts it, and CredentialKeys has no keys", teams)
		}
	}
}

// Microsoft Teams' shapes: a channel 19:<id>@thread.tacv2 or @thread.skype, at most maxChannelIDLen bytes, and a sender and a user
// that are Entra IDs, GUIDs in lowercase hex. Each is an alternative of its field's union, and no Slack shape takes a Teams ID nor
// a Teams shape a Slack one, so that the check at resolution tells the two apart.
func TestTeamsIDShapes(t *testing.T) {
	p := platformOf(t, teamsSpec)
	for _, tc := range []struct {
		name string
		s    IDShape
		in   string
		want bool
	}{
		{"channel", p.Channel, testTeamsChannel, true},
		{"channel", p.Channel, "19:a1b2c3d4e5f60718293a4b5c6d7e8f90@thread.tacv2", true},
		{"channel", p.Channel, "19:a1b2c3d4e5f6@thread.skype", true},
		{"channel", p.Channel, "19:a.b_c-d@thread.tacv2", true},
		{"channel", p.Channel, teamsChannelOf(maxChannelIDLen), true},
		{"channel", p.Channel, teamsChannelOf(maxChannelIDLen + 1), false},
		{"channel", p.Channel, "19:@thread.tacv2", false},
		{"channel", p.Channel, "19:abc@thread.v2", false},
		{"channel", p.Channel, testTeamsChannel + ";messageid=1700000000000", false},
		{"channel", p.Channel, "29:abc@thread.tacv2", false},
		{"channel", p.Channel, "19:a b@thread.tacv2", false},
		{"channel", p.Channel, "19:abc@unq.gbl.spaces", false},
		{"channel", p.Channel, "C0123456789", false},
		{"channel", p.Channel, "General", false},
		{"user", p.User, testTenant, true},
		{"user", p.User, strings.ToUpper(testApp), false},
		{"user", p.User, "29:1a2b3c", false},
		{"user", p.User, "U0123456789", false},
		{"user", p.User, "sam@contoso.com", false},
		{"sender", p.Sender, testApp, true},
		{"sender", p.Sender, "28:" + testApp, false},
		{"sender", p.Sender, "B0123456789", false},
		{"sender", p.Sender, "{" + testApp + "}", false},
	} {
		if got := tc.s.Matches(tc.in); got != tc.want {
			t.Errorf("Teams' %s shape Matches(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
	if p.Group.Taken() {
		t.Errorf("Teams has a Group shape: %+v; want none, as it has no approver groups", p)
	}
	slackP := slack(t)
	for _, tc := range []struct {
		field        string
		slack, teams IDShape
		slackID      string
		teamsID      string
	}{
		{"channel", slackP.Channel, p.Channel, "C0123456789", testTeamsChannel},
		{"sender", slackP.Sender, p.Sender, "B0123456789", testApp},
		{"user", slackP.User, p.User, "U0123456789", testTenant},
	} {
		if tc.slack.Matches(tc.teamsID) || tc.teams.Matches(tc.slackID) {
			t.Errorf("%s: a Slack shape takes the Teams ID %q, or the Teams shape the Slack ID %q", tc.field, tc.teamsID, tc.slackID)
		}
	}
}

// The approver groups and the trigger types of each platform: Slack reads its user groups, and Microsoft Teams has none; both take
// mention, botMessage and personMessage triggers, Teams in either mode and with or without a notification URL. Every platform with
// a Group shape takes tool approvals, for the groups to decide.
func TestTheGroupsAndTriggersOfEachPlatform(t *testing.T) {
	chat := []TriggerType{TriggerMention, TriggerBotMessage, TriggerPersonMessage}
	for _, tc := range []struct {
		name   string
		spec   ChatConnectionSpec
		groups bool
	}{
		{"slack", slackSpec, true},
		{"teams", teamsSpec, false},
		{"teams with a notification URL", teamsAlertsSpec, false},
		{"teams in socket mode", ChatConnectionSpec{Platform: PlatformTeams, Teams: &TeamsConnectionSpec{TenantID: testTenant, AppID: testApp, Mode: TeamsModeSocket}}, false},
	} {
		p := platformOf(t, tc.spec)
		if p.Group.Taken() != tc.groups || !slices.Equal(p.Triggers, chat) {
			t.Errorf("%s: Group taken %v, Triggers %v; want %v, %v", tc.name, p.Group.Taken(), p.Triggers, tc.groups, chat)
		}
		if p.Group.Taken() && !p.ToolApprovals {
			t.Errorf("%s: a Group shape with no tool approvals", tc.name)
		}
	}
}

func TestPlatformMismatchesOnTeams(t *testing.T) {
	const (
		channelFix = "use the channel's ID, 19:…@thread.tacv2 (or @thread.skype), not its name"
		guid       = "a GUID in lowercase hex, such as 11111111-2222-3333-4444-555555555555"
		senderFix  = "use the Microsoft Entra application ID of the app that posts, " + guid
		userFix    = "use the person's Microsoft Entra object ID, " + guid
		groupsFix  = "remove it: any channel member who is not a bot may then decide a tool approval"
		channel    = testTeamsChannel
	)
	teamsIDs := func(b *Binding) {
		b.Spec.Chat.Channel = channel
		bot(b).From.ID = testApp
		b.Spec.Chat.Mentions = []ChatMention{{Name: "sam", User: testTenant}}
	}
	for _, tc := range []struct {
		name     string
		platform ChatPlatform
		mutate   func(*Binding)
		want     []PlatformMismatch
	}{
		{"a Teams Binding with alerts on a connection with a notification URL", platformOf(t, teamsAlertsSpec), func(b *Binding) {
			teamsIDs(b)
			b.Spec.Interactions.ToolApprovals, b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = true, true, true
		}, nil},
		{"a Teams Binding with an alert trigger on a connection with no notification URL", platformOf(t, teamsSpec), func(b *Binding) {
			teamsIDs(b)
		}, nil},
		{"a Slack bot as the sender on a connection with no notification URL", platformOf(t, teamsSpec), func(b *Binding) {
			teamsIDs(b)
			bot(b).From.ID = "B0123456789"
		}, []PlatformMismatch{
			{Path: "spec.chat.triggers[1].from.id", Problem: `"B0123456789" is not an application ID`, Fix: senderFix},
		}},
		{"Slack's IDs, in the order of the fields", platformOf(t, teamsAlertsSpec), func(b *Binding) {
			b.Spec.Chat.Mentions = []ChatMention{{Name: "sam", User: "U0123456789"}}
		}, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: `"C0123456789" is not a channel ID`, Fix: channelFix},
			{Path: "spec.chat.triggers[1].from.id", Problem: `"B0123456789" is not an application ID`, Fix: senderFix},
			{Path: "spec.chat.mentions[0].user", Problem: `"U0123456789" is not a Microsoft Entra object ID`, Fix: userFix},
		}},
		{"a Slack channel's ID past Slack's cap and within Teams': the shape, and not the length", platformOf(t, teamsAlertsSpec), func(b *Binding) {
			teamsIDs(b)
			b.Spec.Chat.Channel = "C" + strings.Repeat("0", maxSlackIDLen)
		}, []PlatformMismatch{
			{Path: "spec.chat.channel", Problem: `"C` + strings.Repeat("0", maxSlackIDLen) + `" is not a channel ID`, Fix: channelFix},
		}},
		{"approver groups, refused whatever their shape", platformOf(t, teamsAlertsSpec), func(b *Binding) {
			teamsIDs(b)
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Interactions.ApproverGroups = []string{"S0123456789", "S9876543210"}
		}, []PlatformMismatch{
			{Path: "spec.interactions.approverGroups", Unsupported: true, Problem: "approver groups are not supported", Fix: groupsFix},
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

// A Teams Binding on Slack is refused for each of its IDs: the channel, the sender and the user have shapes of the union and none
// of Slack's. A Slack channel past Slack's cap and within the union's is refused for its length.
func TestATeamsBindingOnSlack(t *testing.T) {
	b := load[Binding](t, "binding.yaml")
	b.Spec.Chat.Channel = testTeamsChannel
	bot(b).From.ID = testApp
	b.Spec.Chat.Mentions = []ChatMention{{Name: "sam", User: testTenant}}
	if err := b.Validate(); err != nil {
		t.Fatalf("the Binding does not pass Validate: %v", err)
	}
	var paths []string
	for _, m := range b.Spec.PlatformMismatches(slack(t)) {
		if m.Unsupported {
			t.Errorf("%s: unsupported, want an ID that is not Slack's", m.Path)
		}
		paths = append(paths, m.Path)
	}
	if want := []string{"spec.chat.channel", "spec.chat.triggers[1].from.id", "spec.chat.mentions[0].user"}; !slices.Equal(paths, want) {
		t.Errorf("mismatches at %q, want %q", paths, want)
	}

	long := load[Binding](t, "binding.yaml")
	long.Spec.Chat.Channel = "C" + strings.Repeat("0", maxSlackIDLen)
	if err := long.Validate(); err != nil {
		t.Fatalf("a Slack channel within the union's cap does not pass Validate: %v", err)
	}
	want := []PlatformMismatch{{Path: "spec.chat.channel", Problem: "is 33 bytes, longer than the 32 byte limit of a channel ID",
		Fix: "use the channel's ID, such as C0123456789 (G… for an older private channel), not its name"}}
	if got := long.Spec.PlatformMismatches(slack(t)); !reflect.DeepEqual(got, want) {
		t.Errorf("PlatformMismatches = %+v, want %+v", got, want)
	}
}

// The schema's rules of spec.teams.notificationURL refuse nothing that Validate accepts, on a few hundred thousand seeded strings,
// with no cluster, as TestCELURLRulesRefuseOnlyWhatURLParseRefuses does for an AgentBackend's url. The rule of the path is
// looser than Validate, which reads the object's namespace and name: the schema checks the form, and the broker the connection.
func TestCELTeamsNotificationURLRulesRefuseOnlyWhatValidateRefuses(t *testing.T) {
	var rules []orRule
	for _, r := range crdSchema(t, "chatconnections").at(t, "spec.teams.notificationURL").rules() {
		rules = append(rules, parseOrRule(t, r.Rule))
	}
	if len(rules) != 6 {
		t.Fatalf("want 6 notificationURL rules, found %d", len(rules))
	}
	refusedByRules := func(u string) bool {
		return slices.ContainsFunc(rules, func(r orRule) bool { return !r.passes(u) })
	}
	broker := func(u string) bool {
		c := &ChatConnection{Spec: ChatConnectionSpec{Platform: PlatformTeams, CredentialsSecretRef: SecretRef{Name: "s"},
			Teams: &TeamsConnectionSpec{TenantID: testTenant, AppID: testApp, NotificationURL: u}}}
		c.Name, c.Namespace = "a.b", "ns-1"
		return c.Validate() == nil
	}

	schemes := []string{"https://", "HTTPS://", "hTTps://", "http://", "https:", "https:/", "https:///", "ftp://", "//", ""}
	hosts := []string{"h", "kourier.example.com", "10.0.0.7", "[::1]", "[2001:db8::1]", "h:443", "h:", "u:p@h", "@h", "h h", "h\\", "h^", "h`", "h{", "h|", "", "%41", "é"}
	pieces := []string{"/chat/teams/ns-1/a.b", "/chat/teams/ns-1/a.b", "/chat/teams/ns-1/a.b", "/chat/teams/ns-1/", "/chat/teams/", "/chat/slack/ns-1/a.b",
		"/chat/teams/NS-1/a.b", "/", "//", "a", ".b", "-", "#", "?", "?q", "#f", "%", "%2D", "%zz", "@", ":", " ", "\t", "\n", "\x00", "\x7f", "\\", "é", "~"}
	r := rand.New(rand.NewPCG(5, 6)) // seeded, so that a failure repeats
	var accepted, refused int
	for range 300000 {
		var b strings.Builder
		if r.IntN(10) < 9 {
			b.WriteString(schemes[r.IntN(len(schemes))])
		}
		b.WriteString(hosts[r.IntN(len(hosts))])
		if r.IntN(2) == 0 {
			b.WriteString("/chat/teams/ns-1/a.b")
		}
		for range r.IntN(2) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		u := b.String()
		if u == "" {
			continue
		}
		ok := broker(u)
		switch {
		case ok && refusedByRules(u):
			t.Errorf("the rules refuse %q, which Validate accepts", u)
		case ok:
			accepted++
		case refusedByRules(u):
			refused++
		}
	}
	t.Logf("%d URLs accepted by Validate, %d refused by Validate and the rules", accepted, refused)
	if accepted < 2000 || refused < 5000 {
		t.Errorf("the generator no longer reaches every side: accepted %d, refused by the rules %d", accepted, refused)
	}
}

// orRule is one rule of spec.teams.notificationURL, a disjunction of terms (a test for the empty string, `self.matches(...)` and
// `!self.matches(...)`), as Go evaluates it: CEL's matches is Go's regexp, so a term is its regex.
type orRule struct {
	terms []func(string) bool
}

func (r orRule) passes(u string) bool {
	return slices.ContainsFunc(r.terms, func(term func(string) bool) bool { return term(u) })
}

func parseOrRule(t *testing.T, rule string) orRule {
	t.Helper()
	var out orRule
	for term := range strings.SplitSeq(rule, " || ") {
		if term == "self == ''" {
			out.terms = append(out.terms, func(u string) bool { return u == "" })
			continue
		}
		m := regexp.MustCompile(`^(!?)self\.matches\(r?'(.*)'\)$`).FindStringSubmatch(term)
		if m == nil {
			t.Fatalf("url rule %q: the term %q is not self == '' or [!]self.matches('regex')", rule, term)
		}
		re, err := regexp.Compile(m[2])
		if err != nil {
			t.Fatalf("url rule %q: %v", rule, err)
		}
		negated := m[1] == "!"
		out.terms = append(out.terms, func(u string) bool { return re.MatchString(u) != negated })
	}
	return out
}
