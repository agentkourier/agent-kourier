package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// platform is an outbox.Platform whose Post, Find and React answer with the errors the test sets, and that says
// what the Slack adapter says of them.
type platform struct{ postErr, findErr, reactErr error }

func (p platform) Post(context.Context, chat.Target, chat.Outgoing, string) (chat.Ref, error) {
	return chat.Ref{ID: "1.0"}, p.postErr
}

func (p platform) Find(context.Context, chat.Target, string, time.Time) (chat.Ref, bool, error) {
	return chat.Ref{ID: "2.0"}, true, p.findErr
}

func (p platform) React(context.Context, chat.Ref, string) error { return p.reactErr }

func (platform) RefAt(channel, thread, id string) chat.Ref {
	return chat.Ref{Channel: channel, Thread: thread, ID: id}
}

func (platform) Fits(chat.Prompt) error { return nil }

func (platform) Classify(op chat.Op, err error) chat.Verdict {
	return slack.NewPlatform(nil, "ns/conn").Classify(op, err)
}

func slackErr(code string) error {
	return fmt.Errorf("slack chat.postMessage: %w", slackgo.SlackErrorResponse{Err: code})
}

// WatchAuth reports a credential that the platform rejects, once per call that fails with it, for a post, a lookup and
// a reaction alike; the answer passes through untouched, and nothing else is reported.
func TestWatchAuthReportsOnlyFatalAuthAndPassesEverythingThrough(t *testing.T) {
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
		{"a message that is gone", slackErr("message_not_found"), ""},
		{"a rate limit", slackErr("ratelimited"), ""},
		{"a network error", errors.New("dial tcp: connection refused"), ""},
		{"success", nil, ""},
	} {
		for _, call := range []string{"Post", "Find", "React"} {
			var reported []string
			w := WatchAuth(platform{postErr: tc.err, findErr: tc.err, reactErr: tc.err}, func(code string) { reported = append(reported, code) })

			var ref chat.Ref
			var err error
			switch call {
			case "Post":
				ref, err = w.Post(t.Context(), chat.Target{Channel: "C1"}, chat.Outgoing{}, "id")
			case "Find":
				ref, _, err = w.Find(t.Context(), chat.Target{Channel: "C1"}, "id", time.Time{})
			case "React":
				err = w.React(t.Context(), chat.Ref{Channel: "C1", ID: "1.1"}, "eyes")
			}

			if wantID := map[string]string{"Post": "1.0", "Find": "2.0", "React": ""}[call]; ref.ID != wantID || !errors.Is(err, tc.err) && tc.err != nil || tc.err == nil && err != nil {
				t.Errorf("%s/%s: got %+v, %v; want the platform's own answer untouched", tc.name, call, ref, err)
			}
			if tc.want == "" && len(reported) != 0 || tc.want != "" && (len(reported) != 1 || reported[0] != tc.want) {
				t.Errorf("%s/%s: reported %v, want %q", tc.name, call, reported, tc.want)
			}
		}
	}
}

// blocking is a SlackRunner that holds the connection until its context ends, as a healthy one does.
type blocking struct{}

func (blocking) Run(ctx context.Context, _ chat.Sink) error { <-ctx.Done(); return ctx.Err() }

func (blocking) Classify(op chat.Op, err error) chat.Verdict {
	return slack.NewPlatform(nil, "ns/conn").Classify(op, err)
}

// A bot token revoked while the connection is up fails posts with invalid_auth, which the outbox
// treats as transient. The report ends the run with ErrSlackAuth, so the process exits and says so.
func TestAPostThatFindsTheTokenRevokedEndsTheSlackRun(t *testing.T) {
	ctx, report, stop := WithAuthFailure(t.Context())
	defer stop()
	got := make(chan error, 1)
	go func() { got <- RunPlatform(ctx, blocking{}, nil, RunPolicy{}) }()

	w := WatchAuth(platform{postErr: slackErr("token_revoked")}, report)
	if _, err := w.Post(t.Context(), chat.Target{Channel: "C1"}, chat.Outgoing{}, "id"); err == nil {
		t.Fatal("the platform's error was swallowed")
	}

	select {
	case err := <-got:
		if !errors.Is(err, ErrSlackAuth) || errors.Is(err, context.Canceled) {
			t.Errorf("RunPlatform = %v, want ErrSlackAuth and not a clean stop", err)
		}
		if !strings.Contains(err.Error(), "token_revoked") {
			t.Errorf("RunPlatform = %v, want the code token_revoked named", err)
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
	go func() { got <- RunPlatform(ctx, blocking{}, nil, RunPolicy{}) }()

	w := WatchAuth(platform{postErr: slackErr("channel_not_found")}, report)
	_, _ = w.Post(t.Context(), chat.Target{Channel: "C1"}, chat.Outgoing{}, "id")
	select {
	case err := <-got:
		t.Fatalf("a post error that is not about the tokens ended the run: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	if err := <-got; !errors.Is(err, context.Canceled) || errors.Is(err, ErrSlackAuth) {
		t.Errorf("RunPlatform after a signal = %v, want a clean stop", err)
	}
}
