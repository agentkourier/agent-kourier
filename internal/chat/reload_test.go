package chat_test

import (
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// A config reload swaps the routing table under a running Intake. A bot a reloaded trigger newly names is accepted
// and matched, a bot it no longer names is refused and matches nothing, and a trigger whose regexes changed is
// compiled again, not kept from the table before.
func TestAConfigReloadChangesWhichBotsAreAcceptedAndMatched(t *testing.T) {
	const (
		oldBot = webhookBot
		newBot = "B0NEWBOT001"
	)
	forBot := func(bot, titleRegex string) v1alpha1.Trigger {
		return botTrigger(bot, func(tr *v1alpha1.Trigger) {
			tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: titleRegex}
		})
	}
	post := func(bot, id string) chat.Inbound {
		return botInbound("C1", id, chat.BotMessage{BotID: bot, Title: "FIRING (1) - Disk"})
	}
	h := newTriggered(t, forBot(oldBot, `^FIRING`))
	matched := func() int { return len(h.got()) }
	say := func(m chat.Inbound) {
		t.Helper()
		if err := h.in.Message(h.ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	if !h.in.AcceptsBot("C1", oldBot) || h.in.AcceptsBot("C1", newBot) {
		t.Fatal("before the reload: want the old bot accepted and the new one not")
	}
	say(post(oldBot, "1.1"))
	say(post(newBot, "1.2"))
	if matched() != 1 {
		t.Fatalf("before the reload: %d matches; want only the old bot's", matched())
	}

	h.reload(t, forBot(newBot, `^FIRING`))
	if h.in.AcceptsBot("C1", oldBot) || !h.in.AcceptsBot("C1", newBot) {
		t.Error("after the reload: want the new bot accepted and the old one refused")
	}
	say(post(newBot, "2.1"))
	say(post(oldBot, "2.2"))
	got := h.got()
	if len(got) != 2 || got[1].Message.Bot.BotID != newBot || got[1].Message.At.ID != "2.1" {
		t.Fatalf("after the reload: matches %+v; want the new bot's message and not the old bot's", got)
	}
	if n := h.counter(t, "courier_trigger_unmatched_total", "sre/sre-alerts"); n != 2 {
		t.Errorf("unmatched = %v; want the new bot's before the reload and the old bot's after it", n)
	}

	// The same bot with other regexes: the compiled table of the old snapshot is not reused.
	h.reload(t, forBot(newBot, `^RESOLVED`))
	say(post(newBot, "3.1"))
	if matched() != 2 {
		t.Fatalf("after changing the regex: %d matches; want the FIRING message no longer matched", matched())
	}
	h.reload(t, forBot(newBot, `^FIRING`))
	say(post(newBot, "3.2"))
	if matched() != 3 {
		t.Fatalf("after changing it back: %d matches; want the message matched again", matched())
	}
}
