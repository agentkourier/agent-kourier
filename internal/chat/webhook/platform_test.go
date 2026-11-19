package webhook_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/webhook"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

func TestNewRefusesAConfigItCannotRunWith(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*webhook.Config)
	}{
		{"no connection", func(c *webhook.Config) { c.Connection = "" }},
		{"no channel shape", func(c *webhook.Config) { c.ValidChannel = nil }},
		{"no verify header", func(c *webhook.Config) { c.Verify.Header = "" }},
		{"an unknown scheme", func(c *webhook.Config) { c.Verify.Scheme = "md5" }},
		{"a timestamp on the token scheme", func(c *webhook.Config) {
			c.Verify.Scheme, c.Verify.Secret.Value = webhook.SchemeToken, secret.New(fakeToken)
			c.Verify.Timestamp = &webhook.Timestamp{From: "/ts", Unit: time.Second, Window: time.Minute}
		}},
		{"a timestamp pointer without its slash", func(c *webhook.Config) {
			c.Verify.Timestamp = &webhook.Timestamp{From: "ts", Unit: time.Second, Window: time.Minute}
		}},
		{"a timestamp in minutes", func(c *webhook.Config) {
			c.Verify.Timestamp = &webhook.Timestamp{From: "/ts", Unit: time.Minute, Window: time.Minute}
		}},
		{"a timestamp with no window", func(c *webhook.Config) { c.Verify.Timestamp = &webhook.Timestamp{From: "/ts", Unit: time.Second} }},
		{"a channel with pointers and a value", func(c *webhook.Config) { c.Channel.Value = "acme/widgets" }},
		{"a channel with neither", func(c *webhook.Config) { c.Channel.From = nil }},
		{"a channel value not of the shape", func(c *webhook.Config) { c.Channel = webhook.Channel{Value: "#general"} }},
		{"a channel value with a colon", func(c *webhook.Config) {
			c.Channel, c.ValidChannel = webhook.Channel{Value: "a:b"}, func(string) bool { return true }
		}},
		{"a channel pointer that is no pointer", func(c *webhook.Config) { c.Channel.From = []string{"repository"} }},
		{"no actor pointer", func(c *webhook.Config) { c.Actor.From = nil }},
		{"no identity", func(c *webhook.Config) { c.Identity = "" }},
		{"actor pointers and none", func(c *webhook.Config) { c.Actor.None = true }},
		{"a field with no name", func(c *webhook.Config) { c.Fields[0].Name = "" }},
		{"a field named twice", func(c *webhook.Config) { c.Fields[1].Name = c.Fields[0].Name }},
		{"a field with a header and pointers", func(c *webhook.Config) { c.Fields[0].From = []string{"/action"} }},
		{"a field with neither", func(c *webhook.Config) { c.Fields[0].Header = "" }},
		{"a field that reads the verify header", func(c *webhook.Config) { c.Fields[0].Header = strings.ToLower(sigHeader) }},
		{"a field that reads Authorization", func(c *webhook.Config) { c.Fields[0].Header = "authorization" }},
		{"a field that reads a cookie", func(c *webhook.Config) { c.Fields[0].Header = "Cookie" }},
		{"a delivery header that is the verify header", func(c *webhook.Config) { c.DeliveryHeader = sigHeader }},
		{"a delivery header that is Proxy-Authorization", func(c *webhook.Config) { c.DeliveryHeader = "Proxy-Authorization" }},
		{"a thread of no field", func(c *webhook.Config) { c.Thread.Field = "missing" }},
		{"a thread of a text field", func(c *webhook.Config) { c.Thread.Field = "body" }},
		{"a thread prefix with a colon", func(c *webhook.Config) { c.Thread.Prefix = "pr:" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config(nil, nil)
			cfg.Fields = slices.Clone(cfg.Fields)
			cfg.Thread = &webhook.Thread{Field: cfg.Thread.Field, Prefix: cfg.Thread.Prefix}
			tc.edit(&cfg)
			if _, err := webhook.New(cfg); err == nil {
				t.Fatal("built")
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*webhook.Config)
	}{
		{"the test's config", func(*webhook.Config) {}},
		{"a fixed channel", func(c *webhook.Config) { c.Channel = webhook.Channel{Value: "OPS"} }},
		{"no actors", func(c *webhook.Config) { c.Actor, c.Identity = webhook.Actor{None: true}, "" }},
		{"no thread and no delivery header", func(c *webhook.Config) { c.Thread, c.DeliveryHeader = nil, "" }},
		{"the token scheme", func(c *webhook.Config) {
			c.Verify.Scheme, c.Verify.Secret.Value = webhook.SchemeToken, secret.New(fakeToken)
		}},
		{"a timestamp in milliseconds", func(c *webhook.Config) {
			c.Verify.Timestamp = &webhook.Timestamp{From: "/ts", Unit: time.Millisecond, Window: time.Minute}
		}},
	} {
		cfg := config(nil, nil)
		tc.edit(&cfg)
		if _, err := webhook.New(cfg); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// A secret or a token with a control character, most often the newline that a file read into a Secret keeps, is refused when the
// platform is built: used raw, the HMAC key would not be the sender's, and every delivery would be refused as unverified. The error
// names the Secret and the key, wraps ErrControlCharacter, and never holds the value. An empty one, and a short token, are refused too.
func TestNewRefusesACredentialItCannotUse(t *testing.T) {
	const tail = "zq-tail"
	for _, tc := range []struct {
		name   string
		scheme webhook.Scheme
		value  string
		want   error
	}{
		{"a secret with a trailing newline", webhook.SchemeHMACSHA256, fakeSecret + tail + "\n", webhook.ErrControlCharacter},
		{"a secret with a trailing CRLF", webhook.SchemeHMACSHA256, fakeSecret + tail + "\r\n", webhook.ErrControlCharacter},
		{"a secret with a NUL inside", webhook.SchemeHMACSHA256, fakeSecret + "\x00" + tail, webhook.ErrControlCharacter},
		{"a secret with a tab inside", webhook.SchemeHMACSHA256, fakeSecret + "\t" + tail, webhook.ErrControlCharacter},
		{"a secret with DEL", webhook.SchemeHMACSHA256, fakeSecret + tail + "\x7f", webhook.ErrControlCharacter},
		{"a secret with a C1 control", webhook.SchemeHMACSHA256, fakeSecret + tail + "\u0085", webhook.ErrControlCharacter},
		{"a token with a trailing newline", webhook.SchemeToken, fakeToken + tail + "\n", webhook.ErrControlCharacter},
		{"an empty secret", webhook.SchemeHMACSHA256, "", secret.ErrEmptyValue},
		{"an empty token", webhook.SchemeToken, "", secret.ErrEmptyValue},
		{"a short token", webhook.SchemeToken, tail + strings.Repeat("x", webhook.MinTokenLen-len(tail)-1), webhook.ErrTokenTooShort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config(nil, nil)
			cfg.Verify.Scheme = tc.scheme
			cfg.Verify.Secret = webhook.Credential{Secret: "team-a/github-hook", Key: "webhookSecret", Value: secret.New(tc.value)}
			_, err := webhook.New(cfg)
			if !errors.Is(err, tc.want) {
				t.Fatalf("New = %v, want %v", err, tc.want)
			}
			msg := err.Error()
			if !strings.Contains(msg, "team-a/github-hook") || !strings.Contains(msg, `"webhookSecret"`) || !strings.Contains(msg, "ns/hooks") {
				t.Errorf("the error %q does not name the connection, the Secret and the key", msg)
			}
			if strings.Contains(msg, tail) || strings.Contains(msg, fakeSecret) {
				t.Errorf("the error %q holds the value", msg)
			}
		})
	}
	cfg := config(nil, nil)
	cfg.Verify.Scheme, cfg.Verify.Secret.Value = webhook.SchemeToken, secret.New(strings.Repeat("t", webhook.MinTokenLen))
	if _, err := webhook.New(cfg); err != nil {
		t.Errorf("a token of the least length: %v", err)
	}
}

// What the platform has none of, each method says in the way its contract allows; it sends nothing at all.
func TestTheMethodsSayWhatThePlatformLacks(t *testing.T) {
	p := newPlatform(t, config(nil, nil))
	ctx := t.Context()
	target := chat.Target{Conn: "ns/hooks", Channel: "acme/widgets", Thread: "pr-1"}
	ref := p.RefAt("acme/widgets", "pr-1", "e-1")
	if want := (chat.Ref{Platform: "webhook", Conn: "ns/hooks", Channel: "acme/widgets", Thread: "pr-1", ID: "e-1"}); ref != want {
		t.Errorf("RefAt = %+v, want %+v", ref, want)
	}
	check := func(name string, err error, want ...error) {
		t.Helper()
		for _, w := range want {
			if !errors.Is(err, w) {
				t.Errorf("%s: %v, want %v", name, err, w)
			}
		}
	}
	_, err := p.Post(ctx, target, chat.Outgoing{}, "tag")
	check("Post", err, webhook.ErrNoReplies, chat.ErrInvalid)
	check("Update", p.Update(ctx, ref, chat.Outgoing{}), webhook.ErrNoReplies, chat.ErrInvalid)
	check("Fits", p.Fits(chat.Prompt{}), webhook.ErrNoReplies, chat.ErrInvalid)
	check("Ephemeral", p.Ephemeral(ctx, target, "u-1", chat.Message{Fallback: "x"}), webhook.ErrNoReplies, chat.ErrInvalid, chat.ErrNoEphemeral)
	for _, op := range []chat.Op{chat.OpPost, chat.OpOther} {
		if v := p.Classify(op, err); v.Class != chat.Permanent {
			t.Errorf("Classify(%v) of a post is %+v, want Permanent", op, v)
		}
	}
	if r, found, err := p.Find(ctx, target, "tag", epoch); found || err != nil || r != (chat.Ref{}) {
		t.Errorf("Find = %+v, %t, %v; want not found", r, found, err)
	}
	_, err = p.Stream(ctx, target, "u-1", "x")
	check("Stream", err, chat.ErrNoStreams)
	st := p.StreamAt(ref)
	if st.Ref() != ref {
		t.Errorf("StreamAt's Ref is %+v, want %+v", st.Ref(), ref)
	}
	check("Append", st.Append(ctx, "x"), chat.ErrNotStreaming)
	check("Step", st.Step(ctx, chat.Step{ID: "s"}), chat.ErrNotStreaming)
	check("Stop", st.Stop(ctx, "", chat.Message{}, "t"), chat.ErrNotStreaming)
	check("React", p.React(ctx, ref, "eyes"), chat.ErrNoReactions)
	check("Unreact", p.Unreact(ctx, ref, "eyes"), chat.ErrNoReactions)
	check("MarkBusy", p.MarkBusy(ctx, target, true), chat.ErrNoBusy)
	_, _, err = p.ThreadBetween(ctx, target, "", "", 10)
	check("ThreadBetween", err, chat.ErrNoThreadReads)
	_, _, err = p.ThreadFromStart(ctx, target, "", 10)
	check("ThreadFromStart", err, chat.ErrNoThreadReads)
	if c := p.Capabilities(); c.Streams || c.Steps || c.Ephemeral || c.Busy || c.AckReaction != "" {
		t.Errorf("Capabilities = %+v, want none", c)
	}
	if link, ok := p.Permalink(ref); ok || link != "" {
		t.Errorf("Permalink: %q, %t; want none", link, ok)
	}
	if member, err := p.IsMember(ctx, "acme/widgets", "u-1"); member || err != nil {
		t.Errorf("IsMember: %t, %v; want false: there are no presses to authorise", member, err)
	}
	if bot, err := p.IsBot(ctx, "u-1"); bot || err != nil {
		t.Errorf("IsBot: %t, %v; want false", bot, err)
	}
	if u, err := p.ResolveUser(ctx, "u-1"); err != nil || u.ID != "u-1" {
		t.Errorf("ResolveUser: %+v, %v; want the ID", u, err)
	}
	in, err := p.IsInAnyGroup(ctx, []string{"grp-1"}, "u-1")
	if in || !errors.Is(err, chat.ErrGroupsMisconfigured) {
		t.Errorf("IsInAnyGroup: %t, %v; want false and chat.ErrGroupsMisconfigured", in, err)
	}
	if in, err := p.IsInAnyGroup(ctx, nil, "u-1"); in || err != nil {
		t.Errorf("IsInAnyGroup of no groups: %t, %v; want false and no error", in, err)
	}
	var ge *chat.GroupError
	if err := p.GroupsReadable(ctx, []string{"grp-1", "grp-2"}); !errors.As(err, &ge) || ge.Group != "grp-1" || !errors.Is(err, chat.ErrGroupNotFound) {
		t.Errorf("GroupsReadable: %v, want a GroupError of grp-1 that wraps chat.ErrGroupNotFound", err)
	}
	if err := p.GroupsReadable(ctx, nil); err != nil {
		t.Errorf("GroupsReadable of no groups: %v", err)
	}
	if v := p.Classify(chat.OpOther, context.Canceled); v.Class != chat.Transient {
		t.Errorf("Classify of a cancelled context = %+v, want Transient", v)
	}
}

// ChannelAccess has no platform to ask: a channel of the webhook shape is one Agent Kourier is in, and any other is not found.
func TestChannelAccessReadsTheShape(t *testing.T) {
	p := newPlatform(t, config(nil, nil))
	ctx := t.Context()
	if a, err := p.ChannelAccess(ctx, "acme/widgets"); err != nil || a != (chat.Access{Member: true}) {
		t.Errorf("a channel of the shape: %+v, %v; want a member", a, err)
	}
	for _, ch := range []string{"", "a:b", "#general", "acme//widgets", strings.Repeat("c", 141)} {
		a, err := p.ChannelAccess(ctx, ch)
		if !errors.Is(err, chat.ErrChannelNotFound) || a != (chat.Access{}) {
			t.Errorf("%q: %+v, %v; want chat.ErrChannelNotFound", ch, a, err)
		}
		if v := p.Classify(chat.OpOther, err); v.Class != chat.Permanent || v.Reason != chat.ReasonChannelGone {
			t.Errorf("%q: Classify is %+v, want Permanent, channel gone", ch, v)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.ChannelAccess(cancelled, "acme/widgets"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context: %v, want context.Canceled", err)
	}
}

// The connection is up while a Run takes requests: Config.OnConnection is told true when it starts to, and false once it stops.
func TestOnConnectionFollowsTheRun(t *testing.T) {
	var mu sync.Mutex
	var told []bool
	cfg := config(nil, testutil.DiscardLogger())
	cfg.OnConnection = func(up bool) { mu.Lock(); told = append(told, up); mu.Unlock() }
	p := newPlatform(t, cfg)
	toldNow := func() []bool { mu.Lock(); defer mu.Unlock(); return slices.Clone(told) }
	stop := running(t, p, &sink{})
	// Run stores the run before it tells OnConnection, so running can return before the call: wait for it.
	testutil.Eventually(t, "the connection to be told it is up", func() bool { return len(toldNow()) > 0 })
	if got := toldNow(); !slices.Equal(got, []bool{true}) {
		t.Errorf("told %v while the run takes requests, want [true]", got)
	}
	stop()
	got := toldNow()
	if !slices.Equal(got, []bool{true, false}) {
		t.Errorf("told %v once the run ended, want [true false]", got)
	}
}
