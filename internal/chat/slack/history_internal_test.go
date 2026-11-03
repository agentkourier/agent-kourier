package slack

import (
	"encoding/json"
	"testing"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
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
				Bot: &chat.BotMessage{ID: "B1", Kind: v1alpha1.SenderKindBot, Title: "FIRING (1) - A", Link: "http://am", Fallback: "[FIRING:1] A | http://am", Footer: "f", Body: "t"}}},
		{"a reply in a thread", slackgo.Message{Msg: slackgo.Msg{User: "U1", Text: "and now?", Timestamp: "1.4", ThreadTimestamp: "1.1"}}, false, false,
			chat.Inbound{At: chat.Ref{Platform: "slack", Conn: "ns/c", Channel: "C1", Thread: "1.1", ID: "1.4"}, User: "U1", Text: "and now?"}},
		{"agent-kourier's own post, by bot ID", slackgo.Message{Msg: slackgo.Msg{BotID: "BME", SubType: "bot_message", Timestamp: "1.3", Attachments: attach[:1]}}, true, false,
			chat.Inbound{At: chat.Ref{Platform: "slack", Conn: "ns/c", Channel: "C1", ID: "1.3"},
				Bot: &chat.BotMessage{ID: "BME", Kind: v1alpha1.SenderKindBot, Title: "FIRING (1) - A", Link: "http://am", Fallback: "[FIRING:1] A | http://am", Footer: "f", Body: "t"}}},
		{"agent-kourier's own post, by user ID", slackgo.Message{Msg: slackgo.Msg{User: "UME", BotID: "BME", Text: "on it", Timestamp: "1.5"}}, true, false,
			chat.Inbound{At: chat.Ref{Platform: "slack", Conn: "ns/c", Channel: "C1", ID: "1.5"}, User: "UME", Text: "on it", Bot: &chat.BotMessage{ID: "BME", Kind: v1alpha1.SenderKindBot}}},
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

// A bot's post in a thread says what its blocks say when its attachment says nothing, and a post it has already said that in, in its
// text or its attachment, is as it was. What decodes a channel's history for a trigger reads no blocks.
func TestThreadMessageReadsTheBlocksOfABotsPost(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	section := slackgo.Blocks{BlockSet: []slackgo.Block{slackgo.NewSectionBlock(slackgo.NewTextBlockObject("mrkdwn", "p99 is high", false, false), nil, nil)}}
	post := func(text string, a []slackgo.Attachment) slackgo.Message {
		return slackgo.Message{Msg: slackgo.Msg{BotID: "B1", SubType: "bot_message", Timestamp: "1.2", Text: text, Attachments: a, Blocks: section}}
	}
	for _, tc := range []struct {
		name string
		msg  slackgo.Message
		want string
	}{
		{"blocks alone", post("", nil), "p99 is high"},
		{"blocks and a fallback", post("an alert fired", nil), "p99 is high"},
		{"blocks that repeat the text", post("p99 is high", nil), ""},
		{"an attachment that says something", post("", []slackgo.Attachment{{Text: "t"}}), "t"},
		{"an attachment that says nothing", post("", []slackgo.Attachment{{Title: "FIRING"}}), "p99 is high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := threadMessage(me, "ns/c", "C1", tc.msg)
			if got.Ignored || got.Message.Bot == nil || got.Message.Bot.Body != tc.want {
				t.Errorf("message = %+v (bot %+v); want a body of %q", got.Message, got.Message.Bot, tc.want)
			}
			if h := historyMessage(me, "ns/c", "C1", tc.msg); h.Message.Bot != nil && h.Message.Bot.Body == "p99 is high" {
				t.Errorf("the history read the blocks: %+v", h.Message.Bot)
			}
		})
	}
	person := slackgo.Message{Msg: slackgo.Msg{User: "U1", Text: "hi", Timestamp: "1.3", Blocks: section}}
	if got := threadMessage(me, "ns/c", "C1", person); got.Message.Text != "hi" || got.Message.Bot != nil {
		t.Errorf("a person's message = %+v; want its text", got.Message)
	}
}

// A bot's broadcast and its file share are posts of the thread, with no subtype to hide them, and Agent Kourier's own stays its own.
// Live, the same events are no bot's message: only reading a thread takes them.
func TestThreadMessageKeepsABotsBroadcastAndFileShare(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	for _, subtype := range []string{"thread_broadcast", "file_share"} {
		msg := slackgo.Message{Msg: slackgo.Msg{BotID: "B1", SubType: subtype, Timestamp: "1.2", ThreadTimestamp: "1.1", Text: "the findings"}}
		got := threadMessage(me, "ns/c", "C1", msg)
		if got.Ignored || got.Own || got.Message.Bot == nil || got.Message.Text != "the findings" {
			t.Errorf("%s = %+v; want a bot's post with its text", subtype, got)
		}
		if !historyMessage(me, "ns/c", "C1", msg).Ignored {
			t.Errorf("%s in the history is not ignored", subtype)
		}
		msg.BotID = "BME"
		if got := threadMessage(me, "ns/c", "C1", msg); !got.Own {
			t.Errorf("Agent Kourier's own %s = %+v; want Own", subtype, got)
		}
	}
	other := slackgo.Message{Msg: slackgo.Msg{BotID: "B1", SubType: "channel_join", Timestamp: "1.3", Text: "joined"}}
	if got := threadMessage(me, "ns/c", "C1", other); !got.Ignored {
		t.Errorf("a bot's channel_join = %+v; want it ignored", got)
	}
}

// Slack stores a markdown block as rich_text and keeps the post's text as the fallback it was: these are the shapes the sandbox
// returned. What the blocks say past the text is the body, and what only repeats it, in other markup, is not.
func TestABotsPostSaysWhatItsBlocksSayPastItsText(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	const report = `{"type":"rich_text","elements":[
		{"type":"rich_text_section","elements":[{"type":"text","text":"Deploy report","style":{"bold":true}}]},
		{"type":"rich_text_list","style":"bullet","elements":[
			{"type":"rich_text_section","elements":[{"type":"text","text":"failed on "},{"type":"text","text":"api","style":{"code":true}}]},
			{"type":"rich_text_section","elements":[{"type":"text","text":"see "},{"type":"link","url":"https://x.test/log","text":"the log"}]}]},
		{"type":"rich_text_quote","elements":[{"type":"text","text":"quoted"},{"type":"user","user_id":"U1"}]},
		{"type":"rich_text_preformatted","elements":[{"type":"text","text":"stack\n"}]}]}`
	const header = `{"type":"header","text":{"type":"plain_text","text":"Heading"}}`
	const plain = `{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"a & b "},{"type":"user","user_id":"U1"},{"type":"text","text":" see "},{"type":"link","url":"https://x.test","text":"this"}]}]}`
	for _, tc := range []struct{ name, text, blocks, want string }{
		{"a markdown block's detail", "Deploy report", report, "Deploy report\nfailed on api\nsee the log\nquoted\nstack"},
		{"a header", "Summary", header, "Heading"},
		{"a markdown block as it was sent", "Summary", `{"type":"markdown","text":"**detail**"}`, "**detail**"},
		{"Slack's rich_text of the text itself", "*a &amp; b* <@U1> see <https://x.test|this>", plain, ""},
		{"a block with no words", "Summary", `{"type":"divider"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var msg slackgo.Message
			raw := `{"type":"message","bot_id":"B1","ts":"1.2","text":` + quoted(tc.text) + `,"blocks":[` + tc.blocks + `]}`
			if err := json.Unmarshal([]byte(raw), &msg); err != nil {
				t.Fatal(err)
			}
			got := threadMessage(me, "ns/c", "C1", msg)
			if got.Message.Bot == nil || got.Message.Bot.Body != tc.want {
				t.Errorf("message = %+v (bot %+v); want a body of %q", got.Message, got.Message.Bot, tc.want)
			}
			var ev slackevents.AppMentionEvent
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				t.Fatal(err)
			}
			if b := mentionPost(&ev); b.Text != tc.want {
				t.Errorf("the post of a mention = %+v; want a text of %q", b, tc.want)
			}
		})
	}
}

func quoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
