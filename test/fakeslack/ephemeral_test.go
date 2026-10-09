package fakeslack_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

func TestPostEphemeralIsVisibleOnlyToTheFakesInspection(t *testing.T) {
	fake, api := newFake(t)
	root := post(t, api, text("root"))
	blocks := slack.NewSectionBlock(mrkdwn("Please answer every question."), nil, nil)

	ts, err := api.PostEphemeral("C1", "U1", text("incomplete"), slack.MsgOptionTS(root), slack.MsgOptionBlocks(blocks))
	if err != nil || ts == "" {
		t.Fatalf("PostEphemeral = %q, %v", ts, err)
	}
	got := fake.Ephemerals("C1")
	if len(got) != 1 || got[0].Recipient != "U1" || got[0].TS != ts || got[0].Text != "incomplete" || got[0].ThreadTS != root ||
		got[0].BotID != "BFAKEBOT" || len(got[0].Blocks) == 0 {
		t.Fatalf("Ephemerals = %+v; want one, for U1, in the thread, from the bot", got)
	}
	if ts == root {
		t.Fatal("the ephemeral reused the root's ts")
	}

	// Nothing a user of the channel could read: not in messages, history or replies.
	if n := len(fake.Messages("C1")); n != 1 {
		t.Fatalf("Messages has %d messages; want just the root", n)
	}
	h, err := api.GetConversationHistory(&slack.GetConversationHistoryParameters{ChannelID: "C1"})
	if err != nil || len(h.Messages) != 1 || h.Messages[0].ReplyCount != 0 {
		t.Fatalf("history = %+v, %v; want just the root, with no replies counted", h, err)
	}
	replies, _, _, err := api.GetConversationReplies(&slack.GetConversationRepliesParameters{ChannelID: "C1", Timestamp: root})
	if err != nil || len(replies) != 1 {
		t.Fatalf("replies = %+v, %v; want just the root", replies, err)
	}
	if thread := fake.Thread("C1", root); len(thread) != 1 {
		t.Fatalf("Thread = %+v; want just the root", thread)
	}
	if n := len(fake.Ephemerals("C2")); n != 0 {
		t.Fatalf("Ephemerals(C2) = %d; want none", n)
	}
}

func TestPostEphemeralRefusesWhatSlackWould(t *testing.T) {
	_, api := newFake(t)
	tests := []struct {
		name          string
		channel, user string
		opts          []slack.MsgOption
		want          string
	}{
		{"no channel", "", "U1", []slack.MsgOption{text("x")}, "channel_not_found"},
		{"no user", "C1", "", []slack.MsgOption{text("x")}, "invalid_arguments"},
		{"no text or blocks", "C1", "U1", nil, "no_text"},
		{"invalid blocks", "C1", "U1", []slack.MsgOption{slack.MsgOptionBlocks(slack.NewSectionBlock(nil, nil, nil))}, "invalid_blocks"},
		{"text too long", "C1", "U1", []slack.MsgOption{text(chars(40001))}, "msg_too_long"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := api.PostEphemeral(tc.channel, tc.user, tc.opts...); err == nil || err.Error() != tc.want {
				t.Fatalf("PostEphemeral = %v; want %q", err, tc.want)
			}
		})
	}
}

func TestPostEphemeralRefusesANonMember(t *testing.T) {
	fake, api := newFake(t)
	fake.SetMembers("C1", "U1", "U2")
	for user, want := range map[string]string{"U1": "", "U2": "", "U3": "user_not_in_channel", "UFAKEBOT": "user_not_in_channel"} {
		_, err := api.PostEphemeral("C1", user, text("x"))
		if (err == nil) != (want == "") || (err != nil && err.Error() != want) {
			t.Errorf("PostEphemeral to %s = %v; want %q", user, err, want)
		}
	}
	// A channel with no registered membership accepts anyone.
	if _, err := api.PostEphemeral("C9", "U3", text("x")); err != nil {
		t.Errorf("PostEphemeral in an unregistered channel = %v; want success", err)
	}
	if n := len(fake.Ephemerals("C1")); n != 2 {
		t.Errorf("Ephemerals(C1) = %d; want the two to members", n)
	}
}

func TestPostEphemeralNeedsABotToken(t *testing.T) {
	fake, _ := newFake(t)
	form := url.Values{"token": {testAppToken}, "channel": {"C1"}, "user": {"U1"}, "text": {"x"}}
	if body := rawBody(t, fake, "chat.postEphemeral", form); !strings.Contains(body, `"error":"invalid_auth"`) {
		t.Fatalf("chat.postEphemeral with an app token = %s; want invalid_auth", body)
	}
}

// Slack sends no event for an ephemeral message, so Agent Kourier's notice to a user
// must not come back to it as a message event.
func TestPostEphemeralDeliversNoEvent(t *testing.T) {
	fake, smc := newSocket(t)
	waitEvent(t, smc, socketmode.EventTypeHello)
	if _, err := botClient(fake).PostEphemeral("C1", "U1", text("please answer")); err != nil {
		t.Fatalf("PostEphemeral: %v", err)
	}
	if _, err := fake.DM("U1", "hi"); err != nil {
		t.Fatalf("DM: %v", err)
	}
	if _, _, first := nextInner(t, smc); first.(*slackevents.MessageEvent).User != "U1" {
		t.Fatalf("first event = %#v; want the user's DM, with no event for the ephemeral", first)
	}
}
