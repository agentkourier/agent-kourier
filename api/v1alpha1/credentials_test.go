package v1alpha1

import (
	"slices"
	"testing"
)

// The keys of a Slack connection's credentials Secret are those of spec section 7 for its mode, the bot token first, as the broker reads
// and reports them: Socket Mode, which is also the mode of a connection that names none, needs the app-level token, and HTTP mode the
// signing secret. A platform that is not supported, or a mode that is not, has none.
func TestCredentialKeysPerPlatformAndMode(t *testing.T) {
	socket, http := []string{"botToken", "appToken"}, []string{"botToken", "signingSecret"}
	tests := []struct {
		name string
		spec ChatConnectionSpec
		want []string
	}{
		{"slack with no slack block", ChatConnectionSpec{Platform: PlatformSlack}, socket},
		{"slack with an empty slack block", ChatConnectionSpec{Platform: PlatformSlack, Slack: &SlackConnectionSpec{}}, socket},
		{"slack in Socket Mode", ChatConnectionSpec{Platform: PlatformSlack, Slack: &SlackConnectionSpec{Mode: SlackModeSocket}}, socket},
		{"slack in HTTP mode", ChatConnectionSpec{Platform: PlatformSlack, Slack: &SlackConnectionSpec{Mode: SlackModeHTTP}}, http},
		{"slack in a mode that is not supported", ChatConnectionSpec{Platform: PlatformSlack, Slack: &SlackConnectionSpec{Mode: "websocket"}}, nil},
		{"no platform", ChatConnectionSpec{}, nil},
		{"teams", ChatConnectionSpec{Platform: "teams"}, nil},
		{"Slack, capitalised", ChatConnectionSpec{Platform: "Slack"}, nil},
	}
	for _, tc := range tests {
		if got := CredentialKeys(tc.spec); !slices.Equal(got, tc.want) {
			t.Errorf("%s: CredentialKeys = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A caller may sort or change the slice it is given (the broker sorts it) without changing what the next caller gets.
func TestCredentialKeysReturnsACopy(t *testing.T) {
	for _, mode := range []SlackMode{SlackModeSocket, SlackModeHTTP} {
		spec := ChatConnectionSpec{Platform: PlatformSlack, Slack: &SlackConnectionSpec{Mode: mode}}
		keys := CredentialKeys(spec)
		keys[0] = "changed"
		if got := CredentialKeys(spec); got[0] != "botToken" {
			t.Errorf("CredentialKeys in mode %q = %q after a caller changed its slice, want the table unchanged", mode, got)
		}
	}
}

// Every platform and mode that a ChatConnection may name has keys, and one it may not name has none: a platform or mode added to
// Validate without keys would start with no credentials read.
func TestEveryPlatformAndModeValidateAcceptsHasCredentialKeys(t *testing.T) {
	for _, scheme := range []WebhookScheme{WebhookHMACSHA256, WebhookToken, "", "hmac-sha1", "Token"} {
		c := load[ChatConnection](t, "webhook/connection.yaml")
		c.Spec.Webhook.Verify.Scheme = scheme
		if valid, keyed := c.Validate() == nil, CredentialKeys(c.Spec) != nil; valid != keyed {
			t.Errorf("webhook scheme %q: Validate accepts it %v, CredentialKeys has keys %v: the two must agree", scheme, valid, keyed)
		}
	}
	for _, p := range []string{PlatformSlack, PlatformWebhook, "", "teams", "github", "Slack"} {
		for _, slack := range []*SlackConnectionSpec{nil, {}, {Mode: SlackModeSocket}, {Mode: SlackModeHTTP}, {Mode: "websocket"}, {Mode: "HTTP"}} {
			c := load[ChatConnection](t, "chatconnection.yaml")
			c.Spec.Platform, c.Spec.Slack = p, slack
			if valid, keyed := c.Validate() == nil, CredentialKeys(c.Spec) != nil; valid != keyed {
				t.Errorf("platform %q, slack %+v: Validate accepts it %v, CredentialKeys has keys %v: the two must agree", p, slack, valid, keyed)
			}
		}
	}
}

// A connection that names no mode is in Socket Mode: the API sets no default (spec D42), so the mode is read through SlackMode.
func TestSlackModeOfASpec(t *testing.T) {
	tests := []struct {
		slack *SlackConnectionSpec
		want  SlackMode
	}{
		{nil, SlackModeSocket},
		{&SlackConnectionSpec{}, SlackModeSocket},
		{&SlackConnectionSpec{Mode: SlackModeSocket}, SlackModeSocket},
		{&SlackConnectionSpec{Mode: SlackModeHTTP}, SlackModeHTTP},
	}
	for _, tc := range tests {
		if got := (ChatConnectionSpec{Platform: PlatformSlack, Slack: tc.slack}).SlackMode(); got != tc.want {
			t.Errorf("SlackMode() of slack %+v = %q, want %q", tc.slack, got, tc.want)
		}
	}
}

// Default leaves the mode unset: an apiserver default would be written into every stored ChatConnection (spec D42, as for the
// acknowledgment's emoji).
func TestDefaultLeavesTheSlackModeUnset(t *testing.T) {
	for _, slack := range []*SlackConnectionSpec{nil, {}} {
		c := load[ChatConnection](t, "chatconnection.yaml")
		c.Spec.Slack = slack
		c.Default()
		if c.Spec.Slack != slack || (slack != nil && slack.Mode != "") {
			t.Errorf("Default() changed slack %+v to %+v, want it left as it was", slack, c.Spec.Slack)
		}
	}
}
