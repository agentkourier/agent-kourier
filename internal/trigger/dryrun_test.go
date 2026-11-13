package trigger_test

import (
	"maps"
	"strconv"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/trigger"
)

// replayOf is a Replay over the Binding that newTriggered's routing table holds for C1.
func replayOf(t *testing.T, h *triggered) *trigger.Replay {
	t.Helper()
	rb, ok := h.snap.Load().Binding("sre", "sre-alerts")
	if !ok {
		t.Fatal("no Binding sre/sre-alerts")
	}
	r, err := trigger.NewReplay(rb.Binding)
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
	otherBot.ID = "B0OTHERBOT1"
	otherKind := kindFiring
	otherKind.Kind = "webhook"
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
		want    trigger.ReplayVerdict
	}{
		{"kind firing, trial", exclude(trialTrigger()), history("1.1", &kindFiring), trigger.ReplayVerdict{Outcome: trigger.ReplayMatched, Trigger: 0, Fields: kindFields}},
		{"kind resolved, trial", exclude(trialTrigger()), history("1.2", &kindResolved), trigger.ReplayVerdict{Outcome: trigger.ReplayMatched, Trigger: 0,
			Fields: map[string]string{"state": "RESOLVED", "alertname": "KubePodCrashLooping", "key": "KubePodCrashLooping shop"}}},
		{"production firing, trial: the receiver differs", exclude(trialTrigger()), history("1.3", &prodFiring), trigger.ReplayVerdict{Outcome: trigger.ReplayUnmatched, Why: "trigger 0: match link"}},
		{"production firing, production", exclude(productionTrigger()), history("1.4", &prodFiring), trigger.ReplayVerdict{Outcome: trigger.ReplayMatched, Trigger: 0,
			Fields: map[string]string{"state": "FIRING", "alertname": "KubeAPIErrorBudgetBurn", "key": "KubeAPIErrorBudgetBurn kube-system"}}},
		{"kind firing, production", exclude(productionTrigger()), history("1.5", &kindFiring), trigger.ReplayVerdict{Outcome: trigger.ReplayUnmatched, Why: "trigger 0: match link"}},
		{"heartbeat", exclude(productionTrigger()), history("1.6", &heartbeat), trigger.ReplayVerdict{Outcome: trigger.ReplayUnmatched, Why: "trigger 0: match link"}},
		{"Grafana", exclude(productionTrigger()), history("1.7", &grafanaFiring), trigger.ReplayVerdict{Outcome: trigger.ReplayUnmatched, Why: "trigger 0: match fallback"}},
		{"an alert the exclude names", exclude(trialTrigger()), history("1.8", &infoInhibitor), trigger.ReplayVerdict{Outcome: trigger.ReplayExcluded, Why: "trigger 0: exclude title"}},
		{"the heartbeat, when only the exclude turns it away", botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
			tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldFallback: `^\[(?:FIRING|RESOLVED)`}
			tr.Exclude = map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: `^Prometheus heartbeat$`}
		}), history("1.9", &heartbeat), trigger.ReplayVerdict{Outcome: trigger.ReplayExcluded, Why: "trigger 0: exclude title"}},
		{"an extract that misses", botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
			tr.Extract = map[string]v1alpha1.Extract{"key": {Field: v1alpha1.FieldFooter, Regex: `^agent-kourier-key: (?P<key>\S+)$`}}
		}), history("1.10", &kindFiring), trigger.ReplayVerdict{Outcome: trigger.ReplayUnmatched, Why: "trigger 0: extract key"}},
		{"the trigger's ID with another kind", exclude(trialTrigger()), history("2.0", &otherKind), trigger.ReplayVerdict{Outcome: trigger.ReplayUnmatched, Why: "trigger 0: sender kind"}},
		{"another bot", exclude(trialTrigger()), history("2.1", &otherBot), trigger.ReplayVerdict{Outcome: trigger.ReplayOtherBot}},
		{"a person", exclude(trialTrigger()), person("2.2"), trigger.ReplayVerdict{Outcome: trigger.ReplayPerson}},
		{"agent-kourier's own post", exclude(trialTrigger()), own, trigger.ReplayVerdict{Outcome: trigger.ReplayOwn}},
		{"a message the adapter drops", exclude(trialTrigger()), chat.HistoryMessage{Ignored: true}, trigger.ReplayVerdict{Outcome: trigger.ReplayIgnored}},
		{"a bot's reply in a thread", exclude(trialTrigger()), reply, trigger.ReplayVerdict{Outcome: trigger.ReplayIgnored}},
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
	if got := r.Classify(history("1.1", &kindFiring)); got.Outcome != trigger.ReplayMatched || got.Trigger != 2 || got.Fields["state"] != "FIRING" {
		t.Errorf("Classify = %+v; want a match of trigger 2, the second of the webhook bot", got)
	}
	if got := r.Classify(history("1.2", &heartbeat)); got.Outcome != trigger.ReplayUnmatched || got.Why != "trigger 0: match title; trigger 2: match title" {
		t.Errorf("Classify = %+v; want unmatched, with the misses of triggers 0 and 2", got)
	}
}

// Whatever the dry run says is matched is what the intake counts as matched, and the rest it counts as unmatched, for
// the same messages: the two cannot disagree. When the match starts a turn, which a firing alert does and a resolve
// or a repeat does not (it is a note in a thread), the intake hands on the same trigger and fields.
func TestReplayAgreesWithTheIntake(t *testing.T) {
	const binding = "sre/sre-alerts"
	msgs := []chat.BotMessage{kindFiring, kindResolved, kindGroup, prodFiring, prodResolved, grafanaFiring, heartbeat}
	for _, tr := range []v1alpha1.Trigger{trialTrigger(), productionTrigger()} {
		h := newTriggered(t, tr)
		r := replayOf(t, h)
		for i, m := range msgs {
			id := strconv.Itoa(i+1) + ".0"
			started, matched, unmatched := len(h.got()), h.counter(t, "agentkourier_trigger_matched_total", binding), h.counter(t, "agentkourier_trigger_unmatched_total", binding)
			if err := h.in.Message(h.ctx, botInbound("C1", id, m)); err != nil {
				t.Fatal(err)
			}
			intakeMatched := h.counter(t, "agentkourier_trigger_matched_total", binding)-matched == 1
			intakeUnmatched := h.counter(t, "agentkourier_trigger_unmatched_total", binding)-unmatched == 1
			got := r.Classify(history(id, &m))
			// Both ways: what replay calls matched is what the intake matched, and what it calls unmatched or excluded is
			// what the intake counted as unmatched, so that a message cannot be lost to some other outcome.
			replayUnmatched := got.Outcome == trigger.ReplayUnmatched || got.Outcome == trigger.ReplayExcluded
			if (got.Outcome == trigger.ReplayMatched) != intakeMatched || replayUnmatched != intakeUnmatched {
				t.Fatalf("%q: replay says %q; the intake matched %t and counted it unmatched %t", m.Title, got.Outcome, intakeMatched, intakeUnmatched)
			}
			if began := h.got()[started:]; len(began) == 1 && (began[0].Index != got.Trigger || !maps.Equal(began[0].Fields, got.Fields)) {
				t.Errorf("%q: replay says trigger %d with %v; the intake started %d with %v", m.Title, got.Trigger, got.Fields, began[0].Index, began[0].Fields)
			}
		}
	}
}

// A personMessage trigger is replayed as the intake runs it: a person's top-level message that it takes is matched, one its
// exclude turns away is excluded and says where, and one its match misses is unmatched. A person's reply in a thread, and a
// mention where the Binding has a mention trigger, are no input of it and stay person; a bot's message is still a botMessage
// trigger's alone.
func TestReplayRunsAPersonMessageTrigger(t *testing.T) {
	mention := v1alpha1.Trigger{Type: v1alpha1.TriggerMention}
	reports := personTrigger(func(tr *v1alpha1.Trigger) {
		tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldText: `(?i)down|slow`}
		tr.Exclude = map[v1alpha1.MessageField]string{v1alpha1.FieldText: `^(?i:thanks)\b`}
	})
	said := func(id, thread, text string, mentioned bool) chat.HistoryMessage {
		m := personPost("C1", id, thread, text)
		m.Mentioned = mentioned
		return chat.HistoryMessage{Message: m}
	}
	for _, tc := range []struct {
		name     string
		triggers []v1alpha1.Trigger
		msg      chat.HistoryMessage
		want     trigger.ReplayVerdict
	}{
		{"a report", []v1alpha1.Trigger{mention, reports}, said("1.1", "", "checkout is down", false), trigger.ReplayVerdict{Outcome: trigger.ReplayMatched, Trigger: 1, Fields: map[string]string{}}},
		{"thanks", []v1alpha1.Trigger{mention, reports}, said("1.2", "", "thanks, checkout is down no more", false), trigger.ReplayVerdict{Outcome: trigger.ReplayExcluded, Why: "trigger 1: exclude text"}},
		{"chatter", []v1alpha1.Trigger{mention, reports}, said("1.3", "", "lunch?", false), trigger.ReplayVerdict{Outcome: trigger.ReplayUnmatched, Why: "trigger 1: match text"}},
		{"a reply in a thread", []v1alpha1.Trigger{mention, reports}, said("1.5", "1.1", "it is slow too", false), trigger.ReplayVerdict{Outcome: trigger.ReplayPerson}},
		{"a mention, with a mention trigger", []v1alpha1.Trigger{reports, mention}, said("1.6", "", "<@UFAKEBOT> is it down?", true), trigger.ReplayVerdict{Outcome: trigger.ReplayPerson}},
		{"a mention, without one", []v1alpha1.Trigger{reports}, said("1.7", "", "<@UFAKEBOT> is it down?", true), trigger.ReplayVerdict{Outcome: trigger.ReplayMatched, Trigger: 0, Fields: map[string]string{}}},
		{"a person, with no personMessage trigger", []v1alpha1.Trigger{mention, trialTrigger()}, said("1.8", "", "checkout is down", false), trigger.ReplayVerdict{Outcome: trigger.ReplayPerson}},
		{"a bot, beside a personMessage trigger", []v1alpha1.Trigger{reports, trialTrigger()}, history("1.9", &kindFiring), trigger.ReplayVerdict{Outcome: trigger.ReplayMatched, Trigger: 1,
			Fields: map[string]string{"state": "FIRING", "alertname": "KubePodCrashLooping", "key": "KubePodCrashLooping shop"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := replayOf(t, newTriggered(t, tc.triggers...)).Classify(tc.msg)
			if got.Outcome != tc.want.Outcome || got.Trigger != tc.want.Trigger || got.Why != tc.want.Why || !maps.Equal(got.Fields, tc.want.Fields) {
				t.Fatalf("Classify = %+v; want %+v", got, tc.want)
			}
		})
	}
}

// What the dry run says of a person's message is what the intake does with it: matched where the intake starts a turn from a
// personMessage trigger, unmatched or excluded where it counts it unmatched, and person where it does neither.
func TestReplayOfAPersonAgreesWithTheIntake(t *testing.T) {
	const binding = "sre/sre-alerts"
	reports := personTrigger(func(tr *v1alpha1.Trigger) {
		tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldText: `(?i)down`}
		tr.Exclude = map[v1alpha1.MessageField]string{v1alpha1.FieldText: `^(?i:thanks)\b`}
	})
	h := newTriggered(t, v1alpha1.Trigger{Type: v1alpha1.TriggerMention}, reports)
	r := replayOf(t, h)
	for i, tc := range []struct {
		text, thread string
		mentioned    bool
	}{
		{"checkout is down", "", false}, {"thanks, it is down no more", "", false}, {"lunch?", "", false},
		{"<@UFAKEBOT> is it down?", "", true}, {"still down", "1.0", false},
	} {
		id := strconv.Itoa(i+1) + ".1"
		m := personPost("C1", id, tc.thread, tc.text)
		m.Mentioned = tc.mentioned
		started, unmatched := len(h.got()), h.counter(t, "agentkourier_trigger_unmatched_total", binding)
		if err := h.in.Message(h.ctx, m); err != nil {
			t.Fatal(err)
		}
		intakeStarted := len(h.got())-started == 1
		intakeUnmatched := h.counter(t, "agentkourier_trigger_unmatched_total", binding)-unmatched == 1
		got := r.Classify(chat.HistoryMessage{Message: m})
		replayUnmatched := got.Outcome == trigger.ReplayUnmatched || got.Outcome == trigger.ReplayExcluded
		if (got.Outcome == trigger.ReplayMatched) != intakeStarted || replayUnmatched != intakeUnmatched || (got.Outcome == trigger.ReplayPerson) != (!intakeStarted && !intakeUnmatched) {
			t.Errorf("%q: replay says %q; the intake started %t and counted it unmatched %t", tc.text, got.Outcome, intakeStarted, intakeUnmatched)
		}
	}
}

// A Binding whose trigger does not compile cannot be replayed, and says which; load rejects such a trigger, so this is
// a Binding that did not come through the loader.
func TestNewReplayRefusesATriggerThatDoesNotCompile(t *testing.T) {
	b := &v1alpha1.Binding{}
	b.Spec.Chat.Triggers = []v1alpha1.Trigger{{Type: v1alpha1.TriggerBotMessage, From: &v1alpha1.BotSource{ID: webhookBot},
		Match: map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: `(`}}}
	if _, err := trigger.NewReplay(b); err == nil {
		t.Fatal("NewReplay accepted a regex that does not compile")
	}
}
