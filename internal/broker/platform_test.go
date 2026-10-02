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
	built, err := cfg.newPlatform(slackConn, nil)
	if err != nil || built.Platform == nil || built.closeIdle == nil {
		t.Fatalf("newPlatform(slack) = %+v, %v; want the Slack adapter, and a way to close its connections", built, err)
	}
	if got := built.Capabilities(); !got.Streams || !got.Edits {
		t.Errorf("the adapter's capabilities are %+v; want Slack's", got)
	}
	for _, name := range []string{"teams", "", "Slack"} {
		bad := slackConn
		bad.Platform = name
		built, err := cfg.newPlatform(bad, nil)
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
	built, err := cfg.newPlatform(conn, nil)
	if err != nil || saw.BotToken.Reveal() != "xoxb-fake" || saw.AppToken.Reveal() != "xapp-fake" || saw.HTTPClient == nil {
		t.Fatalf("newPlatform = %+v, %v; client config %+v; want the client built from the credentials, on a transport of its own", built, err, saw)
	}
	if err := built.Run(canceled(t), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v; want the client's, which idleSlack ends with its context", err)
	}
}

func canceled(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}
