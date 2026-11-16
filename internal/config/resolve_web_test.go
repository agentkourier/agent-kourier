package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// outputOf is the output that the Snapshot serves sre/name with, and false if it does not serve it.
func outputOf(snap *Snapshot, name string) (v1alpha1.OutputMode, bool) {
	rb, ok := snap.Binding("sre", name)
	if !ok {
		return "", false
	}
	return rb.Binding.Spec.Chat.Output, true
}

// A Binding that leaves its output out is served with its platform's default, live on Slack, which resolution fills in on the copy
// it serves and not in the Set; one that names an output keeps it. A stored Slack Binding that says live is served as before.
func TestResolutionFillsTheOutputOfTheSlackPlatform(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  v1alpha1.OutputMode
		want v1alpha1.OutputMode
	}{
		{"left out", "", v1alpha1.OutputLive},
		{"live, as a stored Binding says", v1alpha1.OutputLive, v1alpha1.OutputLive},
		{"final", v1alpha1.OutputFinal, v1alpha1.OutputFinal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolvers := map[string]func(*Set) *Snapshot{
				"ResolvePartial": func(s *Set) *Snapshot { snap, _ := ResolvePartial(s); return snap },
				"Resolve": func(s *Set) *Snapshot {
					snap, err := Resolve(t.Context(), s, specSecrets())
					if err != nil {
						t.Fatalf("Resolve: %v", err)
					}
					return snap
				},
			}
			for name, resolve := range resolvers {
				set := specSet(t)
				sreAlerts(set).Spec.Chat.Output = tc.set
				b := sreAlerts(set)
				snap := resolve(set)
				if got, ok := outputOf(snap, "sre-alerts"); !ok || got != tc.want {
					t.Errorf("%s: sre-alerts is served with output %q (served %v); want %q", name, got, ok, tc.want)
				}
				if b.Spec.Chat.Output != tc.set {
					t.Errorf("%s: the Set's Binding has output %q after resolution; want it left as %q", name, b.Spec.Chat.Output, tc.set)
				}
			}
		})
	}
	// The minimal example names no output at all.
	snap, _ := ResolvePartial(specSet(t))
	if got, _ := outputOf(snap, "chat-only"); got != v1alpha1.OutputLive {
		t.Errorf("chat-only, which names no output, is served with %q; want live", got)
	}
}

// Output none, which posts nothing, is refused on Slack, where Agent Kourier posts the replies: UnsupportedOnPlatform, as a feature
// the platform does not serve, and the Binding is not served.
func TestOutputNoneIsUnsupportedOnSlack(t *testing.T) {
	const msg = `spec.chat.output: "none" is not supported on Slack, the platform of ChatConnection agent-kourier-system/slack-main: ` +
		"set it to live or final, or leave it out for live (see " + PlatformsPage + ")"
	set := specSet(t)
	sreAlerts(set).Spec.Chat.Output = v1alpha1.OutputNone
	snap, got := ResolvePartial(set)
	if want := []Rejection{rejected("sre-alerts", ReasonUnsupportedOnPlatform, msg)}; !slices.Equal(got, want) {
		t.Errorf("rejections:\n%+v\nwant:\n%+v", got, want)
	}
	if s := served(snap); !slices.Equal(s, []string{"sre/chat-only"}) {
		t.Errorf("served = %v; want chat-only alone", s)
	}
	set = specSet(t)
	sreAlerts(set).Spec.Chat.Output = v1alpha1.OutputNone
	wantProblems(t, set, specSecrets(), atSREAlerts+"Binding sre/sre-alerts: "+msg)

	set = specSet(t)
	sreAlerts(set).Spec.Chat.Output = v1alpha1.OutputNone
	if reason, message := RefusedOnPlatform(sreAlerts(set), &set.ChatConnections[0]); reason != ReasonUnsupportedOnPlatform || message != msg {
		t.Errorf("RefusedOnPlatform = %q, %q; want %q, %q", reason, message, ReasonUnsupportedOnPlatform, msg)
	}
}

// onWeb makes the spec set's connection a web connection with the profile, and sre-alerts a Binding that the profile takes: a
// channel and a sender of its shapes, and none of what it does not serve. The secret is the profile's.
func onWeb(t *testing.T, profile v1alpha1.WebProfile) (*Set, fakeSecrets) {
	t.Helper()
	set := specSet(t)
	conn := &set.ChatConnections[0]
	conn.Spec.Platform = v1alpha1.PlatformWeb
	conn.Spec.Web = &v1alpha1.WebConnectionSpec{Profile: profile}
	b := sreAlerts(set)
	b.Spec.Chat.Output = ""
	b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
	chatOnly := &set.Bindings[slices.IndexFunc(set.Bindings, func(b v1alpha1.Binding) bool { return b.Name == "chat-only" })]
	switch profile {
	case v1alpha1.WebProfileGitHub:
		b.Spec.Chat.Channel, b.Spec.Chat.Triggers[1].From.ID = "octo-org/hello-world", "dependabot[bot]"
		chatOnly.Spec.Chat.Channel = "octo-org/chat"
	case v1alpha1.WebProfileKourierV1:
		conn.Spec.Web.URL = "https://receiver.example.com/replies"
		b.Spec.Chat.Channel, b.Spec.Chat.Triggers[1].From.ID = "ch-1", "billing-bot"
		chatOnly.Spec.Chat.Channel = "ch-2"
	}
	if err := conn.Validate(); err != nil {
		t.Fatalf("the web connection does not validate: %v", err)
	}
	secrets := specSecrets()
	secrets["agent-kourier-system/slack-main"] = v1alpha1.CredentialKeys(conn.Spec)
	return set, secrets
}

// On the github profile, where the agent replies itself, a Binding that leaves its output out is served with none, and one that
// names none too; live and final, questions, tool approvals and feedback are refused with UnsupportedOnPlatform, each in the order
// of the fields. Its IDs are held to GitHub's shapes.
func TestResolutionOnTheGitHubProfile(t *testing.T) {
	const on = " on web (github profile), the platform of ChatConnection agent-kourier-system/slack-main: "
	page := " (see " + PlatformsPage + ")"
	noOutput := "set it to none, or leave it out: the agent replies itself, and Agent Kourier posts nothing"
	tests := []struct {
		name     string
		mutate   func(*v1alpha1.Binding)
		output   v1alpha1.OutputMode // served with, when rejected is empty
		rejected []Rejection
	}{
		{"its output left out", func(*v1alpha1.Binding) {}, v1alpha1.OutputNone, nil},
		{"output none", func(b *v1alpha1.Binding) { b.Spec.Chat.Output = v1alpha1.OutputNone }, v1alpha1.OutputNone, nil},
		{"output live", func(b *v1alpha1.Binding) { b.Spec.Chat.Output = v1alpha1.OutputLive }, "", []Rejection{
			rejected("sre-alerts", ReasonUnsupportedOnPlatform, `spec.chat.output: "live" is not supported`+on+noOutput+page)}},
		{"output final", func(b *v1alpha1.Binding) { b.Spec.Chat.Output = v1alpha1.OutputFinal }, "", []Rejection{
			rejected("sre-alerts", ReasonUnsupportedOnPlatform, `spec.chat.output: "final" is not supported`+on+noOutput+page)}},
		{"everything a post carries, in the order of the fields", func(b *v1alpha1.Binding) {
			b.Spec.Interactions.AskUser = true
			b.Spec.Interactions.ToolApprovals = true
			b.Spec.Feedback.Enabled = true
		}, "", []Rejection{
			rejected("sre-alerts", ReasonUnsupportedOnPlatform, "spec.interactions.askUser: questions to a person are not supported"+on+"set it to false"+page),
			rejected("sre-alerts", ReasonUnsupportedOnPlatform, "spec.interactions.toolApprovals: tool approvals are not supported"+on+"set it to false"+page),
			rejected("sre-alerts", ReasonUnsupportedOnPlatform, "spec.feedback.enabled: feedback buttons are not supported"+on+"set it to false"+page),
		}},
		{"a Slack channel", func(b *v1alpha1.Binding) { b.Spec.Chat.Channel = "C0123456789" }, "", []Rejection{
			rejected("sre-alerts", ReasonInvalidPlatformID, `spec.chat.channel: "C0123456789" is not a repository`+on+
				"use the repository's owner/name, such as octo-org/hello-world"+page)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set, _ := onWeb(t, v1alpha1.WebProfileGitHub)
			tc.mutate(sreAlerts(set))
			snap, got := ResolvePartial(set)
			if !slices.Equal(got, tc.rejected) {
				t.Errorf("rejections:\n%+v\nwant:\n%+v", got, tc.rejected)
			}
			out, ok := outputOf(snap, "sre-alerts")
			if len(tc.rejected) == 0 && (!ok || out != tc.output) {
				t.Errorf("sre-alerts is served with output %q (served %v); want %q", out, ok, tc.output)
			}
			if len(tc.rejected) > 0 && ok {
				t.Error("sre-alerts is served; want it rejected")
			}
			if got, _ := outputOf(snap, "chat-only"); got != v1alpha1.OutputNone {
				t.Errorf("chat-only, which names no output, is served with %q; want none", got)
			}

			set, secrets := onWeb(t, v1alpha1.WebProfileGitHub)
			tc.mutate(sreAlerts(set))
			if len(tc.rejected) == 0 {
				if _, err := Resolve(t.Context(), set, secrets); err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				return
			}
			var want []string
			for _, r := range tc.rejected {
				want = append(want, atSREAlerts+"Binding sre/sre-alerts: "+r.Message)
			}
			wantProblems(t, set, secrets, want...)
		})
	}
}

// On the kourier-v1 profile, which posts the replies, a Binding that leaves its output out is served with live; output none,
// questions, tool approvals and feedback are refused; and its IDs are held to the profile's opaque shapes.
func TestResolutionOnTheKourierV1Profile(t *testing.T) {
	const on = " on web (kourier-v1 profile), the platform of ChatConnection agent-kourier-system/slack-main: "
	page := " (see " + PlatformsPage + ")"
	set, secrets := onWeb(t, v1alpha1.WebProfileKourierV1)
	snap, err := Resolve(t.Context(), set, secrets)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, _ := outputOf(snap, "sre-alerts"); got != v1alpha1.OutputLive {
		t.Errorf("sre-alerts, which names no output, is served with %q; want live", got)
	}

	set, _ = onWeb(t, v1alpha1.WebProfileKourierV1)
	b := sreAlerts(set)
	b.Spec.Chat.Output = v1alpha1.OutputNone
	b.Spec.Chat.Channel = "octo-org/hello-world"
	b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = true, true
	_, got := ResolvePartial(set)
	want := []Rejection{
		rejected("sre-alerts", ReasonInvalidPlatformID, `spec.chat.channel: "octo-org/hello-world" is not a channel ID`+on+
			"use the channel's ID as the sender sends it: letters, digits, '.', '_' and '-', with a digit, such as ch-1"+page),
		rejected("sre-alerts", ReasonUnsupportedOnPlatform, `spec.chat.output: "none" is not supported`+on+"set it to live or final, or leave it out for live"+page),
		rejected("sre-alerts", ReasonUnsupportedOnPlatform, "spec.interactions.askUser: questions to a person are not supported"+on+"set it to false"+page),
		rejected("sre-alerts", ReasonUnsupportedOnPlatform, "spec.feedback.enabled: feedback buttons are not supported"+on+"set it to false"+page),
	}
	if !slices.Equal(got, want) {
		t.Errorf("rejections:\n%+v\nwant:\n%+v", got, want)
	}
	// RefusedOnPlatform says the first reason, and every problem.
	reason, message := RefusedOnPlatform(b, &set.ChatConnections[0])
	if reason != ReasonInvalidPlatformID || strings.Count(message, "; ") != 3 {
		t.Errorf("RefusedOnPlatform = %q, %q; want InvalidPlatformID and the four problems", reason, message)
	}
}

// A web connection's Secret must hold its profile's key, as a Slack connection's must hold its mode's: the files route checks it
// when it resolves, without reading the value.
func TestAWebConnectionsSecretNeedsItsProfilesKey(t *testing.T) {
	for _, profile := range []v1alpha1.WebProfile{v1alpha1.WebProfileKourierV1, v1alpha1.WebProfileGitHub} {
		set, secrets := onWeb(t, profile)
		key := v1alpha1.CredentialKeys(set.ChatConnections[0].Spec)[0]
		secrets["agent-kourier-system/slack-main"] = []string{"botToken", "appToken"}
		_, err := Resolve(t.Context(), set, secrets)
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: Resolve with a Secret of Slack's keys = %v; want a problem that names %s", profile, err, key)
		}
	}
}
