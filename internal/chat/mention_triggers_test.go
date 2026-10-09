package chat_test

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// Spec 8.2: an empty trigger list is [mention], and a list that names triggers is the whole list. A Binding that
// lists triggers and leaves out the mention one starts no session from an @mention, in or out of a thread, while a
// reply in a thread Agent Kourier owns still continues it: that is the table's other row, which no trigger gates.
func TestMentionsFollowTheBindingsTriggers(t *testing.T) {
	mention := v1alpha1.Trigger{Type: v1alpha1.TriggerMention}
	tests := []struct {
		name     string
		triggers []v1alpha1.Trigger
		m        chat.Inbound
		create   bool // a session may be created
		submit   bool // Submit is asked at all
	}{
		{"no triggers: a mention starts a thread", nil, inbound("C1", "5.5", "", true), true, true},
		{"no triggers: a mention in a thread", nil, inbound("C1", "5.5", "4.4", true), true, true},
		{"explicit mention trigger: a mention starts a thread", []v1alpha1.Trigger{mention, kindOnly()}, inbound("C1", "5.5", "", true), true, true},
		{"explicit mention trigger after another: a mention in a thread", []v1alpha1.Trigger{kindOnly(), mention}, inbound("C1", "5.5", "4.4", true), true, true},
		{"no mention trigger: a mention starts nothing", []v1alpha1.Trigger{kindOnly()}, inbound("C1", "5.5", "", true), false, false},
		{"no mention trigger: a mention in a thread creates no session, and only asks whether Agent Kourier owns the thread", []v1alpha1.Trigger{kindOnly()}, inbound("C1", "5.5", "4.4", true), false, true},
		{"no mention trigger: a reply in a thread Agent Kourier owns continues it", []v1alpha1.Trigger{kindOnly()}, inbound("C1", "5.5", "4.4", false), false, true},
		{"no mention trigger: a message outside a thread is ignored", []v1alpha1.Trigger{kindOnly()}, inbound("C1", "5.5", "", false), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTriggered(t, tt.triggers...)
			if err := h.in.Message(h.ctx, tt.m); err != nil {
				t.Fatal(err)
			}
			got := h.sub.submitted()
			if (len(got) == 1) != tt.submit || len(got) > 1 {
				t.Fatalf("submitted %+v; want a submit: %v", got, tt.submit)
			}
			if tt.submit && (got[0].Create != tt.create || (tt.create && got[0].Binding != "sre/sre-alerts") || (!tt.create && got[0].Binding != "")) {
				t.Errorf("submitted %+v; want create=%v", got[0], tt.create)
			}
		})
	}
}

// The intake says once, at INFO, that a Binding's triggers leave out the mention one, at its first mention, and
// not for a Binding that has it, nor again.
func TestABindingWithoutAMentionTriggerSaysSoOnce(t *testing.T) {
	h := newTriggered(t, kindOnly())
	for _, m := range []chat.Inbound{inbound("C1", "5.5", "", true), inbound("C1", "5.6", "", true), inbound("C1", "5.7", "4.4", true)} {
		if err := h.in.Message(h.ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	logs := h.logs.String()
	if n := strings.Count(logs, "have no mention trigger"); n != 1 || !strings.Contains(logs, `"level":"INFO"`) || !strings.Contains(logs, "sre/sre-alerts") {
		t.Errorf("%d notices in %q; want one, at INFO, naming the Binding", n, logs)
	}
	other := newTriggered(t, v1alpha1.Trigger{Type: v1alpha1.TriggerMention}, kindOnly())
	if err := other.in.Message(other.ctx, inbound("C1", "5.5", "", true)); err != nil || strings.Contains(other.logs.String(), "have no mention trigger") {
		t.Errorf("a Binding with a mention trigger was told off: %v %q", err, other.logs.String())
	}
}

// The notice that a Binding has no mention trigger says that a reply in a thread Agent Kourier owns still continues it,
// which is true only when the Binding takes all replies: in mention mode it says that a reply needs a mention.
func TestTheNoMentionTriggerNoticeTellsTheTruthAboutReplies(t *testing.T) {
	for _, tt := range []struct {
		mode        v1alpha1.ThreadReplies
		want, avoid string
	}{
		{v1alpha1.ThreadRepliesAll, "replies in a thread Agent Kourier owns still continue it", "only a reply that mentions courier"},
		{"", "replies in a thread Agent Kourier owns still continue it", "only a reply that mentions courier"},
		{v1alpha1.ThreadRepliesMention, "only a reply that mentions courier continues it", "replies in a thread Agent Kourier owns still continue it"},
	} {
		h := newTriggered(t, kindOnly())
		rb, _ := h.snap.Load().Binding("sre", "sre-alerts")
		rb.Binding.Spec.Chat.ThreadReplies = tt.mode
		if err := h.in.Message(h.ctx, inbound("C1", "5.5", "", true)); err != nil {
			t.Fatal(err)
		}
		if logs := h.logs.String(); !strings.Contains(logs, tt.want) || strings.Contains(logs, tt.avoid) {
			t.Errorf("mode %q logged %q; want %q and not %q", tt.mode, logs, tt.want, tt.avoid)
		}
	}
}
