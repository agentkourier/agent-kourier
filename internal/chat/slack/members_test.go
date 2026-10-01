package slack_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// membersClient returns a Client over a fake Slack whose C1 has members U1 to U7, paged two at a
// time, with every request going through calls.
func membersClient(t *testing.T, mod func(*slack.Config)) (*fakeslack.Server, *slack.Client, *counter) {
	t.Helper()
	fake := newFake(t)
	fake.SetMembers("C1", "U1", "U2", "U3", "U4", "U5", "U6", "U7")
	fake.AddUser(fakeslack.User{ID: "U1", Name: "alice"})
	fake.AddUser(fakeslack.User{ID: "UBOT", Name: "helper", IsBot: true})
	calls := &counter{n: map[string]int{}}
	cfg := clientConfig(fake)
	cfg.HTTPClient, cfg.PageLimit = &http.Client{Transport: calls}, 2
	if mod != nil {
		mod(&cfg)
	}
	return fake, slack.New(cfg), calls
}

func TestIsChannelMember(t *testing.T) {
	_, c, calls := membersClient(t, nil)
	for _, tc := range []struct {
		user  string
		want  bool
		pages int // conversations.members calls this one needs
	}{
		{"U1", true, 1},  // on the first page: no need to read on
		{"U7", true, 4},  // only on the last page
		{"U8", false, 4}, // nowhere: every page is read before saying so
	} {
		before := calls.count("/conversations.members")
		got, err := c.IsChannelMember(t.Context(), "C1", tc.user)
		if used := calls.count("/conversations.members") - before; err != nil || got != tc.want || used != tc.pages {
			t.Errorf("IsChannelMember(%s) = %v, %v after %d calls; want %v after %d", tc.user, got, err, used, tc.want, tc.pages)
		}
	}
}

// failNth passes requests on, but fails the nth to path.
type failNth struct {
	path  string
	n     int32
	seen  atomic.Int32
	inner http.RoundTripper
}

func (f *failNth) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == f.path && f.seen.Add(1) == f.n {
		return nil, errors.New("connection reset")
	}
	return f.inner.RoundTrip(r)
}

func TestIsChannelMemberFailsClosed(t *testing.T) {
	_, c, _ := membersClient(t, func(cfg *slack.Config) {
		cfg.HTTPClient = &http.Client{Transport: &failNth{path: "/conversations.members", n: 2, inner: http.DefaultTransport}}
	})
	// U7 is a member, on the last page, but the second page failed: not known to be a member.
	if got, err := c.IsChannelMember(t.Context(), "C1", "U7"); got || err == nil {
		t.Errorf("IsChannelMember across a failed page = %v, %v; want false and the error", got, err)
	}
	_, c, _ = membersClient(t, nil)
	if got, err := c.IsChannelMember(t.Context(), "CNONE", "U1"); got || err == nil {
		t.Errorf("IsChannelMember of an unknown channel = %v, %v; want false and the error", got, err)
	}
}

// TestIsChannelMemberIsBoundedAtTwoSeconds: a Slack that never answers cannot hold a press, though
// the caller's context has no deadline.
func TestIsChannelMemberIsBoundedAtTwoSeconds(t *testing.T) {
	_, c, calls := membersClient(t, nil)
	calls.hang = true
	start := time.Now()
	got, err := c.IsChannelMember(t.Context(), "C1", "U1")
	if d := time.Since(start); got || !errors.Is(err, context.DeadlineExceeded) || d < 1900*time.Millisecond || d > 4*time.Second {
		t.Errorf("IsChannelMember on a hanging Slack = %v, %v after %v; want false and a deadline error after about 2 s", got, err, d)
	}
}

func TestIsBot(t *testing.T) {
	_, c, _ := membersClient(t, nil)
	for user, want := range map[string]bool{"UBOT": true, "U1": false} {
		if got, err := c.IsBot(t.Context(), user); err != nil || got != want {
			t.Errorf("IsBot(%s) = %v, %v; want %v", user, got, err, want)
		}
	}
	// Unknown is treated as a bot: a caller that ignores the error still denies.
	if got, err := c.IsBot(t.Context(), "UNOBODY"); !got || err == nil {
		t.Errorf("IsBot of an unknown user = %v, %v; want true and the error", got, err)
	}
}

func TestPostEphemeralReachesOnlyItsUserAndIsInert(t *testing.T) {
	fake, c, calls := membersClient(t, nil)
	blocks := []byte(`[{"type":"section","text":{"type":"mrkdwn","text":"<!channel> answer every question"}}]`)
	if err := c.PostEphemeral(t.Context(), "C1", "1.1", "U1", slack.Content{Text: "incomplete", Blocks: blocks}); err != nil {
		t.Fatal(err)
	}
	eph := fake.Ephemerals("C1")
	if len(eph) != 1 || eph[0].Recipient != "U1" || eph[0].ThreadTS != "1.1" || eph[0].Text != "incomplete" || len(eph[0].Blocks) == 0 {
		t.Fatalf("ephemerals = %+v; want one for U1, in the thread, with the text and blocks", eph)
	}
	if msgs := fake.Messages("C1"); len(msgs) != 0 {
		t.Errorf("the channel holds %+v; an ephemeral is not a channel message", msgs)
	}
	f := calls.sent("/chat.postEphemeral")
	if len(f) != 1 || f[0].Get("user") != "U1" || f[0].Get("thread_ts") != "1.1" || f[0].Get("unfurl_links") != "false" || f[0].Get("unfurl_media") != "false" || f[0].Get("parse") != "none" || f[0].Has("link_names") {
		t.Errorf("request = %v; want user U1, both unfurls false, parse=none and no link_names", f)
	}
	// Slack refuses a recipient outside the channel; the caller sees why.
	if err := c.PostEphemeral(t.Context(), "C1", "1.1", "U9", slack.Content{Text: "x"}); err == nil {
		t.Error("PostEphemeral to a user outside the channel succeeded")
	}
}

func TestMembershipErrorsNameNoToken(t *testing.T) {
	const bot, app, other = "canary-bot-token", "canary-app-token", "xoxp-1234-5678-abcdefgh"
	_, c, calls := membersClient(t, func(cfg *slack.Config) { cfg.BotToken, cfg.AppToken = secret.New(bot), secret.New(app) })
	calls.err = fmt.Errorf("proxy refused %s, %s and %s", bot, app, other)
	_, memberErr := c.IsChannelMember(t.Context(), "C1", "U1")
	_, botErr := c.IsBot(t.Context(), "U1")
	ephErr := c.PostEphemeral(t.Context(), "C1", "", "U1", slack.Content{Text: "x"})
	for name, err := range map[string]error{"IsChannelMember": memberErr, "IsBot": botErr, "PostEphemeral": ephErr} {
		if err == nil {
			t.Fatalf("%s succeeded through a failing transport", name)
		}
		for _, tok := range []string{bot, app, other} {
			if strings.Contains(err.Error(), tok) {
				t.Errorf("%s error holds %q: %v", name, tok, err)
			}
		}
	}
}

// TestMembershipAndBotChecksAreBoundedAtTwoSecondsAndTheBoundIsTunable: the press path waits on both,
// so neither may run on the caller's context alone (users.info used to take the 5 s UserTimeout).
func TestMembershipAndBotChecksAreBoundedAtTwoSecondsAndTheBoundIsTunable(t *testing.T) {
	check := func(name string, mod func(*slack.Config), lo, hi time.Duration) {
		_, c, calls := membersClient(t, mod)
		calls.hang = true
		for op, call := range map[string]func() error{
			"IsChannelMember": func() error { _, err := c.IsChannelMember(t.Context(), "C1", "U1"); return err },
			"IsBot":           func() error { _, err := c.IsBot(t.Context(), "U1"); return err },
		} {
			start := time.Now()
			if err, d := call(), time.Since(start); !errors.Is(err, context.DeadlineExceeded) || d < lo || d > hi {
				t.Errorf("%s, %s: %v after %v; want a deadline error between %v and %v", op, name, err, d, lo, hi)
			}
		}
	}
	check("default", nil, 1900*time.Millisecond, 3*time.Second)
	check("AuthzTimeout of 300 ms", func(c *slack.Config) { c.AuthzTimeout = 300 * time.Millisecond }, 250*time.Millisecond, time.Second)
}

// TestMembershipPagesAtTheMaximum: a big channel is read in few calls, because the 2 s bound covers
// every page; a member of a 2,500-member channel is found on the third.
func TestMembershipPagesAtTheMaximum(t *testing.T) {
	fake, c, calls := membersClient(t, func(cfg *slack.Config) { cfg.PageLimit = 0 })
	var users []string
	for i := range 2500 {
		users = append(users, fmt.Sprintf("W%04d", i))
	}
	fake.SetMembers("CBIG", users...)
	if got, err := c.IsChannelMember(t.Context(), "CBIG", "W2499"); err != nil || !got || calls.count("/conversations.members") != 3 {
		t.Errorf("IsChannelMember = %v, %v after %d calls; want true after 3", got, err, calls.count("/conversations.members"))
	}
	if f := calls.sent("/conversations.members"); len(f) == 0 || f[0].Get("limit") != "1000" {
		t.Errorf("requests %v; want a page limit of 1000", f)
	}
}

func TestPostEphemeralRefusesBadBlocksAsInvalid(t *testing.T) {
	_, c, calls := membersClient(t, nil)
	err := c.PostEphemeral(t.Context(), "C1", "", "U1", slack.Content{Text: "x", Blocks: []byte(`{not json`)})
	if !errors.Is(err, store.ErrInvalid) || calls.count("/chat.postEphemeral") != 0 {
		t.Errorf("PostEphemeral = %v after %d calls; want store.ErrInvalid and no call", err, calls.count("/chat.postEphemeral"))
	}
}
