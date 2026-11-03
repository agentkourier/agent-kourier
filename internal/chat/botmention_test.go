package chat_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/store"
)

const mentioningBot = "B0MENTIONER"

// botMentionOf is a bot's mention, its text the fixed fallback of the message that mentions, in the thread if thread is set.
func botMentionOf(id, thread string) chat.Inbound {
	m := inbound("C1", id, thread, true)
	m.User, m.Text = "UBOT", "The agent named probe in this thread."
	m.Bot = &chat.BotMessage{ID: mentioningBot, Kind: v1alpha1.SenderKindBot}
	return m
}

// A Binding with a mention trigger takes any bot's mention unless it says otherwise. One that names a bot in a botMessage trigger
// leaves that bot to the trigger, and one that lists triggers without the mention one, or says none, takes none.
func TestWhichBindingsTakeABotsMention(t *testing.T) {
	mention := v1alpha1.Trigger{Type: v1alpha1.TriggerMention}
	named := kindOnly()
	namedBot := named.From.ID
	tests := []struct {
		name     string
		triggers []v1alpha1.Trigger
		none     bool
		channel  string
		bot      string
		want     bool
	}{
		{"no triggers: the default mention trigger", nil, false, "C1", mentioningBot, true},
		{"an explicit mention trigger", []v1alpha1.Trigger{mention}, false, "C1", mentioningBot, true},
		{"a mention trigger beside a botMessage trigger of another bot", []v1alpha1.Trigger{mention, named}, false, "C1", mentioningBot, true},
		{"the bot a botMessage trigger names", []v1alpha1.Trigger{mention, named}, false, "C1", namedBot, false},
		{"no mention trigger", []v1alpha1.Trigger{named}, false, "C1", mentioningBot, false},
		{"chat.botMentions: none", []v1alpha1.Trigger{mention}, true, "C1", mentioningBot, false},
		{"a channel with no Binding", nil, false, "C0NOBINDING", mentioningBot, false},
		{"no bot ID", nil, false, "C1", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTriggered(t, tt.triggers...)
			if tt.none {
				h.setBotMentions(t, v1alpha1.BotMentionsNone, tt.triggers)
			}
			if got := h.in.AcceptsBotMention(tt.channel, tt.bot); got != tt.want {
				t.Errorf("AcceptsBotMention = %v; want %v", got, tt.want)
			}
		})
	}
}

// A mention starts one turn, in the thread that holds it, with the fixed prompt and marked as a mention. Its redelivery, and
// its other event, start nothing more.
func TestABotsMentionStartsOneTurnInItsThread(t *testing.T) {
	h := newLimited(t)
	m := botMentionOf("5.5", "4.4")
	for range 2 {
		if err := h.in.Message(h.ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	got := h.got()
	if len(got) != 1 {
		t.Fatalf("OnStart was asked %d times; want once", len(got))
	}
	if !got[0].Mention || got[0].PromptTemplate != chat.BotMentionPrompt || got[0].Message.At.Thread != "4.4" || got[0].Message.At.ID != "5.5" {
		t.Errorf("match = %+v; want a mention, with the fixed prompt, of 5.5 in the thread 4.4", got[0])
	}
	if subs := h.sub.submitted(); len(subs) != 0 {
		t.Errorf("submitted %+v; a bot's mention goes through OnStart and not the person's path", subs)
	}
}

// A mention from a bot the Binding does not take starts nothing, whatever the adapter let through.
func TestTheIntakeAsksAgainWhetherItTakesTheMention(t *testing.T) {
	h := newLimited(t, v1alpha1.Trigger{Type: v1alpha1.TriggerMention})
	h.setBotMentions(t, v1alpha1.BotMentionsNone, []v1alpha1.Trigger{{Type: v1alpha1.TriggerMention}})
	if err := h.in.Message(h.ctx, botMentionOf("5.5", "4.4")); err != nil {
		t.Fatal(err)
	}
	if got := h.got(); len(got) != 0 {
		t.Errorf("OnStart was asked %+v; want nothing", got)
	}
}

// A burst in one thread is held to the fixed limits, whether or not the mentions share a thread, because the limits count
// messages: two bots trading mentions in one thread cannot pass them. Every stopped mention is told to the audit hook.
func TestABurstOfMentionsIsHeldToTheLimits(t *testing.T) {
	h := newLimited(t)
	var stopped []store.TriggerLimit
	h.cfg.OnMentionLimited = func(_ context.Context, _ chat.TriggerMatch, l store.TriggerLimit) { stopped = append(stopped, l) }
	h.in = chat.NewIntake(h.cfg)
	for i := range 6 {
		if err := h.in.Message(h.ctx, botMentionOf(fmt.Sprintf("5.%d", i+1), "4.4")); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.got(); len(got) != 3 {
		t.Errorf("OnStart was asked %d times; want the 3 of the rate limit", len(got))
	}
	if len(stopped) != 3 || stopped[0] != store.LimitRate {
		t.Errorf("stopped = %v; want 3 of the rate limit", stopped)
	}
	h.clk.advance(31 * time.Minute)
	if err := h.in.Message(h.ctx, botMentionOf("6.1", "4.4")); err != nil {
		t.Fatal(err)
	}
	if got := h.got(); len(got) != 4 {
		t.Errorf("after the window OnStart was asked %d times; want a fourth", len(got))
	}
}
