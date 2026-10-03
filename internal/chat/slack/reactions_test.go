package slack_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/store"
)

func TestReactAddsTheReactionOnce(t *testing.T) {
	fake := newFake(t)
	c := slack.New(clientConfig(fake))
	_, ts, err := slackgo.New("xoxb-fake", slackgo.OptionAPIURL(fake.URL()+"/")).PostMessage("C1", slackgo.MsgOptionText("hi", false))
	if err != nil {
		t.Fatal(err)
	}
	// The second call is a retry after a lost answer: the reaction is there, which is what was asked.
	for i := range 2 {
		if err := c.React(t.Context(), "C1", ts, "hourglass"); err != nil {
			t.Fatalf("React #%d = %v; want success, also when the reaction is there already", i+1, err)
		}
	}
	if got := fake.Reactions("C1", ts); len(got) != 1 || got[0].Name != "hourglass" || len(got[0].Users) != 1 {
		t.Fatalf("Reactions = %+v; want one hourglass by one user", got)
	}
}

func TestReactFailures(t *testing.T) {
	fake := newFake(t)
	c := slack.New(clientConfig(fake))
	_, ts, err := slackgo.New("xoxb-fake", slackgo.OptionAPIURL(fake.URL()+"/")).PostMessage("C1", slackgo.MsgOptionText("hi", false))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name               string
		ctx                context.Context
		channel, ts, emoji string
		wantInvalid        bool   // refused before any call
		wantCode           string // the Slack code, found with errors.As
		wantCancelled      bool
	}{
		{"no message", t.Context(), "C1", "1.1", "hourglass", false, "message_not_found", false},
		{"no channel", t.Context(), "", ts, "hourglass", true, "", false},
		{"no ts", t.Context(), "C1", "", "hourglass", true, "", false},
		{"no name", t.Context(), "C1", ts, "", true, "", false},
		{"a name Slack would refuse", t.Context(), "C1", ts, ":eyes:", false, "invalid_name", false},
		{"a context that has ended", cancelled, "C1", ts, "hourglass", false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(fake.Reactions("C1", ts))
			err := c.React(tc.ctx, tc.channel, tc.ts, tc.emoji)
			var resp slackgo.SlackErrorResponse
			switch {
			case err == nil:
				t.Fatal("React succeeded")
			case errors.Is(err, store.ErrInvalid) != tc.wantInvalid:
				t.Fatalf("React = %v; ErrInvalid is %v, want %v", err, !tc.wantInvalid, tc.wantInvalid)
			case tc.wantCode != "" && (!errors.As(err, &resp) || resp.Err != tc.wantCode):
				t.Fatalf("React = %v; want the code %s kept for errors.As", err, tc.wantCode)
			case errors.Is(err, context.Canceled) != tc.wantCancelled:
				t.Fatalf("React = %v; context.Canceled is %v, want %v", err, !tc.wantCancelled, tc.wantCancelled)
			case len(fake.Reactions("C1", ts)) != before:
				t.Fatalf("React = %v; want nothing added", err)
			}
		})
	}
}

// TestReactErrorNamesNoToken fails the request with an error that quotes both tokens.
func TestReactErrorNamesNoToken(t *testing.T) {
	const bot, app = "xoxb-canary-bot-token", "xapp-canary-app-token"
	calls := &counter{n: map[string]int{}, err: fmt.Errorf("proxy refused %s and %s", bot, app)}
	cfg := clientConfig(newFake(t))
	cfg.BotToken, cfg.AppToken, cfg.HTTPClient = secret.New(bot), secret.New(app), &http.Client{Transport: calls}
	err := slack.New(cfg).React(t.Context(), "C1", "1.1", "hourglass")
	if err == nil || strings.Contains(err.Error(), bot) || strings.Contains(err.Error(), app) {
		t.Fatalf("React = %v; want an error that holds no token", err)
	}
}

func TestUnreactTakesTheReactionOffAndIsSuccessWhenItIsNotThere(t *testing.T) {
	fake := newFake(t)
	c := slack.New(clientConfig(fake))
	_, ts, err := slackgo.New("xoxb-fake", slackgo.OptionAPIURL(fake.URL()+"/")).PostMessage("C1", slackgo.MsgOptionText("hi", false))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.React(t.Context(), "C1", ts, "hourglass"); err != nil {
		t.Fatal(err)
	}
	// The second call is a retry after a lost answer: the reaction is gone, which is what was asked.
	for i := range 2 {
		if err := c.Unreact(t.Context(), "C1", ts, "hourglass"); err != nil {
			t.Fatalf("Unreact #%d = %v; want success, also when the reaction is not there", i+1, err)
		}
	}
	if got := fake.Reactions("C1", ts); len(got) != 0 {
		t.Fatalf("Reactions = %+v; want none", got)
	}
}

func TestUnreactFailures(t *testing.T) {
	fake := newFake(t)
	c := slack.New(clientConfig(fake))
	_, ts, err := slackgo.New("xoxb-fake", slackgo.OptionAPIURL(fake.URL()+"/")).PostMessage("C1", slackgo.MsgOptionText("hi", false))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name               string
		ctx                context.Context
		channel, ts, emoji string
		wantInvalid        bool   // refused before any call
		wantCode           string // the Slack code, found with errors.As
		wantCancelled      bool
	}{
		{"no message", t.Context(), "C1", "1.1", "hourglass", false, "message_not_found", false},
		{"no channel", t.Context(), "", ts, "hourglass", true, "", false},
		{"no ts", t.Context(), "C1", "", "hourglass", true, "", false},
		{"no name", t.Context(), "C1", ts, "", true, "", false},
		{"a name Slack would refuse", t.Context(), "C1", ts, ":eyes:", false, "invalid_name", false},
		{"a context that has ended", cancelled, "C1", ts, "hourglass", false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Unreact(tc.ctx, tc.channel, tc.ts, tc.emoji)
			var resp slackgo.SlackErrorResponse
			switch {
			case err == nil:
				t.Fatal("Unreact succeeded")
			case errors.Is(err, store.ErrInvalid) != tc.wantInvalid:
				t.Fatalf("Unreact = %v; ErrInvalid is %v, want %v", err, !tc.wantInvalid, tc.wantInvalid)
			case tc.wantCode != "" && (!errors.As(err, &resp) || resp.Err != tc.wantCode):
				t.Fatalf("Unreact = %v; want the code %s kept for errors.As", err, tc.wantCode)
			case errors.Is(err, context.Canceled) != tc.wantCancelled:
				t.Fatalf("Unreact = %v; context.Canceled is %v, want %v", err, !tc.wantCancelled, tc.wantCancelled)
			}
		})
	}
}

// TestUnreactErrorNamesNoToken fails the request with an error that quotes both tokens.
func TestUnreactErrorNamesNoToken(t *testing.T) {
	const bot, app = "xoxb-canary-bot-token", "xapp-canary-app-token"
	calls := &counter{n: map[string]int{}, err: fmt.Errorf("proxy refused %s and %s", bot, app)}
	cfg := clientConfig(newFake(t))
	cfg.BotToken, cfg.AppToken, cfg.HTTPClient = secret.New(bot), secret.New(app), &http.Client{Transport: calls}
	err := slack.New(cfg).Unreact(t.Context(), "C1", "1.1", "hourglass")
	if err == nil || strings.Contains(err.Error(), bot) || strings.Contains(err.Error(), app) {
		t.Fatalf("Unreact = %v; want an error that holds no token", err)
	}
}
