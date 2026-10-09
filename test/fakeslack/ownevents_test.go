package fakeslack_test

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

func botClient(fake *fakeslack.Server) *slack.Client {
	return slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/"))
}

func TestBotPostsDeliverMessageEvents(t *testing.T) {
	fake, smc := newSocket(t)
	waitEvent(t, smc, socketmode.EventTypeHello)
	api := botClient(fake)
	root := post(t, api, text("root"))
	section := slack.NewSectionBlock(mrkdwn("an answer"), nil, nil)
	reply := post(t, api, slack.MsgOptionTS(root), text("answer"), slack.MsgOptionBlocks(section))

	env1, _, first := nextInner(t, smc)
	m1, ok := first.(*slackevents.MessageEvent)
	if !ok || m1.User != "UFAKEBOT" || m1.BotID != "BFAKEBOT" || m1.SubType != "" || m1.Text != "root" ||
		m1.TimeStamp != root || m1.Channel != "C1" || m1.ChannelType != "channel" || m1.ThreadTimeStamp != "" {
		t.Fatalf("first event = %#v; want the bot's root post as a plain message", first)
	}
	env2, _, second := nextInner(t, smc)
	m2, ok := second.(*slackevents.MessageEvent)
	if !ok || m2.TimeStamp != reply || m2.ThreadTimeStamp != root || len(m2.Blocks.BlockSet) != 1 || m2.BotID != "BFAKEBOT" {
		t.Fatalf("second event = %#v; want the threaded reply with its blocks", second)
	}
	waitAcked(t, fake, env1, env2)

	if _, _, err := api.PostMessage("D1", text("in a DM")); err != nil {
		t.Fatalf("post in a DM: %v", err)
	}
	if _, _, third := nextInner(t, smc); third.(*slackevents.MessageEvent).ChannelType != "im" {
		t.Fatalf("event for a post in a D channel = %#v; want channel_type im", third)
	}
}

// TestBotPostEventShapeOnTheWire reads the raw envelope: slack-go's message
// event has no app_id, and decodes an absent subtype like an empty one.
func TestBotPostEventShapeOnTheWire(t *testing.T) {
	leakCheck(t)
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	ws := dial(t, fake)
	if _, _, err := ws.ReadMessage(); err != nil { // hello
		t.Fatalf("read hello: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second)) // a missing event fails fast
	meta := slack.SlackMetadata{EventType: "agentkourier_message", EventPayload: map[string]any{"outbox_id": "ob-1"}}
	api := botClient(fake)
	ts := post(t, api, text("hi"), slack.MsgOptionMetadata(meta))
	_, raw, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	var env struct {
		Type    string
		Payload struct {
			APIAppID string         `json:"api_app_id"`
			Event    map[string]any `json:"event"`
		}
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	ev := env.Payload.Event
	_, hasSubtype := ev["subtype"]
	_, hasMetadata := ev["metadata"]
	if hasMetadata || env.Type != "events_api" || env.Payload.APIAppID != "AFAKEAPP" || hasSubtype || ev["type"] != "message" || ev["user"] != "UFAKEBOT" ||
		ev["bot_id"] != "BFAKEBOT" || ev["app_id"] != "AFAKEAPP" || ev["ts"] != ts || ev["event_ts"] != ts || ev["channel"] != "C1" || ev["text"] != "hi" {
		t.Fatalf("event = %v; want a message from the bot with bot_id and app_id and no subtype", ev)
	}

	// A change is hidden, as Slack marks message_changed events.
	if _, _, _, err := api.UpdateMessage("C1", ts, text("edited")); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	_, raw, err = ws.ReadMessage()
	if err != nil {
		t.Fatalf("read change: %v", err)
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if ev := env.Payload.Event; ev["subtype"] != "message_changed" || ev["hidden"] != true {
		t.Fatalf("change event = %v; want a hidden message_changed", ev)
	}
}

func TestChatUpdateDeliversMessageChanged(t *testing.T) {
	fake, smc := newSocket(t)
	waitEvent(t, smc, socketmode.EventTypeHello)
	api := botClient(fake)
	ts := post(t, api, text("before"))
	nextInner(t, smc) // the post
	meta := slack.SlackMetadata{EventType: "agentkourier_message", EventPayload: map[string]any{"outbox_id": "ob-2"}}
	if _, _, _, err := api.UpdateMessage("C1", ts, text("after"), slack.MsgOptionMetadata(meta)); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	env, _, inner := nextInner(t, smc)
	m, ok := inner.(*slackevents.MessageEvent)
	if !ok || m.SubType != "message_changed" || m.Channel != "C1" || m.Message == nil || m.PreviousMessage == nil ||
		m.Message.Text != "after" || m.PreviousMessage.Text != "before" || m.Message.Timestamp != ts || m.PreviousMessage.Timestamp != ts ||
		m.Message.User != "UFAKEBOT" || m.Message.BotID != "BFAKEBOT" || m.TimeStamp == ts || m.TimeStamp == "" || m.Message.Metadata.EventType != "" {
		t.Fatalf("event = %#v; want message_changed from before to after on %s", inner, ts)
	}
	waitAcked(t, fake, env)
}

func TestStreamDeliversAMessageThenMessageChanged(t *testing.T) {
	fake, smc := newSocket(t)
	waitEvent(t, smc, socketmode.EventTypeHello)
	api := botClient(fake)
	root := post(t, api, text("question"))
	ts, err := startIn(api, "C1", root, append(recipients(), slack.MsgOptionMarkdownText("Hel"))...)
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	if _, _, err := api.AppendStream("C1", ts, slack.MsgOptionMarkdownText("lo ")); err != nil {
		t.Fatalf("AppendStream: %v", err)
	}
	if _, _, err := api.StopStream("C1", ts, slack.MsgOptionMarkdownText("wor")); err != nil {
		t.Fatalf("StopStream: %v", err)
	}

	nextInner(t, smc) // the root
	_, _, started := nextInner(t, smc)
	if m, ok := started.(*slackevents.MessageEvent); !ok || m.TimeStamp != ts || m.ThreadTimeStamp != root || m.Text != "Hel" || m.SubType != "" {
		t.Fatalf("start event = %#v; want the stream's first message in the thread", started)
	}
	// The append is silent, so the next event is the stop.
	_, _, stopped := nextInner(t, smc)
	if m, ok := stopped.(*slackevents.MessageEvent); !ok || m.SubType != "message_changed" || m.Message.Text != "Hello wor" ||
		m.PreviousMessage.Text != "Hello " || m.Message.Timestamp != ts {
		t.Fatalf("stop event = %#v; want message_changed from %q to %q", stopped, "Hello ", "Hello wor")
	}
}

func TestOwnPostEventsCanBeTurnedOffAndSkipFailures(t *testing.T) {
	tests := []struct {
		name string
		opts []fakeslack.Option
		act  func(t *testing.T, api *slack.Client)
	}{
		{"turned off", []fakeslack.Option{fakeslack.WithoutOwnPostEvents()}, func(t *testing.T, api *slack.Client) { post(t, api, text("quiet")) }},
		{"refused post", nil, func(t *testing.T, api *slack.Client) {
			if _, _, err := api.PostMessage("C1"); err == nil {
				t.Error("a post with no text or blocks succeeded")
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, smc := newSocket(t, tc.opts...)
			waitEvent(t, smc, socketmode.EventTypeHello)
			tc.act(t, botClient(fake))
			if _, err := fake.DM("U1", "hi"); err != nil {
				t.Fatalf("DM: %v", err)
			}
			if _, _, first := nextInner(t, smc); first.(*slackevents.MessageEvent).User != "U1" {
				t.Fatalf("first event = %#v; want the user's DM, with no event for the bot's post", first)
			}
		})
	}
}

func TestBotPostsWithNoClientAreUnaffected(t *testing.T) {
	leakCheck(t)
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	post(t, botClient(fake), text("nobody is listening"))
	if n := len(fake.Messages("C1")); n != 1 {
		t.Fatalf("Messages = %d; want the post stored", n)
	}
}

func TestOwnEventEnvelopesAreRecordedForAcks(t *testing.T) {
	fake, smc := newSocket(t)
	waitEvent(t, smc, socketmode.EventTypeHello)
	api := botClient(fake)
	ts := post(t, api, text("before"))
	if _, _, _, err := api.UpdateMessage("C1", ts, text("after")); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}

	ids := fake.OwnEventEnvelopes("C1", ts)
	if len(ids) != 2 {
		t.Fatalf("OwnEventEnvelopes = %v; want the post's and the update's", ids)
	}
	for i, want := range ids { // in delivery order, and the ones the client saw
		if env, _, _ := nextInner(t, smc); env != want {
			t.Fatalf("event %d has envelope %q; want %q", i, env, want)
		}
	}
	waitAcked(t, fake, ids...) // what a no-loop test waits on
	if n := len(fake.OwnEventEnvelopes("C1", "1.000001")) + len(fake.OwnEventEnvelopes("C2", ts)); n != 0 {
		t.Fatalf("envelopes recorded for a message that was not posted: %d", n)
	}
}

func TestOwnEventEnvelopesAreNotRecordedWithoutEvents(t *testing.T) {
	leakCheck(t)
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	ts := post(t, botClient(fake), text("nobody is listening"))
	if ids := fake.OwnEventEnvelopes("C1", ts); len(ids) != 0 {
		t.Fatalf("OwnEventEnvelopes with no client = %v; want none", ids)
	}
}

// TestOwnEventsBelongToTheirRequest posts and updates from many goroutines at
// once. Each call must have sent exactly its own event by the time it returns,
// and a post's event must come before its update's. A queue shared between
// requests lets one request send another's event after that request has
// answered, so this fails on it (see the mutation in the WP-11f fix report).
func TestOwnEventsBelongToTheirRequest(t *testing.T) {
	leakCheck(t)
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	ws := dial(t, fake)
	if _, _, err := ws.ReadMessage(); err != nil { // hello
		t.Fatalf("read hello: %v", err)
	}
	go func() { // drain, so that writes never block
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	const posters, rounds = 16, 12
	errs := make(chan string, posters*rounds*2)
	var wg sync.WaitGroup
	for range posters {
		wg.Go(func() {
			api := botClient(fake)
			for range rounds {
				_, ts, err := api.PostMessage("C1", text("p"))
				if err != nil {
					errs <- "post: " + err.Error()
					return
				}
				if n := len(fake.OwnEventEnvelopes("C1", ts)); n != 1 {
					errs <- fmt.Sprintf("%d events recorded when the post returned; want 1", n)
				}
				if _, _, _, err := api.UpdateMessage("C1", ts, text("u")); err != nil {
					errs <- "update: " + err.Error()
					return
				}
				ids := fake.OwnEventEnvelopes("C1", ts)
				if len(ids) != 2 || ids[0] >= ids[1] { // envelope IDs are zero-padded counters
					errs <- fmt.Sprintf("events %v when the update returned; want the post's, then the update's", ids)
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
