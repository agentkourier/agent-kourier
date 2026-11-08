package chat_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// personTrigger is a personMessage trigger, as the Binding loader would hand it on after defaulting. Its limits are the
// ones every personMessage trigger must set, wide enough that no test reaches them and no daily cap.
func personTrigger(mutate func(*v1alpha1.Trigger)) v1alpha1.Trigger {
	t := v1alpha1.Trigger{Type: v1alpha1.TriggerPersonMessage,
		PromptTemplate: "A person wrote in {{ .Channel }}.",
		Limits: v1alpha1.TriggerLimits{
			RateLimit:     &v1alpha1.RateLimit{MaxRuns: 1000, Per: metav1.Duration{Duration: time.Hour}, MaxConcurrent: 1000},
			MaxRunsPerDay: new(int32(0)),
		},
	}
	if mutate != nil {
		mutate(&t)
	}
	return t
}

// personPost is a person's message with text in channel at id, in the thread (empty for none).
func personPost(channel, id, thread, text string) chat.Inbound {
	m := inbound(channel, id, thread, false)
	m.Text = text
	return m
}

// A person's top-level message starts a turn of its own, anchored to the message, when a personMessage trigger of the
// channel's Binding takes it: its match regexes read the text, and no exclude entry may.
func TestAPersonsTopLevelMessageMatchesAPersonMessageTrigger(t *testing.T) {
	onlyText := func(match, exclude string) v1alpha1.Trigger {
		return personTrigger(func(tr *v1alpha1.Trigger) {
			if match != "" {
				tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldText: match}
			}
			if exclude != "" {
				tr.Exclude = map[v1alpha1.MessageField]string{v1alpha1.FieldText: exclude}
			}
		})
	}
	tests := []struct {
		name    string
		trigger v1alpha1.Trigger
		msg     chat.Inbound
		match   bool
	}{
		{"a trigger with no regexes takes any top-level message", personTrigger(nil), personPost("C1", "9.9", "", "checkout is down"), true},
		{"the root of a thread is top-level", personTrigger(nil), personPost("C1", "9.9", "9.9", "checkout is down"), true},
		{"a reply in a thread is not", personTrigger(nil), personPost("C1", "9.9", "1.1", "checkout is down"), false},
		{"match reads the text", onlyText(`(?i)down`, ""), personPost("C1", "9.9", "", "Checkout is DOWN"), true},
		{"a match that fails is no match", onlyText(`(?i)down`, ""), personPost("C1", "9.9", "", "all quiet"), false},
		{"an exclude turns a message away that match accepts", onlyText(`checkout`, `^(thanks|ok)\b`), personPost("C1", "9.9", "", "thanks, checkout is back"), false},
		{"a message no exclude entry matches is kept", onlyText(`checkout`, `^(thanks|ok)\b`), personPost("C1", "9.9", "", "checkout is down"), true},
		{"a message with no user is nobody's", personTrigger(nil), chat.Inbound{At: chat.Ref{Platform: "slack", Conn: conn, Channel: "C1", ID: "9.9"}, Text: "checkout is down"}, false},
		{"a direct message is not served", personTrigger(nil), func() chat.Inbound {
			m := personPost("C1", "9.9", "", "checkout is down")
			m.Direct = true
			return m
		}(), false},
		{"a channel with no such trigger is not read", personTrigger(nil), personPost("G1", "9.9", "", "checkout is down"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTriggered(t, tt.trigger)
			if err := h.in.Message(h.ctx, tt.msg); err != nil {
				t.Fatal(err)
			}
			got := h.got()
			if (len(got) == 1) != tt.match || len(got) > 1 {
				t.Fatalf("started %d times, want a start: %v", len(got), tt.match)
			}
			if !tt.match {
				return
			}
			m := got[0]
			if m.Message.At.ID != tt.msg.At.ID || m.Index != 0 || m.Binding != bindingName || m.Mention || !m.Person || m.Message.User != tt.msg.User {
				t.Errorf("match = binding %q trigger %d anchored to %q, mention %v, person %v by %q; want %s trigger 0 anchored to the message itself, a person's, by %q",
					m.Binding, m.Index, m.Message.At.ID, m.Mention, m.Person, m.Message.User, bindingName, tt.msg.User)
			}
			if len(m.Fields) != 0 || m.Thread.By != "" || m.PromptTemplate != tt.trigger.PromptTemplate || m.Limits.RateLimit == nil {
				t.Errorf("match = fields %v thread %+v prompt %q limits %+v; want no fields, no thread rules, the trigger's prompt and limits", m.Fields, m.Thread, m.PromptTemplate, m.Limits)
			}
			if h.counter(t, "agentkourier_trigger_matched_total", bindingName) != 1 {
				t.Error("agentkourier_trigger_matched_total did not count the match")
			}
		})
	}
}

// Replies keep their way: one in a thread goes to the session manager whatever triggers the Binding has, and a
// personMessage trigger starts nothing from it. The thread of a message the trigger started is one of those.
func TestAReplyInAThreadIsNotAPersonMessageStart(t *testing.T) {
	h := newTriggered(t, personTrigger(nil))
	if err := h.in.Message(h.ctx, personPost("C1", "9.9", "", "checkout is down")); err != nil {
		t.Fatal(err)
	}
	if err := h.in.Message(h.ctx, personPost("C1", "9.10", "9.9", "it is the database")); err != nil {
		t.Fatal(err)
	}
	if got := h.started(); len(got) != 1 || got[0] != "9.9" {
		t.Errorf("turns started for %v; want one, for 9.9", got)
	}
	subs := h.sub.submitted()
	if len(subs) != 1 || subs[0].Key.Thread != "9.9" || subs[0].Create || subs[0].MessageRef != "9.10" {
		t.Errorf("submitted = %+v; want the reply, to the session of thread 9.9, creating none", subs)
	}
}

// A bot's message is never a person's, whatever it says: a personMessage trigger leaves it to the botMessage triggers,
// and a person's message is no input of a botMessage trigger.
func TestAPersonMessageTriggerTakesNoBotAndABotTriggerNoPerson(t *testing.T) {
	h := newTriggered(t, personTrigger(nil))
	if err := h.in.Message(h.ctx, botInbound("C1", "9.9", chat.BotMessage{ID: webhookBot, Title: "FIRING"})); err != nil {
		t.Fatal(err)
	}
	if len(h.got()) != 0 || h.counter(t, "agentkourier_trigger_unmatched_total", bindingName) != 1 {
		t.Errorf("a bot's message started %d turns and was unmatched %v times; want none, and once", len(h.got()), h.counter(t, "agentkourier_trigger_unmatched_total", bindingName))
	}

	h = newTriggered(t, botTrigger(webhookBot, nil))
	if err := h.in.Message(h.ctx, personPost("C1", "9.9", "", "FIRING")); err != nil {
		t.Fatal(err)
	}
	if len(h.got()) != 0 || h.counter(t, "agentkourier_trigger_unmatched_total", bindingName) != 0 {
		t.Errorf("a person's message started %d turns and was unmatched %v times; want none, and not counted", len(h.got()), h.counter(t, "agentkourier_trigger_unmatched_total", bindingName))
	}
}

// Triggers are tried in order and the first match handles the message, whatever the sender: a bot trigger and a
// person trigger side by side each take their own.
func TestBotAndPersonTriggersSideBySideTakeTheirOwnSenders(t *testing.T) {
	h := newTriggered(t, botTrigger(webhookBot, nil), personTrigger(nil))
	if err := h.in.Message(h.ctx, botInbound("C1", "9.9", chat.BotMessage{ID: webhookBot, Title: "FIRING"})); err != nil {
		t.Fatal(err)
	}
	if err := h.in.Message(h.ctx, personPost("C1", "9.10", "", "checkout is down")); err != nil {
		t.Fatal(err)
	}
	got := h.got()
	if len(got) != 2 || got[0].Index != 0 || got[0].Message.At.ID != "9.9" || got[1].Index != 1 || got[1].Message.At.ID != "9.10" {
		t.Fatalf("matches = %+v; want the bot's message by trigger 0, the person's by trigger 1", got)
	}
	if got[0].Person || !got[1].Person {
		t.Errorf("person = %v and %v; want the bot's match a botMessage trigger's and the person's a personMessage trigger's", got[0].Person, got[1].Person)
	}
}

// A person's message holds its place in the trigger's concurrency for its own turn alone: while that turn's task runs, a second
// report is limited; once it has ended, a later turn in its thread (a person's mentioned reply, running or queued) holds no
// place, so the next report starts. An alert's start, by contrast, counts every task of its thread
// (TestMaxConcurrentCountsRunningInvestigations).
func TestAPersonMessageHoldsItsConcurrencyPlaceForItsOwnTurnAlone(t *testing.T) {
	h := newLimited(t, personTrigger(func(tr *v1alpha1.Trigger) {
		tr.Limits.RateLimit = &v1alpha1.RateLimit{MaxRuns: 100, Per: metav1.Duration{Duration: time.Hour}, MaxConcurrent: 1}
	}))
	report := func(id string) {
		t.Helper()
		if err := h.in.Message(h.ctx, personPost("C1", id, "", "checkout is down "+id)); err != nil {
			t.Fatal(err)
		}
		h.clk.advance(time.Second)
	}
	key := store.ConversationKey{Connection: conn, Channel: "C1", Thread: "9.1"}
	report("9.1")
	if err := h.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: bindingName, Origin: store.Origin{Source: store.OriginPersonMessage}, LastActivity: h.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	// The start's own task runs, as the session manager records it: the turn of the message that began the session.
	if err := h.st.SetSessionTask(t.Context(), key, "c1", "t1", store.TaskTurn{ID: "slack:" + conn + ":C1:9.1", User: "U1", MessageRef: "9.1", Mention: "9.1"}); err != nil {
		t.Fatal(err)
	}
	report("9.2")
	// The start's task ended, and a person's mentioned reply in its thread runs a task of its own, with another queued behind it.
	if err := h.st.SetSessionTask(t.Context(), key, "c1", "t2", store.TaskTurn{ID: "slack:" + conn + ":C1:9.5", User: "U2", MessageRef: "9.5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.EnqueueReply(t.Context(), store.Reply{ID: "slack:" + conn + ":C1:9.6", Key: key, UserID: "U2", MessageRef: "9.6", Text: "and now?", EnqueuedAt: h.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	report("9.3")
	if got := h.started(); !slices.Equal(got, []string{"9.1", "9.3"}) {
		t.Errorf("turns started for %v; want 9.1, and 9.3 once only later turns of its thread run", got)
	}
	if got := h.limitedCount(t, "0"); got["concurrent"] != 1 || len(got) != 1 {
		t.Errorf("agentkourier_trigger_limited_total = %v; want one, for 9.2, by the concurrency limit", got)
	}
}

// A top-level message that mentions Agent Kourier is the mention trigger's where the Binding has one, so a person who asks
// for the agent is not also a report that takes a place in the personMessage trigger's limits. Where the Binding has no
// mention trigger it is a message like any other.
func TestAMentionIsTheMentionTriggersWhereThereIsOne(t *testing.T) {
	mention := v1alpha1.Trigger{Type: v1alpha1.TriggerMention}
	for _, tt := range []struct {
		name      string
		triggers  []v1alpha1.Trigger
		wantStart bool
		wantAsk   bool // submitted to the session manager as a new session
	}{
		{"mention first", []v1alpha1.Trigger{mention, personTrigger(nil)}, false, true},
		{"mention last", []v1alpha1.Trigger{personTrigger(nil), mention}, false, true},
		{"no mention trigger", []v1alpha1.Trigger{personTrigger(nil)}, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newTriggered(t, tt.triggers...)
			m := personPost("C1", "9.9", "", "<@UAGENT> checkout is down")
			m.Mentioned = true
			if err := h.in.Message(h.ctx, m); err != nil {
				t.Fatal(err)
			}
			if got := len(h.got()) == 1; got != tt.wantStart {
				t.Errorf("a trigger started %v times; want a start: %v", len(h.got()), tt.wantStart)
			}
			subs := h.sub.submitted()
			if asked := len(subs) == 1 && subs[0].Create && subs[0].Binding == bindingName; asked != tt.wantAsk || (!tt.wantAsk && len(subs) != 0) {
				t.Errorf("submitted = %+v; want a new session for the mention: %v", subs, tt.wantAsk)
			}
		})
	}
}

// A message no personMessage trigger takes is not remembered: its other event, if it says the message mentions Agent Kourier,
// is still the mention trigger's, and a redelivery is tried, and counted unmatched, again.
func TestAnUnmatchedPersonMessageIsNotRemembered(t *testing.T) {
	h := newTriggered(t, v1alpha1.Trigger{Type: v1alpha1.TriggerMention}, personTrigger(func(tr *v1alpha1.Trigger) {
		tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldText: `(?i)down`}
	}))
	m := personPost("C1", "9.9", "", "please look")
	for range 2 {
		if err := h.in.Message(h.ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.counter(t, "agentkourier_trigger_unmatched_total", bindingName); n != 2 || len(h.sub.submitted()) != 0 {
		t.Fatalf("unmatched %v times and submitted %+v; want twice, and nothing", n, h.sub.submitted())
	}
	m.Mentioned = true
	if err := h.in.Message(h.ctx, m); err != nil {
		t.Fatal(err)
	}
	if subs := h.sub.submitted(); len(subs) != 1 || !subs[0].Create || subs[0].Key.Thread != "9.9" || len(h.got()) != 0 {
		t.Errorf("submitted %+v and started %d; want the mention as a new session, and no trigger start", subs, len(h.got()))
	}
}

// A message is one start however many times Slack sends it: the second event of a message, and a redelivery, are a
// dedup and start nothing.
func TestAPersonMessageIsOneStartWhenItIsDeliveredAgain(t *testing.T) {
	h := newLimited(t, personTrigger(nil))
	for range 3 {
		if err := h.in.Message(h.ctx, personPost("C1", "9.9", "", "checkout is down")); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.started(); len(got) != 1 {
		t.Fatalf("turns started for %v; want one", got)
	}
	if d := h.decisions(t); d["started"] != 1 || d["dedup"] != 2 {
		t.Errorf("decisions = %v; want one started and two dedup", d)
	}
}

// The limits bound a flood of people's messages like a storm of alerts: past them a message starts nothing and the thread
// is silent, and the window gets one digest line that counts messages, not bot messages.
func TestAFloodOfPeopleIsHeldToTheTriggersLimits(t *testing.T) {
	h := newLimited(t, personTrigger(func(tr *v1alpha1.Trigger) {
		tr.Limits.RateLimit = &v1alpha1.RateLimit{MaxRuns: 2, Per: metav1.Duration{Duration: 10 * time.Minute}, MaxConcurrent: 5}
	}))
	for i, id := range []string{"9.1", "9.2", "9.3", "9.4"} {
		if err := h.in.Message(h.ctx, personPost("C1", id, "", "checkout is down "+strings.Repeat("x", i))); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.started(); len(got) != 2 {
		t.Errorf("turns started for %v; want the first two", got)
	}
	if got := h.limitedCount(t, "0"); got["rate"] != 2 {
		t.Errorf("limited = %v; want two stopped by the rate limit", got)
	}
	if d := h.decisions(t); d["started"] != 2 || d["limited"] != 2 {
		t.Errorf("decisions = %v; want two started and two limited", d)
	}
	h.notes.mu.Lock()
	notes := len(h.notes.got)
	h.notes.mu.Unlock()
	if notes != 0 || len(h.sub.submitted()) != 0 {
		t.Errorf("%d notes and %d submitted replies; want a limited message to leave its thread alone", notes, len(h.sub.submitted()))
	}
	if ds := h.digests(t); len(ds) != 1 || ds[0].Counts["rate"] != 2 {
		t.Errorf("digests = %+v; want one, counting the two", ds)
	}
}

// What a person wrote is untrusted data for the agent and nothing else: it reaches no log line, and no metric label.
func TestWhatAPersonWroteReachesNoLogOrMetric(t *testing.T) {
	const secret = "swordfish-hunter2"
	h := newLimited(t, personTrigger(nil), personTrigger(func(tr *v1alpha1.Trigger) { tr.Match = map[v1alpha1.MessageField]string{v1alpha1.FieldText: `nomatch`} }))
	for _, id := range []string{"9.1", "9.2"} {
		if err := h.in.Message(h.ctx, personPost("C1", id, "", "the "+secret+" is in the config")); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.in.Message(h.ctx, personPost("C1", "9.3", "", secret)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.logs.String(), secret) {
		t.Errorf("the message's text reached the log:\n%s", h.logs.String())
	}
	families, err := h.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, m := range f.Metric {
			for _, l := range m.Label {
				if strings.Contains(l.GetValue(), secret) {
					t.Errorf("the message's text reached the label %s=%q of %s", l.GetName(), l.GetValue(), f.GetName())
				}
			}
		}
	}
}
