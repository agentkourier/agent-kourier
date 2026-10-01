package slack_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// TestFatalAuthCode makes Run's own auth.test call fail with each code, so the classifier reads the
// error as Run returns it, scrubbed and wrapped.
func TestFatalAuthCode(t *testing.T) {
	for code, want := range map[string]string{
		"invalid_auth": "invalid_auth", "token_revoked": "token_revoked", "account_inactive": "account_inactive", "not_authed": "not_authed",
		"ratelimited": "", "internal_error": "", "service_unavailable": "", // a try later may work
		"channel_not_found": "", "msg_too_long": "", // permanent for a post, not for the connection
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code})
		}))
		cfg := clientConfig(newFake(t))
		cfg.APIURL = srv.URL + "/"
		err := slack.New(cfg).Run(t.Context(), nil)
		srv.Close()
		if err == nil {
			t.Fatalf("%s: Run succeeded", code)
		}
		if got := slack.FatalAuthCode(err); got != want {
			t.Errorf("FatalAuthCode(%s) = %q, want %q", code, got, want)
		}
	}
}

func TestFatalAuthCodeIgnoresWhatIsNotSlacks(t *testing.T) {
	for _, err := range []error{nil, errors.New("invalid_auth"), errors.New("dial tcp: connection refused"), t.Context().Err()} {
		if got := slack.FatalAuthCode(err); got != "" {
			t.Errorf("FatalAuthCode(%v) = %q, want none: only Slack's own answer counts", err, got)
		}
	}
}

// A bad app token fails at apps.connections.open, a different call from auth.test, and the fake
// answers invalid_auth there too: the connection's own failure is as fatal as a bad bot token.
func TestFatalAuthCodeOfARejectedAppToken(t *testing.T) {
	for name, tok := range map[string]func(*slack.Config){
		"bad app token": func(c *slack.Config) { c.AppToken = secret.New("wrong-app-token") },
		"bad bot token": func(c *slack.Config) { c.BotToken = secret.New("wrong-bot-token") },
	} {
		cfg := clientConfig(newFake(t))
		tok(&cfg)
		_, wait := run(t, slack.New(cfg), &sink{})
		if got := slack.FatalAuthCode(wait()); got != "invalid_auth" {
			t.Errorf("%s: FatalAuthCode(Run's error) = %q, want invalid_auth", name, got)
		}
	}
}
