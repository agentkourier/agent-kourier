package slack_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// mentionSink is a Sink that says which bots' mentions it takes, and no bot's plain message.
type mentionSink struct {
	*sink
	mu    sync.Mutex
	asked [][2]string
}

func (s *mentionSink) AcceptsBot(string, string) bool { return false }

func (s *mentionSink) AcceptsBotMention(channel, botID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, [2]string{channel, botID})
	return botID == webhookBot
}

func startMentions(t *testing.T) (*fakeslack.Server, *mentionSink) {
	t.Helper()
	fake := newFake(t)
	sk := &mentionSink{sink: &sink{}}
	run(t, slack.New(clientConfig(fake)), sk)
	waitConnected(t, fake)
	return fake, sk
}

// botMentionEvent is the app_mention a second app receives for render.MentionMessage, as the probe recorded it: the token is in a
// context block, and the text is the post's fallback.
func botMentionEvent(botID string) map[string]any {
	return message(map[string]any{
		"type": "app_mention", "ts": "2.2", "event_ts": "2.2", "thread_ts": "1.1", "user": "UBOT", "bot_id": botID, "app_id": "A1",
		"text":   "The agent named probe in this thread.",
		"blocks": []map[string]any{{"type": "context", "block_id": "b", "elements": []map[string]any{{"type": "mrkdwn", "text": botMark}, {"type": "plain_text", "text": "was named as probe in the reply above."}}}},
	})
}

// A mention from a bot the Sink takes reaches it, in its thread and Mentioned; one from any other bot does not; and the post's
// message event, which holds no token, is the bot's plain message, which this Sink does not take.
func TestABotsMentionReachesTheSinkOnlyIfItTakesTheBot(t *testing.T) {
	fake, sk := startMentions(t)
	waitAck(t, fake, send(t, fake, botMentionEvent("B0OTHERBOT1")))
	if msgs := sk.messages(); len(msgs) != 0 {
		t.Fatalf("a bot the sink does not take: the sink took %+v", msgs)
	}
	waitAck(t, fake, send(t, fake, botMentionEvent(webhookBot)))
	msgs := sk.messages()
	if len(msgs) != 1 {
		t.Fatalf("the sink took %+v; want the one mention", msgs)
	}
	if m := msgs[0]; !m.Mentioned || m.Bot.ID != webhookBot || m.TS != "2.2" || m.ThreadTS != "1.1" {
		t.Errorf("message = %+v; want it Mentioned, from the bot, in the thread", m)
	}
	ev := botMentionEvent(webhookBot)
	ev["type"] = "message"
	ev["event_ts"] = "2.3"
	waitAck(t, fake, send(t, fake, ev))
	if msgs := sk.messages(); len(msgs) != 1 {
		t.Errorf("the post's message event was taken too: %+v", msgs)
	}
}

// Agent Kourier's own mention is never given to the Sink, even a Sink that takes every bot.
func TestAgentKouriersOwnMentionNeverReachesTheSink(t *testing.T) {
	fake := newFake(t)
	sk := &mentionSink{sink: &sink{}}
	run(t, slack.New(clientConfig(fake)), sk)
	waitConnected(t, fake)
	ev := botMentionEvent(webhookBot)
	ev["user"] = "UFAKEBOT"
	waitAck(t, fake, send(t, fake, ev))
	if msgs := sk.messages(); len(msgs) != 0 {
		t.Errorf("the sink took %+v; want nothing", msgs)
	}
}

// A channel shared with another workspace fails closed: a bot of the other workspace may have written it.
func TestABotsMentionInASharedChannelIsDropped(t *testing.T) {
	fake, sk := startMentions(t)
	ev := botMentionEvent(webhookBot)
	id, err := fake.SendEnvelope("events_api", map[string]any{"type": "event_callback", "team_id": "TFAKE", "event_id": fmt.Sprint("Ev", ev["event_ts"]), "is_ext_shared_channel": true, "event": ev})
	if err != nil {
		t.Fatal(err)
	}
	waitAck(t, fake, id)
	if msgs := sk.messages(); len(msgs) != 0 {
		t.Errorf("the sink took %+v; want nothing", msgs)
	}
}
