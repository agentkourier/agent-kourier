package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// The IDs of the tests' Teams connection and Bindings: fake GUIDs, and channels of Teams' shape.
const (
	teamsTenant  = "11111111-2222-3333-4444-555555555555"
	teamsApp     = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	teamsSender  = "0b7a9c1e-3f2d-4a5b-8c6d-9e0f1a2b3c4d"
	teamsUser    = "7c1d2e3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f"
	teamsChannel = "19:KourierStandInChannelForTestsOnly0000000000a@thread.tacv2"
)

// onTeams makes the spec set's connection a teams connection, with the notification URL url (none when it is empty), and its
// Bindings Bindings that the platform takes: a channel and a sender of Teams' shapes. The secret is the platform's.
func onTeams(t *testing.T, url string) (*Set, fakeSecrets) {
	t.Helper()
	set := specSet(t)
	conn := &set.ChatConnections[0]
	conn.Spec.Platform = v1alpha1.PlatformTeams
	conn.Spec.Teams = &v1alpha1.TeamsConnectionSpec{TenantID: teamsTenant, AppID: teamsApp, NotificationURL: url}
	b := sreAlerts(set)
	b.Spec.Chat.Channel, b.Spec.Chat.Triggers[1].From.ID = teamsChannel, teamsSender
	chatOnly := &set.Bindings[slices.IndexFunc(set.Bindings, func(b v1alpha1.Binding) bool { return b.Name == "chat-only" })]
	chatOnly.Spec.Chat.Channel = "19:general@thread.skype"
	if err := conn.Validate(); err != nil {
		t.Fatalf("the teams connection does not validate: %v", err)
	}
	secrets := specSecrets()
	secrets["agent-kourier-system/slack-main"] = v1alpha1.CredentialKeys(conn.Spec)
	return set, secrets
}

// The notification URL of the tests' Teams connection, the route of agent-kourier-system/slack-main.
const teamsNotificationURL = "https://kourier.example.com/chat/teams/agent-kourier-system/slack-main"

// On Microsoft Teams a Binding is held to Teams' shapes; it may ask questions, approve tool calls and show the feedback buttons, but
// may not name approver groups; and it may have a botMessage trigger with or without the connection's notification URL. Each refusal
// is in the order of the fields, with InvalidPlatformID for an ID and UnsupportedOnPlatform for a feature, on both resolutions.
func TestResolutionOnTeams(t *testing.T) {
	const on = " on Microsoft Teams, the platform of ChatConnection agent-kourier-system/slack-main: "
	page := " (see " + PlatformsPage + ")"
	tests := []struct {
		name     string
		url      string
		mutate   func(*v1alpha1.Binding)
		rejected []Rejection
	}{
		{"a Teams Binding with an alert trigger, on a connection with a notification URL", teamsNotificationURL, func(b *v1alpha1.Binding) {
			b.Spec.Interactions.ToolApprovals = true
		}, nil},
		{"a Slack channel", teamsNotificationURL, func(b *v1alpha1.Binding) { b.Spec.Chat.Channel = "C0123456789" }, []Rejection{
			rejected("sre-alerts", ReasonInvalidPlatformID, `spec.chat.channel: "C0123456789" is not a channel ID`+on+
				"use the channel's ID, 19:…@thread.tacv2 (or @thread.skype), not its name"+page)}},
		{"a Slack user as a mention", teamsNotificationURL, func(b *v1alpha1.Binding) {
			b.Spec.Chat.Mentions = []v1alpha1.ChatMention{{Name: "sam", User: "U0123456789"}}
		}, []Rejection{
			rejected("sre-alerts", ReasonInvalidPlatformID, `spec.chat.mentions[0].user: "U0123456789" is not a Microsoft Entra object ID`+on+
				"use the person's Microsoft Entra object ID, a GUID in lowercase hex, such as 11111111-2222-3333-4444-555555555555"+page)}},
		{"a Slack bot as the sender", teamsNotificationURL, func(b *v1alpha1.Binding) { b.Spec.Chat.Triggers[1].From.ID = "B0123456789" }, []Rejection{
			rejected("sre-alerts", ReasonInvalidPlatformID, `spec.chat.triggers[1].from.id: "B0123456789" is not an application ID`+on+
				"use the Microsoft Entra application ID of the app that posts, a GUID in lowercase hex, such as 11111111-2222-3333-4444-555555555555"+page)}},
		{"approver groups", teamsNotificationURL, func(b *v1alpha1.Binding) {
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Interactions.ApproverGroups = []string{"S0123456789"}
		}, []Rejection{
			rejected("sre-alerts", ReasonUnsupportedOnPlatform, "spec.interactions.approverGroups: approver groups are not supported"+on+
				"remove it: any channel member who is not a bot may then decide a tool approval"+page)}},
		{"a Teams Binding with an alert trigger, on a connection with no notification URL", "", func(*v1alpha1.Binding) {}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set, _ := onTeams(t, tc.url)
			tc.mutate(sreAlerts(set))
			snap, got := ResolvePartial(set)
			if !slices.Equal(got, tc.rejected) {
				t.Errorf("rejections:\n%+v\nwant:\n%+v", got, tc.rejected)
			}
			want := []string{"sre/chat-only", "sre/sre-alerts"}
			if len(tc.rejected) > 0 {
				want = want[:1]
			}
			if s := served(snap); !slices.Equal(s, want) {
				t.Errorf("served = %v; want %v", s, want)
			}

			set, secrets := onTeams(t, tc.url)
			tc.mutate(sreAlerts(set))
			if len(tc.rejected) == 0 {
				if _, err := Resolve(t.Context(), set, secrets); err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				return
			}
			var problems []string
			for _, r := range tc.rejected {
				problems = append(problems, atSREAlerts+"Binding sre/sre-alerts: "+r.Message)
			}
			wantProblems(t, set, secrets, problems...)

			set, _ = onTeams(t, tc.url)
			tc.mutate(sreAlerts(set))
			if reason, _ := RefusedOnPlatform(sreAlerts(set), &set.ChatConnections[0], v1alpha1.ChatPlatformOf); reason != tc.rejected[0].Reason {
				t.Errorf("RefusedOnPlatform = %q; want %q", reason, tc.rejected[0].Reason)
			}
		})
	}
}

// Teams' IDs on a Slack connection pass Validate, whose union takes them, and are refused with InvalidPlatformID, each in the order
// of the fields, by both resolutions and by RefusedOnPlatform.
func TestTeamsIDsOnSlackAreRefused(t *testing.T) {
	set := specSet(t)
	b := sreAlerts(set)
	b.Spec.Chat.Channel, b.Spec.Chat.Triggers[1].From.ID = teamsChannel, teamsSender
	b.Spec.Chat.Mentions = []v1alpha1.ChatMention{{Name: "sam", User: teamsUser}}
	if err := b.Validate(); err != nil {
		t.Fatalf("Teams' IDs do not pass Validate: %v", err)
	}
	want := []Rejection{
		rejected("sre-alerts", ReasonInvalidPlatformID, onSlack("spec.chat.channel", `"`+teamsChannel+`" is not a channel ID`, channelFix)),
		rejected("sre-alerts", ReasonInvalidPlatformID, onSlack("spec.chat.triggers[1].from.id", `"`+teamsSender+`" is not a bot ID`, botFix)),
		rejected("sre-alerts", ReasonInvalidPlatformID, onSlack("spec.chat.mentions[0].user", `"`+teamsUser+`" is not a user ID`, "use the person's user ID, such as U0123456789")),
	}
	if _, got := ResolvePartial(set); !slices.Equal(got, want) {
		t.Errorf("rejections:\n%+v\nwant:\n%+v", got, want)
	}
	var problems []string
	for _, r := range want {
		problems = append(problems, atSREAlerts+"Binding sre/sre-alerts: "+r.Message)
	}
	wantProblems(t, set, specSecrets(), problems...)
	if reason, _ := RefusedOnPlatform(b, &set.ChatConnections[0], v1alpha1.ChatPlatformOf); reason != ReasonInvalidPlatformID {
		t.Errorf("RefusedOnPlatform = %q; want %q", reason, ReasonInvalidPlatformID)
	}
}

// A teams connection's Secret must hold clientSecret: the files route checks it when it resolves, without reading the value.
func TestATeamsConnectionsSecretNeedsTheClientSecret(t *testing.T) {
	set, secrets := onTeams(t, "")
	if _, err := Resolve(t.Context(), set, secrets); err != nil {
		t.Fatalf("Resolve with the client secret: %v", err)
	}
	secrets["agent-kourier-system/slack-main"] = []string{"botToken", "appToken"}
	if _, err := Resolve(t.Context(), set, secrets); err == nil || !strings.Contains(err.Error(), v1alpha1.TeamsClientSecretKey) {
		t.Errorf("Resolve with a Secret of Slack's keys = %v; want a problem that names %s", err, v1alpha1.TeamsClientSecretKey)
	}
}
