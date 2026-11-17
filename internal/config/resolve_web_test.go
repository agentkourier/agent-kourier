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

// onWeb makes the spec set's connection a web connection of the kourier-v1 profile, and sre-alerts a Binding that the profile takes:
// a channel and a sender of its shapes, and none of what it does not serve. The secret is the profile's.
func onWeb(t *testing.T) (*Set, fakeSecrets) {
	t.Helper()
	set := specSet(t)
	conn := &set.ChatConnections[0]
	conn.Spec.Platform = v1alpha1.PlatformWeb
	conn.Spec.Web = &v1alpha1.WebConnectionSpec{Profile: v1alpha1.WebProfileKourierV1, URL: "https://receiver.example.com/replies"}
	b := sreAlerts(set)
	b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = false, false
	chatOnly := &set.Bindings[slices.IndexFunc(set.Bindings, func(b v1alpha1.Binding) bool { return b.Name == "chat-only" })]
	b.Spec.Chat.Channel, b.Spec.Chat.Triggers[1].From.ID = "ch-1", "billing-bot"
	chatOnly.Spec.Chat.Channel = "ch-2"
	if err := conn.Validate(); err != nil {
		t.Fatalf("the web connection does not validate: %v", err)
	}
	secrets := specSecrets()
	secrets["agent-kourier-system/slack-main"] = v1alpha1.CredentialKeys(conn.Spec)
	return set, secrets
}

// On the kourier-v1 profile, which posts the replies, a Binding is served with its output; questions, tool approvals and feedback
// are refused; and its IDs are held to the profile's opaque shapes.
func TestResolutionOnTheKourierV1Profile(t *testing.T) {
	const on = " on web (kourier-v1 profile), the platform of ChatConnection agent-kourier-system/slack-main: "
	page := " (see " + PlatformsPage + ")"
	set, secrets := onWeb(t)
	sreAlerts(set).Spec.Chat.Output = v1alpha1.OutputFinal
	snap, err := Resolve(t.Context(), set, secrets)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, _ := outputOf(snap, "sre-alerts"); got != v1alpha1.OutputFinal {
		t.Errorf("sre-alerts is served with output %q; want final", got)
	}

	set, _ = onWeb(t)
	b := sreAlerts(set)
	b.Spec.Chat.Channel = "octo-org/hello-world"
	b.Spec.Interactions.AskUser, b.Spec.Feedback.Enabled = true, true
	_, got := ResolvePartial(set)
	want := []Rejection{
		rejected("sre-alerts", ReasonInvalidPlatformID, `spec.chat.channel: "octo-org/hello-world" is not a channel ID`+on+
			"use the channel's ID as the sender sends it: letters, digits, '.', '_' and '-', starting with a letter or a digit, such as ch-1"+page),
		rejected("sre-alerts", ReasonUnsupportedOnPlatform, "spec.interactions.askUser: questions to a person are not supported"+on+"set it to false"+page),
		rejected("sre-alerts", ReasonUnsupportedOnPlatform, "spec.feedback.enabled: feedback buttons are not supported"+on+"set it to false"+page),
	}
	if !slices.Equal(got, want) {
		t.Errorf("rejections:\n%+v\nwant:\n%+v", got, want)
	}
	// RefusedOnPlatform says the first reason, and every problem.
	reason, message := RefusedOnPlatform(b, &set.ChatConnections[0])
	if reason != ReasonInvalidPlatformID || strings.Count(message, "; ") != 2 {
		t.Errorf("RefusedOnPlatform = %q, %q; want InvalidPlatformID and the three problems", reason, message)
	}
}

// A web connection's Secret must hold its profile's key, as a Slack connection's must hold its mode's: the files route checks it
// when it resolves, without reading the value.
func TestAWebConnectionsSecretNeedsItsProfilesKey(t *testing.T) {
	set, secrets := onWeb(t)
	key := v1alpha1.CredentialKeys(set.ChatConnections[0].Spec)[0]
	secrets["agent-kourier-system/slack-main"] = []string{"botToken", "appToken"}
	_, err := Resolve(t.Context(), set, secrets)
	if err == nil || !strings.Contains(err.Error(), key) {
		t.Errorf("Resolve with a Secret of Slack's keys = %v; want a problem that names %s", err, key)
	}
}
