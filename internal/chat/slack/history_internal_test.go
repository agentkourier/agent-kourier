package slack

import (
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// A message of the history is decoded by the code that decodes a live event, so the same messages are dropped, the same
// ones are Agent Kourier's own, and a bot's attachment is flattened the same way.
func TestHistoryMessageDecodesAsALiveEventDoes(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	attach := []slackgo.Attachment{{Title: "FIRING (1) - A", TitleLink: "http://am", Fallback: "[FIRING:1] A | http://am", Text: "t", Footer: "f"}, {Title: "second"}}
	for _, tc := range []struct {
		name        string
		msg         slackgo.Message
		own, ignore bool
		want        chat.Inbound
	}{
		{"a person's message", slackgo.Message{Msg: slackgo.Msg{User: "U1", Text: "hi <@UME>", Timestamp: "1.1"}}, false, false,
			chat.Inbound{At: chat.Ref{Platform: "slack", Conn: "ns/c", Channel: "C1", ID: "1.1"}, User: "U1", Text: "hi <@UME>", Mentioned: true}},
		{"a webhook's alert, from its first attachment", slackgo.Message{Msg: slackgo.Msg{BotID: "B1", SubType: "bot_message", Timestamp: "1.2", Attachments: attach}}, false, false,
			chat.Inbound{At: chat.Ref{Platform: "slack", Conn: "ns/c", Channel: "C1", ID: "1.2"},
				Bot: &chat.BotMessage{BotID: "B1", Title: "FIRING (1) - A", TitleLink: "http://am", Fallback: "[FIRING:1] A | http://am", Footer: "f", AttachmentText: "t"}}},
		{"a reply in a thread", slackgo.Message{Msg: slackgo.Msg{User: "U1", Text: "and now?", Timestamp: "1.4", ThreadTimestamp: "1.1"}}, false, false,
			chat.Inbound{At: chat.Ref{Platform: "slack", Conn: "ns/c", Channel: "C1", Thread: "1.1", ID: "1.4"}, User: "U1", Text: "and now?"}},
		{"agent-kourier's own post, by bot ID", slackgo.Message{Msg: slackgo.Msg{BotID: "BME", SubType: "bot_message", Timestamp: "1.3", Attachments: attach[:1]}}, true, false,
			chat.Inbound{At: chat.Ref{Platform: "slack", Conn: "ns/c", Channel: "C1", ID: "1.3"},
				Bot: &chat.BotMessage{BotID: "BME", Title: "FIRING (1) - A", TitleLink: "http://am", Fallback: "[FIRING:1] A | http://am", Footer: "f", AttachmentText: "t"}}},
		{"agent-kourier's own post, by user ID", slackgo.Message{Msg: slackgo.Msg{User: "UME", BotID: "BME", Text: "on it", Timestamp: "1.5"}}, true, false,
			chat.Inbound{At: chat.Ref{Platform: "slack", Conn: "ns/c", Channel: "C1", ID: "1.5"}, User: "UME", Text: "on it", Bot: &chat.BotMessage{BotID: "BME"}}},
		{"a subtype that starts nothing", slackgo.Message{Msg: slackgo.Msg{User: "U1", SubType: "channel_join", Text: "joined", Timestamp: "1.6"}}, false, true, chat.Inbound{}},
		{"a bot's message with no content", slackgo.Message{Msg: slackgo.Msg{BotID: "B1", SubType: "bot_message", Timestamp: "1.7"}}, false, true, chat.Inbound{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := historyMessage(me, "ns/c", "C1", tc.msg)
			if got.Own != tc.own || got.Ignored != tc.ignore {
				t.Fatalf("own = %t, ignored = %t; want %t, %t", got.Own, got.Ignored, tc.own, tc.ignore)
			}
			if tc.ignore {
				if want := (chat.Ref{Platform: "slack", Conn: "ns/c", Channel: "C1", ID: tc.msg.Timestamp}); got.Message != (chat.Inbound{At: want}) {
					t.Errorf("an ignored message = %+v; want only where it was, %+v", got.Message, want)
				}
			} else if !inboundEqual(got.Message, tc.want) {
				t.Errorf("message = %+v (bot %+v);\nwant %+v (bot %+v)", got.Message, got.Message.Bot, tc.want, tc.want.Bot)
			}
		})
	}
}

// inboundEqual compares two Inbounds, following their Bot pointers.
func inboundEqual(a, b chat.Inbound) bool {
	ab, bb := a.Bot, b.Bot
	a.Bot, b.Bot = nil, nil
	if a != b || (ab == nil) != (bb == nil) {
		return false
	}
	return ab == nil || *ab == *bb
}
