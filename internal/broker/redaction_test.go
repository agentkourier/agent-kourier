package broker

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// wantUnredacted fails unless text comes out of both redaction paths as it went in: the one a Binding's status message takes
// (statusMessage) and the one a log line's error attribute takes (telemetry.RedactAttr). The redaction rules read a secret word followed
// by ":" as a name and its value, so a broker message such as "the bot's token: replace it" loses the word after the colon. The messages
// here are the broker's own words, with no secret in them, and they say it in a form the rules leave alone.
func wantUnredacted(t *testing.T, what, text string) {
	t.Helper()
	if text == "" {
		t.Errorf("%s: the message is empty", what)
	}
	if got := statusMessage(text); got != text {
		t.Errorf("%s: the status shows %q; want the message as it is, %q", what, got, text)
	}
	a := telemetry.RedactAttr(nil, slog.Any("error", errors.New(text)))
	if got := a.Value.String(); got != text {
		t.Errorf("%s: the log shows %q; want the message as it is, %q", what, got, text)
	}
}

// The words the broker itself writes in a Binding's status and in the log survive redaction. Each case is made by the code that makes it
// in production, so that a reworded message, or a new one in the same shape, is held to the same rule.
func TestTheBrokersOwnMessagesSurviveRedaction(t *testing.T) {
	t.Run("the platform rejected the bot's token", func(t *testing.T) {
		for _, selfRestarting := range []bool{false, true} {
			out := credentialsWatch{selfRestarting: selfRestarting}.rejected()
			wantUnredacted(t, "ChatCredentialsRejected, self-restarting "+map[bool]string{false: "no", true: "yes"}[selfRestarting], out.Message)
		}
	})
	t.Run("the credentials changed or are unusable", func(t *testing.T) {
		for _, selfRestarting := range []bool{false, true} {
			for name, now := range map[string]failingReader{
				"changed": {value: "xoxb-fake-token-new"},
				"gone":    {err: secret.ErrKeyNotFound},
			} {
				w := credentialsWatch{
					baseline:       map[string]secret.Value{"botToken": secret.New("xoxb-fake-token-old")},
					secrets:        now,
					where:          func() (string, string, bool) { return "team-a", "slack-credentials", true },
					selfRestarting: selfRestarting,
				}
				out, held := w.hold(t.Context())
				if !held {
					t.Fatalf("hold of a Secret that is %s did not hold", name)
				}
				wantUnredacted(t, "a probe held back for credentials that are "+name, out.Message)
			}
		}
	})
	t.Run("the Binding requires a token", func(t *testing.T) {
		wantUnredacted(t, "ErrTokenRequired", credentialsOutcome(agent.ErrTokenRequired).Message)
	})
	t.Run("a connection that is down", func(t *testing.T) {
		wantUnredacted(t, "a rejected connection", (&connectionStopped{conn: "team-a/slack", rejected: true, code: "invalid_auth"}).Error())
		wantUnredacted(t, "a connection with unusable credentials", (&connectionStopped{conn: "team-a/slack", cause: errors.New("the key is empty")}).Error())
	})
	t.Run("an identity call that failed", func(t *testing.T) {
		for _, method := range []string{"auth.test", "bots.info"} {
			for _, cause := range []error{
				slackgo.SlackErrorResponse{Err: "invalid_auth"},
				slackgo.SlackErrorResponse{Err: "token_revoked"},
				slackgo.SlackErrorResponse{Err: "account_inactive"},
				slackgo.SlackErrorResponse{Err: "missing_scope"},
				slackgo.SlackErrorResponse{Err: "ratelimited"},
				slackgo.StatusCodeError{Code: 503, Status: "503 Service Unavailable"},
				&slackgo.RateLimitedError{},
				context.DeadlineExceeded,
				context.Canceled,
				errors.New("dial tcp: connection refused"),
			} {
				wantUnredacted(t, method+" failing with "+cause.Error(), identifyError(method, cause).Error())
			}
		}
	})
	t.Run("an identity call that answered with no bot or no app", func(t *testing.T) {
		f := newFakeSlackWeb(t)
		f.bots["xoxb-fake-token-nobot"] = struct{ team, user, bot, app string }{"TW1", "UW1", "", ""}
		_, err := slackIdentity(t.Context(), connWithBotToken("xoxb-fake-token-nobot"), f.url())
		if err == nil {
			t.Fatal("slackIdentity with no bot in the answer succeeded")
		}
		wantUnredacted(t, "no bot", err.Error())

		f.bots["xoxb-fake-token-noapp"] = struct{ team, user, bot, app string }{"TW1", "UW1", "BW1", ""}
		_, err = slackIdentity(t.Context(), connWithBotToken("xoxb-fake-token-noapp"), f.url())
		if err == nil {
			t.Fatal("slackIdentity with no app in the answer succeeded")
		}
		wantUnredacted(t, "no app", err.Error())
	})
	t.Run("a Secret of the broker that cannot be used", func(t *testing.T) {
		// The Postgres DSN: a Secret whose value is only white space is refused by openStore itself, and one that cannot be read is wrapped.
		_, err := openStore(t.Context(), StoreConfig{Kind: StorePostgres, PostgresDSN: dsnRef}, "", &values{dsn: " \n"}, discard())
		if !errors.Is(err, secret.ErrEmptyValue) {
			t.Fatalf("openStore with a blank DSN = %v; want secret.ErrEmptyValue", err)
		}
		wantUnredacted(t, "a blank DSN", err.Error())

		_, err = openStore(t.Context(), StoreConfig{Kind: StorePostgres, PostgresDSN: dsnRef}, "", &values{err: errors.New("the Secret is unreadable")}, discard())
		if !errors.Is(err, ErrWiring) {
			t.Fatalf("openStore with an unreadable Secret = %v; want ErrWiring", err)
		}
		wantUnredacted(t, "an unreadable DSN", err.Error())
	})
	t.Run("the credentials Secret of a ChatConnection that cannot be read", func(t *testing.T) {
		c := &v1alpha1.ChatConnection{Spec: v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformSlack}}
		c.Namespace, c.Name = "team-a", "slack"
		c.Spec.CredentialsSecretRef.Name = "slack-credentials"
		_, err := connectionOf(t.Context(), c, failingReader{err: secret.ErrKeyNotFound})
		if !errors.Is(err, ErrWiring) {
			t.Fatalf("connectionOf with a missing key = %v; want ErrWiring", err)
		}
		wantUnredacted(t, "a missing key of a Secret that is named for credentials", err.Error())
	})
	t.Run("the connection is down for a chat call", func(t *testing.T) {
		wantUnredacted(t, "chat.ErrConnectionDown", downError("team-a/slack").Error())
		wantUnredacted(t, "chat.ErrConnectionDown, bare", chat.ErrConnectionDown.Error())
	})
}

// failingReader is a SecretReader that answers value, or fails with err.
type failingReader struct {
	value string
	err   error
}

func (r failingReader) Value(context.Context, string, string, string) (secret.Value, error) {
	if r.err != nil {
		return secret.Value{}, r.err
	}
	return secret.New(r.value), nil
}
