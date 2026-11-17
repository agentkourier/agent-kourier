package web_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

func TestNewRefusesAConfigItCannotRunWith(t *testing.T) {
	withCaps := func(f func(*chat.Caps)) *testProfile {
		p := newTestProfile()
		f(&p.caps)
		return p
	}
	noEncoder := newTestProfile()
	noEncoder.noEncoder = true
	for name, cfg := range map[string]web.Config{
		"no connection":                      {Profile: newTestProfile(), URL: receiverURL},
		"no profile":                         {Connection: "ns/web", URL: receiverURL},
		"a profile with no encoder":          {Connection: "ns/web", Profile: noEncoder, URL: receiverURL},
		"no URL":                             {Connection: "ns/web", Profile: newTestProfile()},
		"a relative URL":                     {Connection: "ns/web", Profile: newTestProfile(), URL: "/hook"},
		"another scheme":                     {Connection: "ns/web", Profile: newTestProfile(), URL: "file:///etc/passwd"},
		"a URL with no host":                 {Connection: "ns/web", Profile: newTestProfile(), URL: "https:///hook"},
		"streams":                            {Connection: "ns/web", URL: receiverURL, Profile: withCaps(func(c *chat.Caps) { c.Streams = true })},
		"steps":                              {Connection: "ns/web", URL: receiverURL, Profile: withCaps(func(c *chat.Caps) { c.Steps = true })},
		"ephemeral messages":                 {Connection: "ns/web", URL: receiverURL, Profile: withCaps(func(c *chat.Caps) { c.Ephemeral = true })},
		"busy marks":                         {Connection: "ns/web", URL: receiverURL, Profile: withCaps(func(c *chat.Caps) { c.Busy = true })},
		"reactions":                          {Connection: "ns/web", URL: receiverURL, Profile: withCaps(func(c *chat.Caps) { c.AckReaction = "eyes" })},
		"a limit too small to hold the mark": {Connection: "ns/web", URL: receiverURL, Profile: withCaps(func(c *chat.Caps) { c.MaxText = 1 })},
	} {
		// Each case is made with its profile's own platform, so that what it lacks is what its name says.
		if p, ok := cfg.Profile.(*testProfile); ok {
			cfg.Platform = p.platform()
		}
		if _, err := web.New(cfg); err == nil {
			t.Errorf("%s: built", name)
		}
	}
	prof := newTestProfile()
	if _, err := web.New(web.Config{Connection: "ns/web", Profile: prof, Platform: prof.platform(), URL: receiverURL}); err != nil {
		t.Errorf("a profile with an encoder and a URL: %v", err)
	}
}

// The platform is made with what the API says the connection's platform takes, and refuses none.
func TestNewRefusesNoPlatform(t *testing.T) {
	if _, err := web.New(web.Config{Connection: "ns/web", Profile: newTestProfile(), URL: receiverURL}); err == nil {
		t.Error("no platform: built")
	}
}

func TestCapabilitiesAreTheProfiles(t *testing.T) {
	prof := newTestProfile()
	prof.caps.Limits = map[chat.Where]int{chat.WhereHeader: 100}
	p := newPlatform(t, prof, receiverURL, nil)
	got := p.Capabilities()
	if got.MaxText != 1000 || got.Limit(chat.WhereHeader) != 100 || got.Streams || got.AckReaction != "" {
		t.Errorf("Capabilities is %+v, want the profile's", got)
	}
}

// What the platform has none of, each method says in the way its contract allows.
func TestTheMethodsSayWhatThePlatformLacks(t *testing.T) {
	p := newPlatform(t, newTestProfile(), receiverURL, nil)
	ctx := t.Context()
	ref := p.RefAt("ch-1", "m-1", "m-2")
	check := func(name string, err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
	_, err := p.Stream(ctx, target, "u-1", "x")
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
	if err := p.Ephemeral(ctx, target, "u-1", chat.Message{Fallback: "x"}); !errors.Is(err, chat.ErrNoEphemeral) || p.Classify(chat.OpPost, err).Class != chat.Permanent {
		t.Errorf("Ephemeral: %v, want chat.ErrNoEphemeral, which is permanent", err)
	}
	if link, ok := p.Permalink(ref); ok || link != "" {
		t.Errorf("Permalink: %q, %t; want none", link, ok)
	}
	if member, err := p.IsMember(ctx, "ch-1", "u-1"); member || err != nil {
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
}

// ChannelAccess has no platform to ask: a channel of the profile's shape is one Agent Kourier is in, and any other is not found.
func TestChannelAccessReadsTheProfilesShape(t *testing.T) {
	p := newPlatform(t, newTestProfile(), receiverURL, nil)
	ctx := t.Context()
	if a, err := p.ChannelAccess(ctx, "ch-1"); err != nil || a != (chat.Access{Member: true}) {
		t.Errorf("a channel of the shape: %+v, %v; want a member", a, err)
	}
	for _, ch := range []string{"", "ch:1", "#general", "octo/repo", strings.Repeat("c1", 40)} {
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
	if _, err := p.ChannelAccess(cancelled, "ch-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context: %v, want context.Canceled", err)
	}
}

// TestLimitsPage holds docs/reference/limits.md to the bounds of the platform.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "webAckWait", web.AckWait)
	limitsdoc.Duration(t, "webBusyRetryAfter", web.BusyRetryAfter)
	limitsdoc.Duration(t, "webReplyTimeout", web.ReplyTimeout)
	limitsdoc.Duration(t, "webRetryAfter", web.MaxRetryAfter)
}
