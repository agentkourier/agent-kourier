package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// poster is an outbox.Poster whose Post and Find answer with the errors the test sets.
type poster struct{ postErr, findErr error }

func (p poster) Post(context.Context, string, string, slack.Content, string) (string, error) {
	return "1.0", p.postErr
}

func (p poster) Find(context.Context, string, string, string, time.Time) (string, bool, error) {
	return "2.0", true, p.findErr
}

func slackErr(code string) error {
	return fmt.Errorf("slack chat.postMessage: %w", slackgo.SlackErrorResponse{Err: code})
}

func TestWatchPostsReportsOnlyFatalAuthAndPassesEverythingThrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string // the code reported, "" for none
	}{
		{"revoked token", slackErr("token_revoked"), "token_revoked"},
		{"invalid auth", slackErr("invalid_auth"), "invalid_auth"},
		{"inactive account", slackErr("account_inactive"), "account_inactive"},
		{"no auth", slackErr("not_authed"), "not_authed"},
		{"a channel that is gone", slackErr("channel_not_found"), ""}, // permanent for a post, not for the tokens
		{"a rate limit", slackErr("ratelimited"), ""},
		{"a network error", errors.New("dial tcp: connection refused"), ""},
		{"success", nil, ""},
	} {
		for _, call := range []string{"Post", "Find"} {
			var reported []string
			w := WatchPosts(poster{postErr: tc.err, findErr: tc.err}, func(code string) { reported = append(reported, code) })

			var ts string
			var err error
			if call == "Post" {
				ts, err = w.Post(t.Context(), "C1", "", slack.Content{}, "id")
			} else {
				ts, _, err = w.Find(t.Context(), "C1", "", "id", time.Time{})
			}

			if wantTS := map[string]string{"Post": "1.0", "Find": "2.0"}[call]; ts != wantTS || !errors.Is(err, tc.err) && tc.err != nil {
				t.Errorf("%s/%s: got %q, %v; want the poster's own answer untouched", tc.name, call, ts, err)
			}
			if tc.want == "" && len(reported) != 0 || tc.want != "" && (len(reported) != 1 || reported[0] != tc.want) {
				t.Errorf("%s/%s: reported %v, want %q", tc.name, call, reported, tc.want)
			}
		}
	}
}

// blocking is a SlackRunner that holds the connection until its context ends, as a healthy one does.
type blocking struct{}

func (blocking) Run(ctx context.Context, _ slack.Sink) error { <-ctx.Done(); return ctx.Err() }

// A bot token revoked while the connection is up fails posts with invalid_auth, which the outbox
// treats as transient. The report ends the run with ErrSlackAuth, so the process exits and says so.
func TestAPostThatFindsTheTokenRevokedEndsTheSlackRun(t *testing.T) {
	ctx, report, stop := WithAuthFailure(t.Context())
	defer stop()
	got := make(chan error, 1)
	go func() { got <- RunSlack(ctx, blocking{}, nil, RunPolicy{}) }()

	w := WatchPosts(poster{postErr: slackErr("token_revoked")}, report)
	if _, err := w.Post(t.Context(), "C1", "", slack.Content{}, "id"); err == nil {
		t.Fatal("the poster's error was swallowed")
	}

	select {
	case err := <-got:
		if !errors.Is(err, ErrSlackAuth) || errors.Is(err, context.Canceled) {
			t.Errorf("RunSlack = %v, want ErrSlackAuth and not a clean stop", err)
		}
		if !strings.Contains(err.Error(), "token_revoked") {
			t.Errorf("RunSlack = %v, want the code token_revoked named", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run was not ended")
	}
}

// The same wiring with an error that is not fatal leaves the run up, and a shutdown stays a shutdown.
func TestOtherPostErrorsLeaveTheRunAloneAndAShutdownStaysClean(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	ctx, report, stop := WithAuthFailure(parent)
	defer stop()
	got := make(chan error, 1)
	go func() { got <- RunSlack(ctx, blocking{}, nil, RunPolicy{}) }()

	w := WatchPosts(poster{postErr: slackErr("channel_not_found")}, report)
	_, _ = w.Post(t.Context(), "C1", "", slack.Content{}, "id")
	select {
	case err := <-got:
		t.Fatalf("a post error that is not about the tokens ended the run: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	if err := <-got; !errors.Is(err, context.Canceled) || errors.Is(err, ErrSlackAuth) {
		t.Errorf("RunSlack after a signal = %v, want a clean stop", err)
	}
}

// reactor is an outbox.Reactor that answers with the error the test sets.
type reactor struct{ err error }

func (r reactor) React(context.Context, string, string, string) error { return r.err }

// WatchReacts is WatchPosts for the reactions (D6): a fatal auth code is reported once, the error passes
// through untouched, and any other error, or none, reports nothing.
func TestWatchReactsReportsOnlyFatalAuthAndPassesTheErrorThrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string // the code reported, "" for none
	}{
		{"revoked token", slackErr("token_revoked"), "token_revoked"},
		{"invalid auth", slackErr("invalid_auth"), "invalid_auth"},
		{"a message that is gone", slackErr("message_not_found"), ""},
		{"a rate limit", slackErr("ratelimited"), ""},
		{"success", nil, ""},
	} {
		var reported []string
		w := WatchReacts(reactor{tc.err}, func(code string) { reported = append(reported, code) })

		err := w.React(t.Context(), "C1", "1.1", "eyes")

		if !errors.Is(err, tc.err) && tc.err != nil || tc.err == nil && err != nil {
			t.Errorf("%s: React = %v, want the reactor's own error", tc.name, err)
		}
		if tc.want == "" && len(reported) != 0 || tc.want != "" && (len(reported) != 1 || reported[0] != tc.want) {
			t.Errorf("%s: reported %v, want %q", tc.name, reported, tc.want)
		}
	}
}
