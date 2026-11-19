package broker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// The adapter is chosen by ChatConnection.platform: slack builds the Slack adapter over a client of its own, teams the Microsoft
// Teams adapter (the Teams tests), and any other value, webhook among them until its adapter lands, is a wiring error, which no
// retry mends.
func TestNewPlatformIsChosenByThePlatformOfTheConnection(t *testing.T) {
	slackConn := Connection{
		Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}, Platform: v1alpha1.PlatformSlack,
		Credentials: map[string]secret.Value{"botToken": secret.New("xoxb-fake"), "appToken": secret.New("xapp-fake")},
	}
	var cfg RunConfig
	built, err := cfg.newPlatform(slackConn, nil, nil, nil)
	if err != nil || built.Platform == nil || built.closeIdle == nil {
		t.Fatalf("newPlatform(slack) = %+v, %v; want the Slack adapter, and a way to close its connections", built, err)
	}
	if got := built.Capabilities(); !got.Streams || got.AckReaction == "" {
		t.Errorf("the adapter's capabilities are %+v; want Slack's", got)
	}
	for _, name := range []string{v1alpha1.PlatformWebhook, "discord", "", "Slack", "Teams"} {
		bad := slackConn
		bad.Platform = name
		built, err := cfg.newPlatform(bad, nil, nil, nil)
		if !errors.Is(err, ErrWiring) || built.Platform != nil {
			t.Errorf("newPlatform(%q) = %+v, %v; want a wiring error and no platform", name, built, err)
		}
	}
}

// The API takes a webhook ChatConnection before this broker has its adapter: one that Validate accepts is a wiring error that names
// the platform, and no Secret is read for it, so the supervisor reports it as having no adapter and does not wait on a Secret to
// start it. The reader has no values: a read would fail with an empty value, which is not errNoAdapter.
func TestAWebhookConnectionIsAWiringErrorUntilItsAdapterLands(t *testing.T) {
	c := &v1alpha1.ChatConnection{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "github"},
		Spec: v1alpha1.ChatConnectionSpec{
			Platform:             v1alpha1.PlatformWebhook,
			CredentialsSecretRef: v1alpha1.SecretRef{Name: "github-webhook"},
			Webhook: &v1alpha1.WebhookSpec{
				Identity: "pr-reviewer[bot]",
				Verify:   v1alpha1.WebhookVerify{Scheme: v1alpha1.WebhookHMACSHA256, Header: "X-Hub-Signature-256", Prefix: "sha256="},
				Channel:  v1alpha1.WebhookChannel{From: []string{"/repository/full_name"}},
				Actor:    v1alpha1.WebhookActor{From: []string{"/sender/login"}},
			},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the webhook connection is refused: %v", err)
	}
	_, err := connectionOf(t.Context(), c, &reader{})
	if !errors.Is(err, ErrWiring) || !errors.Is(err, errNoAdapter) {
		t.Fatalf("connectionOf(webhook) = %v; want a wiring error for a platform with no adapter", err)
	}
	if !strings.Contains(err.Error(), `"webhook"`) {
		t.Errorf("the error %q does not name the platform", err)
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
	built, err := cfg.newPlatform(conn, nil, nil, nil)
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
	if _, err := cfg.newPlatform(conn, nil, func(up bool) { told = append(told, up) }, nil); err != nil || saw.OnConnection == nil {
		t.Fatalf("newPlatform = %v; client config %+v; want OnConnection set", err, saw)
	}
	saw.OnConnection(true)
	if len(told) != 1 || !told[0] {
		t.Errorf("the hook was told %v; want the call of OnConnection", told)
	}
	if _, err := cfg.newPlatform(conn, nil, nil, nil); err != nil || saw.OnConnection != nil {
		t.Errorf("newPlatform with no hook = %v; OnConnection set = %v; want none", err, saw.OnConnection != nil)
	}
}

// The Slack client is told which ChatConnection it belongs to, as "namespace/name", so that the series it records carry the connection
// of its own and not another's; two connections' clients are told apart.
func TestNewPlatformTellsTheClientItsConnection(t *testing.T) {
	var saw []slack.Config
	cfg := RunConfig{newSlack: func(sc slack.Config) slackAPI { saw = append(saw, sc); return idleSlack{} }}
	for _, ref := range []v1alpha1.ObjectRef{{Namespace: "platform", Name: "slack"}, {Namespace: "team-a", Name: "slack"}} {
		conn := Connection{Ref: ref, Platform: v1alpha1.PlatformSlack,
			Credentials: map[string]secret.Value{"botToken": secret.New("xoxb-fake"), "appToken": secret.New("xapp-fake")}}
		if _, err := cfg.newPlatform(conn, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, sc := range saw {
		got = append(got, sc.Connection)
	}
	if want := []string{"platform/slack", "team-a/slack"}; !slices.Equal(got, want) {
		t.Errorf("the clients were configured for the connections %q; want %q", got, want)
	}
}

func canceled(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

// A turn is attributed to a person on a platform: the ID is the ChatConnection's spec.platform and the name is the one the
// API holds for it (ActorName). A platform the API does not know keeps its ID, with no name, so that a prefix still reads true.
func TestActorPlatformNamesThePlatformOfTheConnection(t *testing.T) {
	for _, tc := range []struct {
		spec v1alpha1.ChatConnectionSpec
		want dialect.Platform
	}{
		{v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformSlack}, dialect.Platform{ID: "slack", Name: "Slack"}},
		{v1alpha1.ChatConnectionSpec{Platform: "teams"}, dialect.Platform{ID: "teams"}},
		{v1alpha1.ChatConnectionSpec{}, dialect.Platform{}},
	} {
		if got := actorPlatform(tc.spec); got != tc.want {
			t.Errorf("actorPlatform(%+v) = %+v, want %+v", tc.spec, got, tc.want)
		}
	}
}
