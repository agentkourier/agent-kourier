package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// The adapter is chosen by ChatConnection.platform: slack builds the Slack adapter over a client of its own, and any
// other value is a wiring error, which no retry mends.
func TestNewPlatformIsChosenByThePlatformOfTheConnection(t *testing.T) {
	slackConn := Connection{
		Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}, Platform: v1alpha1.PlatformSlack,
		Credentials: map[string]secret.Value{"botToken": secret.New("xoxb-fake"), "appToken": secret.New("xapp-fake")},
	}
	var cfg RunConfig
	built, err := cfg.newPlatform(slackConn, nil, nil)
	if err != nil || built.Platform == nil || built.closeIdle == nil {
		t.Fatalf("newPlatform(slack) = %+v, %v; want the Slack adapter, and a way to close its connections", built, err)
	}
	if got := built.Capabilities(); !got.Streams || !got.Edits {
		t.Errorf("the adapter's capabilities are %+v; want Slack's", got)
	}
	for _, name := range []string{"teams", "", "Slack"} {
		bad := slackConn
		bad.Platform = name
		built, err := cfg.newPlatform(bad, nil, nil)
		if !errors.Is(err, ErrWiring) || built.Platform != nil {
			t.Errorf("newPlatform(%q) = %+v, %v; want a wiring error and no platform", name, built, err)
		}
	}
}

// The seam of the tests builds the Slack client, and nothing else: the adapter is still the platform's.
func TestTheSlackSeamBuildsTheClientTheAdapterRunsOn(t *testing.T) {
	var saw slack.Config
	cfg := RunConfig{newSlack: func(sc slack.Config) slackAPI { saw = sc; return idleSlack{} }}
	conn := Connection{
		Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}, Platform: v1alpha1.PlatformSlack,
		Credentials: map[string]secret.Value{"botToken": secret.New("xoxb-fake"), "appToken": secret.New("xapp-fake")},
	}
	built, err := cfg.newPlatform(conn, nil, nil)
	if err != nil || saw.BotToken.Reveal() != "xoxb-fake" || saw.AppToken.Reveal() != "xapp-fake" || saw.HTTPClient == nil {
		t.Fatalf("newPlatform = %+v, %v; client config %+v; want the client built from the credentials, on a transport of its own", built, err, saw)
	}
	if err := built.Run(canceled(t), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v; want the client's, which idleSlack ends with its context", err)
	}
}

// The func that newPlatform is given reaches the Slack client's config as its OnConnection, so the adapter can report
// the connection to the metric; a nil one is no hook.
func TestNewPlatformPassesTheConnectionHookToTheClient(t *testing.T) {
	var saw slack.Config
	cfg := RunConfig{newSlack: func(sc slack.Config) slackAPI { saw = sc; return idleSlack{} }}
	conn := Connection{
		Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}, Platform: v1alpha1.PlatformSlack,
		Credentials: map[string]secret.Value{"botToken": secret.New("xoxb-fake"), "appToken": secret.New("xapp-fake")},
	}
	var told []bool
	if _, err := cfg.newPlatform(conn, nil, func(up bool) { told = append(told, up) }); err != nil || saw.OnConnection == nil {
		t.Fatalf("newPlatform = %v; client config %+v; want OnConnection set", err, saw)
	}
	saw.OnConnection(true)
	if len(told) != 1 || !told[0] {
		t.Errorf("the hook was told %v; want the call of OnConnection", told)
	}
	if _, err := cfg.newPlatform(conn, nil, nil); err != nil || saw.OnConnection != nil {
		t.Errorf("newPlatform with no hook = %v; OnConnection set = %v; want none", err, saw.OnConnection != nil)
	}
}

func canceled(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}
