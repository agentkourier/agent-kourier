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

// platform is a chat.Platform whose calls that reach the chat answer with the error the test sets (err, for all of them; postErr
// for the post alone, when it is set), and that says what the Slack adapter says of them.
type platform struct {
	chat.Platform
	err, postErr error
}

func (p platform) Post(context.Context, chat.Target, chat.Outgoing, string) (chat.Ref, error) {
	if p.postErr != nil {
		return chat.Ref{ID: "1.0"}, p.postErr
	}
	return chat.Ref{ID: "1.0"}, p.err
}

func (p platform) Find(context.Context, chat.Target, string, time.Time) (chat.Ref, bool, error) {
	return chat.Ref{ID: "2.0"}, true, p.err
}
func (p platform) Update(context.Context, chat.Ref, chat.Outgoing) error { return p.err }
func (p platform) Stream(context.Context, chat.Target, string, string) (chat.Stream, error) {
	return stream{err: p.err}, p.err
}
func (p platform) StreamAt(chat.Ref) chat.Stream { return stream{err: p.err} }
func (p platform) Ephemeral(context.Context, chat.Target, string, chat.Message) error {
	return p.err
}
func (p platform) React(context.Context, chat.Ref, string) error   { return p.err }
func (p platform) Unreact(context.Context, chat.Ref, string) error { return p.err }
func (p platform) IsMember(context.Context, string, string) (bool, error) {
	return true, p.err
}
func (p platform) IsInAnyGroup(context.Context, []string, string) (bool, error) {
	return true, p.err
}
func (p platform) GroupsReadable(context.Context, []string) error { return p.err }
func (p platform) ChannelAccess(context.Context, string) (chat.Access, error) {
	return chat.Access{Member: true}, p.err
}
func (p platform) IsBot(context.Context, string) (bool, error) { return false, p.err }
func (p platform) ResolveUser(context.Context, string) (chat.User, error) {
	return chat.User{ID: "U1"}, p.err
}
func (p platform) MarkBusy(context.Context, chat.Target, bool) error { return p.err }
func (p platform) ThreadBetween(context.Context, chat.Target, string, string, int) ([]chat.HistoryMessage, int, error) {
	return nil, 0, p.err
}

func (platform) RefAt(channel, thread, id string) chat.Ref {
	return chat.Ref{Channel: channel, Thread: thread, ID: id}
}

func (platform) Fits(chat.Prompt) error { return nil }

func (platform) Classify(op chat.Op, err error) chat.Verdict {
	return slack.NewPlatform(nil, "ns/conn").Classify(op, err)
}

// stream is a chat.Stream whose calls answer with err.
type stream struct {
	chat.Stream
	err error
}

func (s stream) Ref() chat.Ref                                            { return chat.Ref{ID: "3.0"} }
func (s stream) Append(context.Context, string) error                     { return s.err }
func (s stream) Step(context.Context, chat.Step) error                    { return s.err }
func (s stream) Stop(context.Context, string, chat.Message, string) error { return s.err }

func slackErr(code string) error {
	return fmt.Errorf("slack chat.postMessage: %w", slackgo.SlackErrorResponse{Err: code})
}

// watchedCalls are the calls of a chat.Platform that reach the chat, and a stream's, each made on w, with the error it answered.
var watchedCalls = map[string]func(t *testing.T, w chat.Platform) (id string, err error){
	"Post": func(t *testing.T, w chat.Platform) (string, error) {
		ref, err := w.Post(t.Context(), chat.Target{Channel: "C1"}, chat.Outgoing{}, "id")
		return ref.ID, err
	},
	"Find": func(t *testing.T, w chat.Platform) (string, error) {
		ref, _, err := w.Find(t.Context(), chat.Target{Channel: "C1"}, "id", time.Time{})
		return ref.ID, err
	},
	"Update": func(t *testing.T, w chat.Platform) (string, error) {
		return "", w.Update(t.Context(), chat.Ref{Channel: "C1", ID: "1.1"}, chat.Outgoing{})
	},
	"Stream": func(t *testing.T, w chat.Platform) (string, error) {
		_, err := w.Stream(t.Context(), chat.Target{Channel: "C1"}, "U1", "hi")
		return "", err
	},
	"stream Append": func(t *testing.T, w chat.Platform) (string, error) {
		return "", w.StreamAt(chat.Ref{Channel: "C1", ID: "1.1"}).Append(t.Context(), "more")
	},
	"stream Step": func(t *testing.T, w chat.Platform) (string, error) {
		return "", w.StreamAt(chat.Ref{Channel: "C1", ID: "1.1"}).Step(t.Context(), chat.Step{})
	},
	"stream Stop": func(t *testing.T, w chat.Platform) (string, error) {
		return "", w.StreamAt(chat.Ref{Channel: "C1", ID: "1.1"}).Stop(t.Context(), "", chat.Message{}, "tag")
	},
	"Ephemeral": func(t *testing.T, w chat.Platform) (string, error) {
		return "", w.Ephemeral(t.Context(), chat.Target{Channel: "C1"}, "U1", chat.Message{})
	},
	"React": func(t *testing.T, w chat.Platform) (string, error) {
		return "", w.React(t.Context(), chat.Ref{Channel: "C1", ID: "1.1"}, "eyes")
	},
	"Unreact": func(t *testing.T, w chat.Platform) (string, error) {
		return "", w.Unreact(t.Context(), chat.Ref{Channel: "C1", ID: "1.1"}, "eyes")
	},
	"IsMember": func(t *testing.T, w chat.Platform) (string, error) {
		_, err := w.IsMember(t.Context(), "C1", "U1")
		return "", err
	},
	"IsInAnyGroup": func(t *testing.T, w chat.Platform) (string, error) {
		_, err := w.IsInAnyGroup(t.Context(), []string{"S1"}, "U1")
		return "", err
	},
	"GroupsReadable": func(t *testing.T, w chat.Platform) (string, error) {
		return "", w.GroupsReadable(t.Context(), []string{"S1"})
	},
	"ChannelAccess": func(t *testing.T, w chat.Platform) (string, error) {
		_, err := w.ChannelAccess(t.Context(), "C1")
		return "", err
	},
	"IsBot": func(t *testing.T, w chat.Platform) (string, error) {
		_, err := w.IsBot(t.Context(), "U1")
		return "", err
	},
	"ResolveUser": func(t *testing.T, w chat.Platform) (string, error) {
		u, err := w.ResolveUser(t.Context(), "U1")
		return u.ID, err
	},
	"MarkBusy": func(t *testing.T, w chat.Platform) (string, error) {
		return "", w.MarkBusy(t.Context(), chat.Target{Channel: "C1", Thread: "1.1"}, true)
	},
	"ThreadBetween": func(t *testing.T, w chat.Platform) (string, error) {
		_, _, err := w.ThreadBetween(t.Context(), chat.Target{Channel: "C1", Thread: "1.1"}, "", "", 10)
		return "", err
	},
}

// WatchAuth reports a credential that the platform rejects, once per call that fails with it, for every call that reaches the chat (a post, a
// lookup, an edit, a stream and the calls on it, an ephemeral, a reaction, the reads of membership, groups, channels and users) alike; the
// answer passes through untouched, and nothing else is reported.
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
		for call, make := range watchedCalls {
			var reported []string
			w := WatchAuth(platform{err: tc.err}, func(code string) { reported = append(reported, code) })

			id, err := make(t, w)

			if tc.err == nil && err != nil || tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("%s/%s: error %v; want the platform's own answer untouched", tc.name, call, err)
			}
			if wantID := map[string]string{"Post": "1.0", "Find": "2.0", "ResolveUser": "U1"}[call]; id != wantID {
				t.Errorf("%s/%s: result %q; want %q, the platform's own", tc.name, call, id, wantID)
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
