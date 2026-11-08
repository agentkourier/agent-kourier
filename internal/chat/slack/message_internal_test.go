package slack

import (
	"encoding/json"
	"testing"

	"github.com/slack-go/slack/slackevents"
)

// A message event is Mentioned when its text holds Agent Kourier's user token, in either form Slack writes it, <@UID> or
// <@UID|label>, wherever in the text it is: Slack sends an app_mention for a token in a quote or in code too, so
// the message event must agree with it. Nothing else is a mention.
func TestDecodeMessageMentionedIsAgentKouriersUserTokenAnywhereInTheText(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"the raw token", "<@UME> why?", true},
		{"the token after text", "why? <@UME>", true},
		{"the token alone", "<@UME>", true},
		{"the labelled token", "<@UME|agent_kourier> why?", true},
		{"a labelled token with an empty label", "<@UME|> why?", true},
		{"a label with a pipe in it", "<@UME|a|b> why?", true},
		{"a label ends at its first >", "<@UME|a>b> why?", true},
		{"a label with a space and a symbol", "<@UME|Agent Kourier (bot)!> why?", true},
		{"a token among others", "<@U1> <@UME> <@U2|bob>", true},
		{"a token after another user's labelled one", "<@U1|ME> <@UME>", true},
		{"a token on a later line", "line one\nline two <@UME>", true},
		{"a token after an unfinished one", "<@U1 <@UME> why?", true},
		{"a token after an unfinished labelled one", "<@U1|x <@UME> why?", true},
		{"a token in a quote", "> <@UME> said so\nwhy?", true},
		{"a token in inline code", "try `<@UME>` again", true},
		{"a token in a fence", "```\n<@UME>\n```", true},
		{"the labelled token in code", "`<@UME|agent_kourier>`", true},

		{"no text of interest", "hello", false},
		{"plain @agent_kourier", "@agent_kourier why?", false},
		{"plain @agent_kourier in code", "`@agent_kourier`", false},
		{"another user's token", "<@U1> why?", false},
		{"another user's labelled token", "<@U1|agent_kourier> why?", false},
		{"a user ID that begins with Agent Kourier's", "<@UME2> why?", false},
		{"a labelled user ID that begins with Agent Kourier's", "<@UME2|agent_kourier> why?", false},
		{"a user ID that ends with Agent Kourier's", "<@XUME> why?", false},
		{"agent-kourier's ID as another user's label", "<@U1|UME> why?", false},
		{"agent-kourier's ID in a channel link", "<#UME|general>", false},
		{"agent-kourier's ID in a user group", "<!subteam^UME|@team>", false},
		{"@here", "<!here> why?", false},
		{"@channel", "<!channel> why?", false},
		{"@everyone", "<!everyone> why?", false},
		{"a user group", "<!subteam^S123|@oncall> why?", false},
		{"a token with no closing bracket", "<@UME why?", false},
		{"a labelled token with no closing bracket", "<@UME|agent_kourier why?", false},
		{"a token with something else after the ID", "<@UME x> why?", false},
		{"no opening bracket", "@UME> why?", false},
		{"the ID without a token", "UME why?", false},
		{"the token's characters escaped", "&lt;@UME&gt; why?", false},
		{"an empty token", "<@> why?", false},
		{"an empty labelled token", "<@|x> why?", false},
	} {
		m, v := decode(me, &slackevents.MessageEvent{Type: "message", User: "U1", Channel: "C1", TimeStamp: "1.1", ChannelType: "channel", Text: tc.text})
		if v != forward {
			t.Errorf("%s: verdict %v, want forward", tc.name, v)
			continue
		}
		if m.Mentioned != tc.want {
			t.Errorf("%s: Mentioned = %v for %q, want %v", tc.name, m.Mentioned, tc.text, tc.want)
		}
		if m.Text != tc.text {
			t.Errorf("%s: Text = %q, want it verbatim, %q", tc.name, m.Text, tc.text)
		}
	}
}

// A person's message whose mention of Agent Kourier is only in its blocks, which Slack sends an app_mention for, is Mentioned in its
// message event too, so that whichever event comes first decides the same: a rich_text block's user element, and the token in a
// section or context block's text. Another user named there, or Agent Kourier's ID as mere text, is no mention. Decoded from the
// event's JSON, as Socket Mode delivers it.
func TestDecodeAPersonsMentionInItsBlocksIsAMention(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	for _, tt := range []struct {
		name, blocks string
		want         bool
	}{
		{"a rich_text user element", `[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"UME"},{"type":"text","text":" look"}]}]}]`, true},
		{"one in a list", `[{"type":"rich_text","elements":[{"type":"rich_text_list","style":"bullet","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"UME"}]}]}]}]`, true},
		{"a section block's token", `[{"type":"section","text":{"type":"mrkdwn","text":"<@UME> look"}}]`, true},
		{"a context block's token", `[{"type":"context","elements":[{"type":"mrkdwn","text":"<@UME|agent>"}]}]`, true},
		{"another user", `[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"U1"}]}]}]`, false},
		{"the ID as text", `[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"UME"}]}]}]`, false},
		{"no blocks", `[]`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var ev slackevents.MessageEvent
			if err := json.Unmarshal([]byte(`{"type":"message","user":"U1","channel":"C1","ts":"1.1","channel_type":"channel","text":"please look","blocks":`+tt.blocks+`}`), &ev); err != nil {
				t.Fatal(err)
			}
			m, v := decode(me, &ev)
			if v != forward || m.Mentioned != tt.want {
				t.Errorf("decode = Mentioned %v, verdict %v; want Mentioned %v and forwarded", m.Mentioned, v, tt.want)
			}
		})
	}
}

// Slackbot is no person: a post of its fixed user ID that comes with no bot ID is dropped, so that no personMessage trigger and no
// thread takes it as a person's, and so is its app_mention, so that it starts no turn as a person's mention. Its reminders carry a
// bot ID and are a bot's posts (TestDecodeTheShapesSlackSent); this holds for a Slackbot post without one, which no probe has seen.
func TestDecodeSlackbotIsNoPerson(t *testing.T) {
	for name, ev := range map[string]any{
		"a message":           &slackevents.MessageEvent{Type: "message", User: "USLACKBOT", Channel: "C1", TimeStamp: "1.1", ChannelType: "channel", Text: "Reminder: standup."},
		"a reply in a thread": &slackevents.MessageEvent{Type: "message", User: "USLACKBOT", Channel: "C1", TimeStamp: "1.2", ThreadTimeStamp: "1.0", ChannelType: "channel", Text: "Reminder: look at this."},
		"an app_mention":      &slackevents.AppMentionEvent{User: "USLACKBOT", Channel: "C1", TimeStamp: "1.3", Text: "Reminder: <@UME> check the deploy."},
	} {
		if _, v := decode(self{user: "UME", bot: "BME"}, ev); v != dropped {
			t.Errorf("%s: verdict %v; want dropped", name, v)
		}
	}
}

// With no user ID to look for, nothing is a mention: an empty ID must not match an empty token (the pattern needs an ID).
func TestDecodeMessageWithoutABotUserIDIsNeverMentioned(t *testing.T) {
	for _, text := range []string{"<@> hi", "<@|x> hi", "<@UME> hi"} {
		m, v := decode(self{}, &slackevents.MessageEvent{Type: "message", User: "U1", Channel: "C1", TimeStamp: "1.1", Text: text})
		if v != forward || m.Mentioned {
			t.Errorf("%q: got verdict %v Mentioned %v, want forward and not mentioned", text, v, m.Mentioned)
		}
	}
}

// A bot's message with Agent Kourier's token in it starts nothing: it is forwarded for a trigger to read, unmentioned.
func TestDecodeABotsMessageWithTheTokenIsNotMentioned(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	for _, ev := range []*slackevents.MessageEvent{
		{Type: "message", BotID: "B1", SubType: "bot_message", Channel: "C1", TimeStamp: "1.1", Text: "<@UME> look"},
		{Type: "message", BotID: "B1", User: "UOTHERBOT", Channel: "C1", TimeStamp: "1.2", Text: "<@UME|agent_kourier> look"},
	} {
		m, v := decode(me, ev)
		if v != forward || m.Mentioned || m.Bot.ID != "B1" {
			t.Errorf("%q: got verdict %v Mentioned %v bot %q, want forward, unmentioned, from B1", ev.Text, v, m.Mentioned, m.Bot.ID)
		}
	}
}

// An edit never makes a mention: an edited app_mention and a message_changed are dropped, as they were.
// What a bot's mention says may be outside its text: an attachment, or the section or context block that holds the token. These
// are decoded from the event's JSON, as Socket Mode delivers it.
func TestDecodeABotsAppMentionKeepsWhatThePostHolds(t *testing.T) {
	for _, tt := range []struct {
		name, event string
		want        BotPost
	}{
		{
			"an attachment",
			`{"bot_id":"B1","channel":"C1","ts":"2.2","text":"<@UME> alert","attachments":[{"title":"FIRING","title_link":"https://x.example/a","text":"p99 is high","fallback":"fb","footer":"am"}]}`,
			BotPost{ID: "B1", Title: "FIRING", TitleLink: "https://x.example/a", Text: "p99 is high", Fallback: "fb", Footer: "am"},
		},
		{
			"a section block",
			`{"bot_id":"B1","channel":"C1","ts":"2.2","text":"look","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"<@UME> check the card service"},"fields":[{"type":"mrkdwn","text":"sev 2"}]}]}`,
			BotPost{ID: "B1", Text: "<@UME> check the card service\nsev 2"},
		},
		{
			"a context block",
			`{"bot_id":"B1","channel":"C1","ts":"2.2","text":"look","blocks":[{"type":"context","elements":[{"type":"mrkdwn","text":"<@UME>"},{"type":"plain_text","text":"was named"},{"type":"image","image_url":"https://x.example/i.png","alt_text":"i"}]}]}`,
			BotPost{ID: "B1", Text: "<@UME>\nwas named"},
		},
		{
			"an attachment's text comes before the blocks'",
			`{"bot_id":"B1","channel":"C1","ts":"2.2","attachments":[{"text":"from the attachment"}],"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"from the block"}}]}`,
			BotPost{ID: "B1", Text: "from the attachment"},
		},
		{
			"a rich_text block, whose words are the top-level text",
			`{"bot_id":"B1","channel":"C1","ts":"2.2","text":"<@UME> hi","blocks":[{"type":"rich_text","elements":[]}]}`,
			BotPost{ID: "B1"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var ev slackevents.AppMentionEvent
			if err := json.Unmarshal([]byte(tt.event), &ev); err != nil {
				t.Fatal(err)
			}
			m, v := decode(self{user: "UME", bot: "BME"}, &ev)
			if v != forward || m.Bot != tt.want {
				t.Errorf("decode = %+v, %v; want Bot %+v", m.Bot, v, tt.want)
			}
		})
	}
}

func TestDecodeAnEditIsDroppedWhateverItHolds(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	for name, ev := range map[string]any{
		"an edited app_mention":  &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "1.1", Text: "<@UME> hi", Edited: &slackevents.Edited{User: "U1", TimeStamp: "1.2"}},
		"a message_changed":      &slackevents.MessageEvent{Type: "message", SubType: "message_changed", User: "U1", Channel: "C1", TimeStamp: "1.3", Text: "<@UME> hi"},
		"a message_changed w/ |": &slackevents.MessageEvent{Type: "message", SubType: "message_changed", User: "U1", Channel: "C1", TimeStamp: "1.4", Text: "<@UME|agent_kourier> hi"},
	} {
		if _, v := decode(me, ev); v != dropped {
			t.Errorf("%s: verdict %v, want dropped", name, v)
		}
	}
}

// An app_mention is a mention whatever its text holds: Slack decided.
func TestDecodeAnAppMentionIsAlwaysMentioned(t *testing.T) {
	m, v := decode(self{user: "UME"}, &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "1.1", Text: "`<@UME|agent_kourier>` in code"})
	if v != forward || !m.Mentioned {
		t.Errorf("got verdict %v Mentioned %v, want forward and mentioned", v, m.Mentioned)
	}
}

// A bot's mention is its app_mention, as Slack sent it to a second app in the probe: the post's text may hold no token, because
// the mention is in a context block, and a webhook's has no user (docs/spikes/slack-mention-probe.md).
func TestDecodeABotsAppMention(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	const fallback = "The agent named probe in this thread."
	tests := []struct {
		name string
		ev   *slackevents.AppMentionEvent
		want verdict
	}{
		{"an app's post, in a thread, the token only in its blocks", &slackevents.AppMentionEvent{BotID: "B1", User: "UBOT", Channel: "C1", TimeStamp: "2.2", ThreadTimeStamp: "1.1", Text: fallback}, forward},
		{"at the top level", &slackevents.AppMentionEvent{BotID: "B1", User: "UBOT", Channel: "C1", TimeStamp: "2.2", Text: fallback}, forward},
		{"a webhook's post, which has no user", &slackevents.AppMentionEvent{BotID: "B1", Channel: "C1", TimeStamp: "2.2", Text: "<@UME> hi"}, forward},
		{"a post with no text", &slackevents.AppMentionEvent{BotID: "B1", User: "UBOT", Channel: "C1", TimeStamp: "2.2"}, forward},
		{"Agent Kourier's own, by bot ID", &slackevents.AppMentionEvent{BotID: "BME", User: "UOTHER", Channel: "C1", TimeStamp: "2.2", Text: fallback}, ownPost},
		{"Agent Kourier's own, by user ID", &slackevents.AppMentionEvent{BotID: "B1", User: "UME", Channel: "C1", TimeStamp: "2.2", Text: fallback}, ownPost},
		{"an edit that adds the mention", &slackevents.AppMentionEvent{BotID: "B1", User: "UBOT", Channel: "C1", TimeStamp: "2.2", Text: fallback, Edited: &slackevents.Edited{User: "UBOT", TimeStamp: "3.3"}}, dropped},
		{"no channel", &slackevents.AppMentionEvent{BotID: "B1", User: "UBOT", TimeStamp: "2.2", Text: fallback}, dropped},
		{"no ts", &slackevents.AppMentionEvent{BotID: "B1", User: "UBOT", Channel: "C1", Text: fallback}, dropped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, v := decode(me, tt.ev)
			if v != tt.want {
				t.Fatalf("verdict = %v; want %v", v, tt.want)
			}
			if v == forward && (!m.Mentioned || m.Bot.ID != "B1" || m.TS != tt.ev.TimeStamp || m.ThreadTS != tt.ev.ThreadTimeStamp || m.Text != tt.ev.Text || m.User != tt.ev.User) {
				t.Errorf("message = %+v; want it Mentioned, from B1, as the event has it", m)
			}
		})
	}
}

// The events Slack sent in docs/spikes/slack-user-token-and-slackbot-probe.md, with the IDs replaced, decode as that page says. An
// app that posts with a person's user token sends its own bot ID, so its posts and mentions are a bot's, not the person's, and no
// personMessage trigger takes them. Slackbot's reminders carry the bot ID B01 (in the workspace probed), so they are a bot's posts,
// and a reminder that mentions Agent Kourier is a bot's mention. The /remind command's own post, a user-token me_message and a Slackbot custom response
// (which has no bot ID) are subtypes, and dropped.
func TestDecodeTheShapesSlackSent(t *testing.T) {
	const (
		probeProfile = `"bot_profile":{"id":"BPROBE","app_id":"APROBE","name":"Kourier User-Token Probe","team_id":"TSAND","deleted":false,"updated":1794161711}`
		mentionBlock = `"blocks":[{"type":"rich_text","block_id":"b","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"check "},{"type":"user","user_id":"UME"}]}]}]`
		plainBlock   = `"blocks":[{"type":"rich_text","block_id":"b","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"check"}]}]}]`
		reminderMeta = `"metadata":{"event_type":"slack_system.reminders.posted_in_channel","event_payload":{"reminder_id":"Rm1"}}`
	)
	for _, tt := range []struct {
		name, event string
		want        verdict
		bot, user   string
		mentioned   bool
	}{
		{"a user-token post", `{"type":"message","channel":"C1","channel_type":"channel","user":"UPERSON","bot_id":"BPROBE","app_id":"APROBE",` + probeProfile + `,"team":"TSAND","text":"probe U1",` + plainBlock + `,"ts":"1.1","event_ts":"1.1"}`,
			forward, "BPROBE", "UPERSON", false},
		{"a user-token reply in a thread", `{"type":"message","channel":"C1","channel_type":"channel","user":"UPERSON","bot_id":"BPROBE","app_id":"APROBE",` + probeProfile + `,"team":"TSAND","text":"probe U3",` + plainBlock + `,"thread_ts":"1.1","parent_user_id":"UPERSON","ts":"1.3","event_ts":"1.3"}`,
			forward, "BPROBE", "UPERSON", false},
		{"a user-token post that mentions Agent Kourier", `{"type":"message","channel":"C1","channel_type":"channel","user":"UPERSON","bot_id":"BPROBE","app_id":"APROBE",` + probeProfile + `,"team":"TSAND","text":"probe U2 <@UME>",` + mentionBlock + `,"ts":"1.2","event_ts":"1.2"}`,
			forward, "BPROBE", "UPERSON", false},
		{"the user-token post's app_mention", `{"type":"app_mention","channel":"C1","user":"UPERSON","bot_id":"BPROBE","app_id":"APROBE",` + probeProfile + `,"team":"TSAND","text":"probe U2 <@UME>",` + mentionBlock + `,"ts":"1.2","event_ts":"1.2"}`,
			forward, "BPROBE", "UPERSON", true},
		{"a user-token me_message", `{"type":"message","subtype":"me_message","channel":"C1","channel_type":"channel","user":"UPERSON","bot_id":"BPROBE","app_id":"APROBE","text":"probe U7",` + plainBlock + `,"ts":"1.7","event_ts":"1.7"}`,
			dropped, "", "", false},
		{"the /remind command's post", `{"type":"message","subtype":"reminder_add","channel":"C1","channel_type":"channel","user":"UPERSON","text":" set up a reminder “check @Agent Kourier ” in this channel at 1:18PM today.","ts":"1.8","event_ts":"1.8"}`,
			dropped, "", "", false},
		{"Slackbot's custom response", `{"type":"message","subtype":"slackbot_response","channel":"C1","channel_type":"channel","user":"USLACKBOT","text":"kourierprobe pong &lt;@UME&gt;","ts":"1.95","event_ts":"1.95"}`,
			dropped, "", "", false},
		{"Slackbot's reminder", `{"type":"message","channel":"C1","channel_type":"channel","user":"USLACKBOT","bot_id":"B01","team":"TSAND","text":"Reminder: check.",` + plainBlock + `,` + reminderMeta + `,"ts":"1.9","event_ts":"1.9"}`,
			forward, "B01", "USLACKBOT", false},
		{"Slackbot's reminder that mentions Agent Kourier", `{"type":"message","channel":"C1","channel_type":"channel","user":"USLACKBOT","bot_id":"B01","team":"TSAND","text":"Reminder: check <@UME|agent_kourier> ",` + mentionBlock + `,` + reminderMeta + `,"ts":"2.1","event_ts":"2.1"}`,
			forward, "B01", "USLACKBOT", false},
		{"the reminder's app_mention", `{"type":"app_mention","channel":"C1","user":"USLACKBOT","bot_id":"B01","team":"TSAND","text":"Reminder: check <@UME|agent_kourier> ",` + mentionBlock + `,` + reminderMeta + `,"ts":"2.1","event_ts":"2.1"}`,
			forward, "B01", "USLACKBOT", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			outer, err := slackevents.ParseEvent(json.RawMessage(`{"type":"event_callback","team_id":"TSAND","event":`+tt.event+`}`), slackevents.OptionNoVerifyToken())
			if err != nil {
				t.Fatal(err)
			}
			m, v := decode(self{user: "UME", bot: "BME"}, outer.InnerEvent.Data)
			if v != tt.want || m.Bot.ID != tt.bot || m.User != tt.user || m.Mentioned != tt.mentioned {
				t.Errorf("decode = verdict %v, bot %q, user %q, Mentioned %v; want %v, %q, %q, %v", v, m.Bot.ID, m.User, m.Mentioned, tt.want, tt.bot, tt.user, tt.mentioned)
			}
			if fromAnotherWorkspace(outer) {
				t.Error("an event of the workspace's own channel is from another workspace")
			}
		})
	}
}
