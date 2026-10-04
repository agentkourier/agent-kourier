package chat_test

import (
	"maps"
	"strconv"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// replayOf is a Replay over the Binding that newTriggered's routing table holds for C1.
func replayOf(t *testing.T, h *triggered) *chat.Replay {
	t.Helper()
	rb, ok := h.snap.Load().Binding("sre", "sre-alerts")
	if !ok {
		t.Fatal("no Binding sre/sre-alerts")
	}
	r, err := chat.NewReplay(rb.Binding)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// history is a message of the history as the adapter would hand it on.
func history(id string, b *chat.BotMessage) chat.HistoryMessage {
	m := botInbound("C1", id, chat.BotMessage{})
	m.Bot = b
	return chat.HistoryMessage{Message: m}
}

// person is a person's message in the history.
func person(id string) chat.HistoryMessage {
	return chat.HistoryMessage{Message: chat.Inbound{At: chat.Ref{Platform: "slack", Conn: conn, Channel: "C1", ID: id}, User: "U1", Text: "is this real?"}}
}

// The owner's channel, replayed through the trial and the production triggers: each message ends in the outcome
// the intake would give it, with the reason and what it extracted. The same message is a match under one and
// unmatched under the other, which is what the dry run is for.
func TestReplayClassifiesTheOwnersChannel(t *testing.T) {
	infoInhibitor := kindFiring
	infoInhibitor.Title = "[kind] FIRING (1) - InfoInhibitor"
	otherBot := kindFiring
	otherBot.BotID = "B0OTHERBOT1"
	exclude := func(tr v1alpha1.Trigger) v1alpha1.Trigger {
		tr.Exclude = map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: `^Prometheus heartbeat$|- InfoInhibitor$`}
		return tr
	}
	reply := history("9.2", &kindFiring)
	reply.Message.At.Thread = "9.1"
	own := history("9.3", &kindFiring)
	own.Own = true

	kindFields := map[string]string{"state": "FIRING", "alertname": "KubePodCrashLooping", "key": "KubePodCrashLooping shop"}
	tests := []struct {
		name    string
		trigger v1alpha1.Trigger
		msg     chat.HistoryMessage
		want    chat.ReplayVerdict
	}{
		{"kind firing, trial", exclude(trialTrigger()), history("1.1", &kindFiring), chat.ReplayVerdict{Outcome: chat.ReplayMatched, Trigger: 0, Fields: kindFields}},
		{"kind resolved, trial", exclude(trialTrigger()), history("1.2", &kindResolved), chat.ReplayVerdict{Outcome: chat.ReplayMatched, Trigger: 0,
			Fields: map[string]string{"state": "RESOLVED", "alertname": "KubePodCrashLooping", "key": "KubePodCrashLooping shop"}}},
		{"production firing, trial: the receiver differs", exclude(trialTrigger()), history("1.3", &prodFiring), chat.ReplayVerdict{Outcome: chat.ReplayUnmatched, Why: "trigger 0: match titleLink"}},
		{"production firing, production", exclude(productionTrigger()), history("1.4", &prodFiring), chat.ReplayVerdict{Outcome: chat.ReplayMatched, Trigger: 0,
			Fields: map[string]string{"state": "FIRING", "alertname": "KubeAPIErrorBudgetBurn", "key": "KubeAPIErrorBudgetBurn kube-system"}}},
		{"kind firing, production", exclude(productionTrigger()), history("1.5", &kindFiring), chat.ReplayVerdict{Outcome: chat.ReplayUnmatched, Why: "trigger 0: match titleLink"}},
		{"heartbeat", exclude(productionTrigger()), history("1.6", &heartbeat), chat.ReplayVerdict{Outcome: chat.ReplayUnmatched, Why: "trigger 0: match title"}},
		{"Grafana", exclude(productionTrigger()), history("1.7", &grafanaFiring), chat.ReplayVerdict{Outcome: chat.ReplayUnmatched, Why: "trigger 0: match fallback"}},
		{"an alert the exclude names", exclude(trialTrigger()), history("1.8", &infoInhibitor), chat.ReplayVerdict{Outcome: chat.ReplayExcluded, Why: "trigger 0: exclude title"}},
		{"the heartbeat, when only the exclude turns it away", botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
			tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldFallback: `^\[(?:FIRING|RESOLVED)`}
			tr.Exclude = map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: `^Prometheus heartbeat$`}
		}), history("1.9", &heartbeat), chat.ReplayVerdict{Outcome: chat.ReplayExcluded, Why: "trigger 0: exclude title"}},
		{"an extract that misses", botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
			tr.Extract = map[string]v1alpha1.Extract{"key": {Field: v1alpha1.FieldFooter, Regex: `^courier-key: (?P<key>\S+)$`}}
		}), history("1.10", &kindFiring), chat.ReplayVerdict{Outcome: chat.ReplayUnmatched, Why: "trigger 0: extract key"}},
		{"another bot", exclude(trialTrigger()), history("2.1", &otherBot), chat.ReplayVerdict{Outcome: chat.ReplayOtherBot}},
		{"a person", exclude(trialTrigger()), person("2.2"), chat.ReplayVerdict{Outcome: chat.ReplayPerson}},
		{"courier's own post", exclude(trialTrigger()), own, chat.ReplayVerdict{Outcome: chat.ReplayOwn}},
		{"a message the adapter drops", exclude(trialTrigger()), chat.HistoryMessage{Ignored: true}, chat.ReplayVerdict{Outcome: chat.ReplayIgnored}},
		{"a bot's reply in a thread", exclude(trialTrigger()), reply, chat.ReplayVerdict{Outcome: chat.ReplayIgnored}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := replayOf(t, newTriggered(t, tc.trigger))
			got := r.Classify(tc.msg)
			if got.Outcome != tc.want.Outcome || got.Trigger != tc.want.Trigger || got.Why != tc.want.Why || !maps.Equal(got.Fields, tc.want.Fields) {
				t.Fatalf("Classify = %+v; want %+v", got, tc.want)
			}
		})
	}
}

// With several triggers the first that matches wins, as in the intake; a message that none takes lists each trigger
// of its bot with where it failed, and another bot's trigger is not among them.
func TestReplayFollowsTheOrderOfTheTriggersAndExplainsEachMiss(t *testing.T) {
	first := botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
		tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: `^nothing`}
	})
	second := botTrigger("B0OTHERBOT1", nil)
	third := botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
		tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: `FIRING`}
		tr.Extract = map[string]v1alpha1.Extract{"state": {Field: v1alpha1.FieldTitle, Regex: `(?P<state>FIRING)`}}
	})
	r := replayOf(t, newTriggered(t, first, second, third))
	if got := r.Classify(history("1.1", &kindFiring)); got.Outcome != chat.ReplayMatched || got.Trigger != 2 || got.Fields["state"] != "FIRING" {
		t.Errorf("Classify = %+v; want a match of trigger 2, the second of the webhook bot", got)
	}
	if got := r.Classify(history("1.2", &heartbeat)); got.Outcome != chat.ReplayUnmatched || got.Why != "trigger 0: match title; trigger 2: match title" {
		t.Errorf("Classify = %+v; want unmatched, with the misses of triggers 0 and 2", got)
	}
}

// Whatever the dry run says is matched is exactly what the intake hands to its trigger hook, with the same
// trigger and fields, and what it says is not matched never reaches the hook: the two cannot disagree.
func TestReplayAgreesWithTheIntake(t *testing.T) {
	msgs := []chat.BotMessage{kindFiring, kindResolved, kindGroup, prodFiring, prodResolved, grafanaFiring, heartbeat}
	for _, tr := range []v1alpha1.Trigger{trialTrigger(), productionTrigger()} {
		h := newTriggered(t, tr)
		r := replayOf(t, h)
		for i, m := range msgs {
			id := strconv.Itoa(i+1) + ".0"
			before := len(h.got())
			if err := h.in.Message(h.ctx, botInbound("C1", id, m)); err != nil {
				t.Fatal(err)
			}
			intake := h.got()[before:]
			got := r.Classify(history(id, &m))
			if matched := got.Outcome == chat.ReplayMatched; matched != (len(intake) == 1) {
				t.Fatalf("%q: replay says %q; the intake handed on %d matches", m.Title, got.Outcome, len(intake))
			}
			if len(intake) == 1 && (intake[0].Index != got.Trigger || !maps.Equal(intake[0].Fields, got.Fields)) {
				t.Errorf("%q: replay says trigger %d with %v; the intake %d with %v", m.Title, got.Trigger, got.Fields, intake[0].Index, intake[0].Fields)
			}
		}
	}
}

// A Binding whose trigger does not compile cannot be replayed, and says which; load rejects such a trigger, so this is
// a Binding that did not come through the loader.
func TestNewReplayRefusesATriggerThatDoesNotCompile(t *testing.T) {
	b := &v1alpha1.Binding{}
	b.Spec.Chat.Triggers = []v1alpha1.Trigger{{Type: v1alpha1.TriggerBotMessage, From: &v1alpha1.BotSource{BotID: webhookBot},
		Match: map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: `(`}}}
	if _, err := chat.NewReplay(b); err == nil {
		t.Fatal("NewReplay accepted a regex that does not compile")
	}
}
