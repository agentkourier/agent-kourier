package fakeslack_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// slashCommand is a payload slack-go's client can parse (it wants the string).
var slashCommand = map[string]string{"command": "/agent-kourier", "is_enterprise_install": "false"}

const mentionText = "<@UFAKEBOT> investigate"

// connected starts a fake with a Socket Mode client that has said hello. It
// turns off the events for the bot's own posts, so that tests of user actions
// see only those events; the own-post tests use newSocket.
func connected(t *testing.T) (*fakeslack.Server, *socketmode.Client) {
	t.Helper()
	fake, smc := newSocket(t, fakeslack.WithoutOwnPostEvents())
	waitEvent(t, smc, socketmode.EventTypeHello)
	return fake, smc
}

// nextInner waits for an Events API event, acks it as Agent Kourier would, and
// returns its envelope_id, event_id and inner event.
func nextInner(t *testing.T, smc *socketmode.Client) (envelope, eventID string, inner any) {
	t.Helper()
	ev := waitEvent(t, smc, socketmode.EventTypeEventsAPI)
	if err := smc.Ack(*ev.Request); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	api := ev.Data.(slackevents.EventsAPIEvent)
	return ev.Request.EnvelopeID, api.Data.(*slackevents.EventsAPICallbackEvent).EventID, api.InnerEvent.Data
}

// waitAcked fails the test unless the fake sees an ack for every envelope.
func waitAcked(t *testing.T, fake *fakeslack.Server, envelopes ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := fake.WaitAck(ctx, envelopes...); err != nil {
		t.Fatal(err)
	}
}

func TestEnvelopeAckIsRecorded(t *testing.T) {
	fake, smc := connected(t)
	id, err := fake.SendEnvelope("slash_commands", slashCommand)
	if err != nil || id == "" {
		t.Fatalf("SendEnvelope = %q, %v", id, err)
	}
	ev := waitEvent(t, smc, socketmode.EventTypeSlashCommand)
	if ev.Request.EnvelopeID != id {
		t.Fatalf("client saw envelope %q; want %q", ev.Request.EnvelopeID, id)
	}

	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, acked := fake.Ack(id); acked || !errors.Is(fake.WaitAck(short, id), context.DeadlineExceeded) {
		t.Fatal("envelope counted as acked before the client acked it")
	}
	if err := smc.Ack(*ev.Request, map[string]string{"text": "ok"}); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	waitAcked(t, fake, id)
	if payload, acked := fake.Ack(id); !acked || string(payload) != `{"text":"ok"}` {
		t.Fatalf("Ack(%q) = %s, %v; want the payload the client sent", id, payload, acked)
	}
	if err := fake.WaitAck(t.Context(), "env-unknown"); err == nil {
		t.Fatal("WaitAck for an envelope never sent succeeded")
	}
}

func TestMentionDeliversAppMentionThenMessage(t *testing.T) {
	fake, smc := connected(t)
	got, err := fake.Mention("C1", "U1", mentionText)
	if err != nil || got.Channel != "C1" || len(got.Envelopes) != 2 {
		t.Fatalf("Mention = %+v, %v; want two envelopes", got, err)
	}
	msgs := fake.Messages("C1")
	if len(msgs) != 1 || msgs[0].TS != got.TS || msgs[0].User != "U1" || msgs[0].Text != mentionText || msgs[0].BotID != "" {
		t.Fatalf("stored = %+v; want the user's message", msgs)
	}

	env1, event1, first := nextInner(t, smc)
	mention, ok := first.(*slackevents.AppMentionEvent)
	if !ok || mention.User != "U1" || mention.Channel != "C1" || mention.Text != mentionText ||
		mention.TimeStamp != got.TS || mention.ThreadTimeStamp != "" {
		t.Fatalf("first event = %#v; want the app_mention", first)
	}
	env2, event2, second := nextInner(t, smc)
	message, ok := second.(*slackevents.MessageEvent)
	if !ok || message.User != "U1" || message.Channel != "C1" || message.ChannelType != "channel" ||
		message.TimeStamp != got.TS || message.Text != mentionText {
		t.Fatalf("second event = %#v; want the message", second)
	}
	if env1 != got.Envelopes[0] || env2 != got.Envelopes[1] || event1 == event2 {
		t.Fatalf("envelopes %q, %q and events %q, %q; want the returned envelopes and distinct event IDs", env1, env2, event1, event2)
	}
	waitAcked(t, fake, got.Envelopes...)
}

func TestUserActionsRefuseWhatSlackWouldNotSend(t *testing.T) {
	fake, _ := connected(t)
	root := post(t, slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/")), text("root"))
	tests := []struct {
		name string
		do   func() (fakeslack.UserMessage, error)
	}{
		{"mention without the bot", func() (fakeslack.UserMessage, error) { return fake.Mention("C1", "U1", "hello there") }},
		{"mention in a DM", func() (fakeslack.UserMessage, error) {
			return fake.Mention(fakeslack.DMChannel("U1"), "U1", mentionText)
		}},
		{"mention with no channel", func() (fakeslack.UserMessage, error) { return fake.Mention("", "U1", mentionText) }},
		{"mention with no user", func() (fakeslack.UserMessage, error) { return fake.Mention("C1", "", mentionText) }},
		{"reply with no thread", func() (fakeslack.UserMessage, error) { return fake.Reply("C1", "", "U1", "x") }},
		{"reply with no user", func() (fakeslack.UserMessage, error) { return fake.Reply("C1", root, "", "x") }},
		{"reply with no text", func() (fakeslack.UserMessage, error) { return fake.Reply("C1", root, "U1", "") }},
		{"DM from no user", func() (fakeslack.UserMessage, error) { return fake.DM("", "x") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := tc.do(); err == nil {
				t.Fatalf("succeeded with %+v; want an error", got)
			}
		})
	}
	if n := len(fake.Messages("C1")) + len(fake.Messages(fakeslack.DMChannel("U1"))); n != 1 { // only the root
		t.Fatalf("refused actions stored messages: %d in all; want just the root", n)
	}
}

func TestReplyMentioningTheBotDeliversAppMentionThenMessage(t *testing.T) {
	fake, smc := connected(t)
	root := post(t, slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/")), text("investigating"))
	fake.SetChannelType("C1", "group") // a private channel

	got, err := fake.Reply("C1", root, "U1", mentionText)
	if err != nil || len(got.Envelopes) != 2 {
		t.Fatalf("Reply = %+v, %v; want two envelopes", got, err)
	}
	_, _, first := nextInner(t, smc)
	mention, ok := first.(*slackevents.AppMentionEvent)
	if !ok || mention.ThreadTimeStamp != root || mention.TimeStamp != got.TS || mention.Channel != "C1" {
		t.Fatalf("first event = %#v; want the app_mention in thread %s", first, root)
	}
	_, _, second := nextInner(t, smc)
	if m, ok := second.(*slackevents.MessageEvent); !ok || m.ThreadTimeStamp != root || m.TimeStamp != got.TS || m.ChannelType != "group" {
		t.Fatalf("second event = %#v; want the group message in thread %s", second, root)
	}

	// In a DM, Slack sends no app_mention.
	dm, err := fake.DM("U1", "hi")
	if err != nil {
		t.Fatalf("DM: %v", err)
	}
	wantMessage(t, smc, dm, "", "im", "hi")
	inDM, err := fake.Reply(dm.Channel, dm.TS, "U1", mentionText)
	if err != nil || len(inDM.Envelopes) != 1 {
		t.Fatalf("Reply mentioning the bot in a DM = %+v, %v; want one envelope", inDM, err)
	}
}

func TestEnvelopesCanAcceptResponsePayloads(t *testing.T) {
	fake, smc := connected(t)
	for _, tc := range []struct {
		opts []fakeslack.EnvelopeOption
		want bool
	}{{nil, false}, {[]fakeslack.EnvelopeOption{fakeslack.AcceptsResponsePayload()}, true}} {
		if _, err := fake.SendEnvelope("slash_commands", slashCommand, tc.opts...); err != nil {
			t.Fatalf("SendEnvelope: %v", err)
		}
		if got := waitEvent(t, smc, socketmode.EventTypeSlashCommand).Request.AcceptsResponsePayload; got != tc.want {
			t.Fatalf("accepts_response_payload = %v; want %v", got, tc.want)
		}
	}
}

// wantMessage waits for the next event and checks it is a message event for got.
func wantMessage(t *testing.T, smc *socketmode.Client, got fakeslack.UserMessage, thread, channelType, text string) {
	t.Helper()
	env, _, inner := nextInner(t, smc)
	m, ok := inner.(*slackevents.MessageEvent)
	if !ok || m.Channel != got.Channel || m.ChannelType != channelType || m.ThreadTimeStamp != thread ||
		m.TimeStamp != got.TS || m.Text != text || m.User != "U1" || env != got.Envelopes[0] {
		t.Fatalf("event = %#v (envelope %s); want a %s message %q in thread %q", inner, env, channelType, text, thread)
	}
}

func TestReplyAndDMDeliverMessageEvents(t *testing.T) {
	fake, smc := connected(t)
	bot := slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/"))
	_, root, err := bot.PostMessage("C1", text("investigating"))
	if err != nil {
		t.Fatalf("post root: %v", err)
	}

	reply, err := fake.Reply("C1", root, "U1", "more")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	wantMessage(t, smc, reply, root, "channel", "more")
	if thread := fake.Thread("C1", root); len(thread) != 2 || thread[1].TS != reply.TS || thread[1].User != "U1" {
		t.Fatalf("thread = %+v; want the parent and the user's reply", thread)
	}
	dm, err := fake.DM("U1", "hi")
	if err != nil || dm.Channel != fakeslack.DMChannel("U1") || dm.Channel[0] != 'D' {
		t.Fatalf("DM = %+v, %v; want a D channel", dm, err)
	}
	wantMessage(t, smc, dm, "", "im", "hi")
	inDM, err := fake.Reply(dm.Channel, dm.TS, "U1", "again")
	if err != nil {
		t.Fatalf("Reply in DM: %v", err)
	}
	wantMessage(t, smc, inDM, dm.TS, "im", "again")
	if _, err := fake.Reply("C1", "1.000001", "U1", "orphan"); err == nil {
		t.Fatal("Reply to a thread that does not exist succeeded")
	}
}

func TestUserActionsNeedAClient(t *testing.T) {
	leakCheck(t)
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	if _, err := fake.Mention("C1", "U1", mentionText); !errors.Is(err, fakeslack.ErrNoConnection) {
		t.Fatalf("Mention with no client = %v; want ErrNoConnection", err)
	}
	if n := len(fake.Messages("C1")); n != 0 {
		t.Fatalf("a Mention nobody could receive stored %d messages", n)
	}
}
