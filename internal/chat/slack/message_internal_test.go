package slack

import (
	"testing"

	"github.com/slack-go/slack/slackevents"
)

// A message event is Mentioned when its text holds courier's user token, in either form Slack writes it, <@UID> or
// <@UID|label>, wherever in the text it is: Slack sends an app_mention for a token in a quote or in code too, so
// the message event must agree with it. Nothing else is a mention.
func TestDecodeMessageMentionedIsCouriersUserTokenAnywhereInTheText(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"the raw token", "<@UME> why?", true},
		{"the token after text", "why? <@UME>", true},
		{"the token alone", "<@UME>", true},
		{"the labelled token", "<@UME|courier> why?", true},
		{"a labelled token with an empty label", "<@UME|> why?", true},
		{"a label with a pipe in it", "<@UME|a|b> why?", true},
		{"a label ends at its first >", "<@UME|a>b> why?", true},
		{"a label with a space and a symbol", "<@UME|Agent Courier (bot)!> why?", true},
		{"a token among others", "<@U1> <@UME> <@U2|bob>", true},
		{"a token after another user's labelled one", "<@U1|ME> <@UME>", true},
		{"a token on a later line", "line one\nline two <@UME>", true},
		{"a token after an unfinished one", "<@U1 <@UME> why?", true},
		{"a token after an unfinished labelled one", "<@U1|x <@UME> why?", true},
		{"a token in a quote", "> <@UME> said so\nwhy?", true},
		{"a token in inline code", "try `<@UME>` again", true},
		{"a token in a fence", "```\n<@UME>\n```", true},
		{"the labelled token in code", "`<@UME|courier>`", true},

		{"no text of interest", "hello", false},
		{"plain @courier", "@courier why?", false},
		{"plain @courier in code", "`@courier`", false},
		{"another user's token", "<@U1> why?", false},
		{"another user's labelled token", "<@U1|courier> why?", false},
		{"a user ID that begins with courier's", "<@UME2> why?", false},
		{"a labelled user ID that begins with courier's", "<@UME2|courier> why?", false},
		{"a user ID that ends with courier's", "<@XUME> why?", false},
		{"courier's ID as another user's label", "<@U1|UME> why?", false},
		{"courier's ID in a channel link", "<#UME|general>", false},
		{"courier's ID in a user group", "<!subteam^UME|@team>", false},
		{"@here", "<!here> why?", false},
		{"@channel", "<!channel> why?", false},
		{"@everyone", "<!everyone> why?", false},
		{"a user group", "<!subteam^S123|@oncall> why?", false},
		{"a token with no closing bracket", "<@UME why?", false},
		{"a labelled token with no closing bracket", "<@UME|courier why?", false},
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

// With no user ID to look for, nothing is a mention: an empty ID must not match an empty token (the pattern needs an ID).
func TestDecodeMessageWithoutABotUserIDIsNeverMentioned(t *testing.T) {
	for _, text := range []string{"<@> hi", "<@|x> hi", "<@UME> hi"} {
		m, v := decode(self{}, &slackevents.MessageEvent{Type: "message", User: "U1", Channel: "C1", TimeStamp: "1.1", Text: text})
		if v != forward || m.Mentioned {
			t.Errorf("%q: got verdict %v Mentioned %v, want forward and not mentioned", text, v, m.Mentioned)
		}
	}
}

// A bot's message with courier's token in it starts nothing: it is forwarded for a trigger to read, unmentioned.
func TestDecodeABotsMessageWithTheTokenIsNotMentioned(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	for _, ev := range []*slackevents.MessageEvent{
		{Type: "message", BotID: "B1", SubType: "bot_message", Channel: "C1", TimeStamp: "1.1", Text: "<@UME> look"},
		{Type: "message", BotID: "B1", User: "UOTHERBOT", Channel: "C1", TimeStamp: "1.2", Text: "<@UME|courier> look"},
	} {
		m, v := decode(me, ev)
		if v != forward || m.Mentioned || m.Bot.ID != "B1" {
			t.Errorf("%q: got verdict %v Mentioned %v bot %q, want forward, unmentioned, from B1", ev.Text, v, m.Mentioned, m.Bot.ID)
		}
	}
}

// An edit never makes a mention: an edited app_mention and a message_changed are dropped, as they were.
func TestDecodeAnEditIsDroppedWhateverItHolds(t *testing.T) {
	me := self{user: "UME", bot: "BME"}
	for name, ev := range map[string]any{
		"an edited app_mention":  &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "1.1", Text: "<@UME> hi", Edited: &slackevents.Edited{User: "U1", TimeStamp: "1.2"}},
		"a message_changed":      &slackevents.MessageEvent{Type: "message", SubType: "message_changed", User: "U1", Channel: "C1", TimeStamp: "1.3", Text: "<@UME> hi"},
		"a message_changed w/ |": &slackevents.MessageEvent{Type: "message", SubType: "message_changed", User: "U1", Channel: "C1", TimeStamp: "1.4", Text: "<@UME|courier> hi"},
	} {
		if _, v := decode(me, ev); v != dropped {
			t.Errorf("%s: verdict %v, want dropped", name, v)
		}
	}
}

// An app_mention is a mention whatever its text holds: Slack decided.
func TestDecodeAnAppMentionIsAlwaysMentioned(t *testing.T) {
	m, v := decode(self{user: "UME"}, &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "1.1", Text: "`<@UME|courier>` in code"})
	if v != forward || !m.Mentioned {
		t.Errorf("got verdict %v Mentioned %v, want forward and mentioned", v, m.Mentioned)
	}
}
