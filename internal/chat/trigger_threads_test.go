package chat_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

const bindingName = "sre/sre-alerts"

// Words of the alerts of this file's messages that must never reach a note, a log line or the database: the
// alert's name, and a pod's.
var alertWords = []string{"KubePodCrashLooping", "KubePodNotReady", "checkout-7d9f8b6c5", "CrashLoopBackOff"}

// restart is a restart of Agent Kourier: the database is closed and opened again, and a new Intake, which remembers
// no message, is made over it. The hook, the notices and the clock stay the test's.
func (h *triggered) restart(t *testing.T) {
	t.Helper()
	h.reopenStore(t)
	h.cfg.TriggerThreads = h.st
	h.in = chat.NewIntake(h.cfg)
}

// decisions reads agentkourier_trigger_decisions_total of the test's Binding, by decision.
func (h *triggered) decisions(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := h.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "agentkourier_trigger_decisions_total" {
			continue
		}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if len(labels) != 2 {
				t.Errorf("agentkourier_trigger_decisions_total has labels %v; want binding and decision only", labels)
			}
			if labels["binding"] == bindingName {
				out[labels["decision"]] += m.Counter.GetValue()
			}
		}
	}
	return out
}

// send delivers m as message id of C1 and returns the decision it was counted as: the one decision whose count
// rose, or "" if none did.
func (h *triggered) send(t *testing.T, id string, m chat.BotMessage) string {
	t.Helper()
	before := h.decisions(t)
	if err := h.in.Message(h.ctx, botInbound("C1", id, m)); err != nil {
		t.Fatalf("message %s: %v", id, err)
	}
	var rose []string
	for d, n := range h.decisions(t) {
		if n > before[d] {
			rose = append(rose, d)
		}
	}
	switch len(rose) {
	case 0:
		return ""
	case 1:
		return rose[0]
	}
	t.Fatalf("message %s was counted as %v; want one decision", id, rose)
	return ""
}

// started is the anchors of the turns the start hook was asked for, in order.
func (h *triggered) started() []string {
	var out []string
	for _, m := range h.got() {
		out = append(out, m.Message.At.ID)
	}
	return out
}

// noted is the notices the Intake asked for, in order: their kind and the thread each is for.
func (h *triggered) noted() (kinds []session.NoticeKind, threads []string) {
	h.notes.mu.Lock()
	defer h.notes.mu.Unlock()
	for i, n := range h.notes.got {
		kinds, threads = append(kinds, n.Kind), append(threads, h.notes.key[i].Thread)
	}
	return kinds, threads
}

// The life of one alert, as the preset reads it: the first fire starts the investigation in its own thread; a
// fire again, inside the cooldown or not, while the alert has not resolved, is a note in that thread; the
// resolve is a note and closes the thread; a fire again within the cooldown of the start is a note in the same
// thread, and is no new investigation; one after the cooldown is.
func TestAnAlertThroughItsLife(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	steps := []struct {
		name     string
		after    time.Duration // the clock moves this far before the message
		id       string
		msg      chat.BotMessage
		decision string
		starts   []string // the anchors of all the turns started so far
		note     session.NoticeKind
		thread   string // the thread the note is for
		text     string // the time the note names
	}{
		{"fires", 0, "100.1", kindFiring, "started", []string{"100.1"}, "", "", ""},
		{"fires again, with the cooldown running", time.Hour, "101.1", kindFiring, "repeat", []string{"100.1"}, session.NoticeTriggerRepeat, "100.1", "2026-10-04 13:00 UTC."},
		{"resolves", 30 * time.Minute, "102.1", kindResolved, "closed", []string{"100.1"}, session.NoticeTriggerClosed, "100.1", "2026-10-04 13:30 UTC."},
		{"fires again after the close, within the cooldown", 30 * time.Minute, "103.1", kindFiring, "repeat", []string{"100.1"}, session.NoticeTriggerRepeat, "100.1", "2026-10-04 14:00 UTC."},
		{"resolves again: the thread was reopened, so this is a close", 10 * time.Minute, "104.1", kindResolved, "closed", []string{"100.1"}, session.NoticeTriggerClosed, "100.1", "2026-10-04 14:10 UTC."},
		{"resolves once more: nothing is open", time.Minute, "105.1", kindResolved, "close_unknown", []string{"100.1"}, "", "", ""},
		{"fires after the cooldown of the start", 2 * time.Hour, "106.1", kindFiring, "started", []string{"100.1", "106.1"}, "", "", ""},
		{"fires again: the new thread takes it", time.Minute, "107.1", kindFiring, "repeat", []string{"100.1", "106.1"}, session.NoticeTriggerRepeat, "106.1", "2026-10-04 16:12 UTC."},
	}
	for _, s := range steps {
		h.clk.advance(s.after)
		notesBefore, _ := h.noted()
		if got := h.send(t, s.id, s.msg); got != s.decision {
			t.Fatalf("%s: counted as %q; want %q", s.name, got, s.decision)
		}
		if got := h.started(); !slices.Equal(got, s.starts) {
			t.Fatalf("%s: turns started for %v; want %v", s.name, got, s.starts)
		}
		kinds, threads := h.noted()
		switch {
		case s.note == "" && len(kinds) != len(notesBefore):
			t.Fatalf("%s: a note was posted (%v); want none", s.name, kinds)
		case s.note != "":
			if len(kinds) != len(notesBefore)+1 || kinds[len(kinds)-1] != s.note || threads[len(threads)-1] != s.thread {
				t.Fatalf("%s: notes %v in %v; want one more, %s in thread %s", s.name, kinds, threads, s.note, s.thread)
			}
			n := h.notes.got[len(h.notes.got)-1]
			wantID := chat.MessageID(chat.Ref{Platform: "slack", Conn: conn, Channel: "C1", ID: s.id}) + "/" + string(s.note)
			if n.ID != wantID || n.MessageRef != s.id || n.Detail != s.text {
				t.Errorf("%s: notice %+v; want ID %s, message %s and the time %q only", s.name, n, wantID, s.id, s.text)
			}
			if k := h.notes.key[len(h.notes.key)-1]; k != (session.Key{Connection: conn, Channel: "C1", Thread: s.thread}) {
				t.Errorf("%s: the note is for %+v", s.name, k)
			}
		}
	}
	if got := h.sub.submitted(); len(got) != 0 {
		t.Errorf("the Intake submitted %+v itself; the start is the hook's", got)
	}
}

// The owner's flapping pattern (KubeAPIErrorBudgetBurn fired 68 times and resolved 27 in three months): many fire
// and resolve cycles inside the cooldown are one investigation.
func TestManyFireAndResolveCyclesInsideTheCooldownStartOneTurn(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	const cycles = 20 // every ten minutes: 3 h 20 min, inside the 4 h
	for i := range cycles {
		id := func(n int) string { return "1" + string(rune('a'+i)) + "." + string(rune('0'+n)) }
		want := "repeat"
		if i == 0 {
			want = "started"
		}
		if got := h.send(t, id(1), kindFiring); got != want {
			t.Fatalf("cycle %d fire: %q; want %q", i, got, want)
		}
		h.clk.advance(5 * time.Minute)
		if got := h.send(t, id(2), kindResolved); got != "closed" {
			t.Fatalf("cycle %d resolve: %q; want closed", i, got)
		}
		h.clk.advance(5 * time.Minute)
	}
	if got := h.started(); len(got) != 1 {
		t.Errorf("%d turns started over %d cycles inside the cooldown; want 1", len(got), cycles)
	}
	d := h.decisions(t)
	if d["started"] != 1 || d["repeat"] != cycles-1 || d["closed"] != cycles || len(d) != 3 {
		t.Errorf("decisions = %v; want 1 started, %d repeat, %d closed", d, cycles-1, cycles)
	}
	kinds, threads := h.noted()
	if len(kinds) != 2*cycles-1 || slices.ContainsFunc(threads, func(th string) bool { return th != "1a.1" }) {
		t.Errorf("%d notes in threads %v; want %d, all in the first thread", len(kinds), threads, 2*cycles-1)
	}
}

// Different keys are different threads, and a close takes only its own.
func TestTwoKeysHaveTwoThreads(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	for _, s := range []struct {
		id       string
		msg      chat.BotMessage
		decision string
	}{{"100.1", kindFiring, "started"}, {"101.1", kindGroup, "started"}, {"102.1", kindResolved, "closed"}, {"103.1", kindGroup, "repeat"}, {"104.1", kindFiring, "repeat"}} {
		if got := h.send(t, s.id, s.msg); got != s.decision {
			t.Fatalf("message %s: %q; want %q", s.id, got, s.decision)
		}
	}
	if got := h.started(); !slices.Equal(got, []string{"100.1", "101.1"}) {
		t.Errorf("turns started for %v; want the two firing messages", got)
	}
	kinds, threads := h.noted()
	if !slices.Equal(threads, []string{"100.1", "101.1", "100.1"}) ||
		!slices.Equal(kinds, []session.NoticeKind{session.NoticeTriggerClosed, session.NoticeTriggerRepeat, session.NoticeTriggerRepeat}) {
		t.Errorf("notes %v in %v; want the close of the first thread, then a repeat in each", kinds, threads)
	}
}

// A key belongs to the bot that sent it: the same text from another bot is a thread of its own.
func TestTheSameKeyFromAnotherBotIsAnotherThread(t *testing.T) {
	const other = "B0OTHERBOT1"
	second := trialTrigger()
	second.From = &v1alpha1.BotSource{BotID: other}
	h := newTriggered(t, trialTrigger(), second)
	otherFiring := kindFiring
	otherFiring.BotID = other
	if got := h.send(t, "100.1", kindFiring); got != "started" {
		t.Fatalf("first bot: %q", got)
	}
	if got := h.send(t, "101.1", otherFiring); got != "started" {
		t.Errorf("the same key from another bot: %q; want it a thread of its own", got)
	}
}

// A resolve with no thread for its key is ignored and counted, and a later fire is a new investigation.
func TestACloseWithNoKnownThreadIsIgnoredAndCounted(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	if got := h.send(t, "100.1", kindResolved); got != "close_unknown" {
		t.Fatalf("resolve with no thread: %q; want close_unknown", got)
	}
	if kinds, _ := h.noted(); len(kinds) != 0 || len(h.got()) != 0 {
		t.Errorf("notes %v, turns %v; want none: a close with no thread is ignored", kinds, h.started())
	}
	if got := h.send(t, "101.1", kindFiring); got != "started" {
		t.Errorf("the fire after it: %q; want started", got)
	}
}

// The mapping is in the database, not in memory: a restart loses neither the thread, nor the close, nor the
// cooldown.
func TestThreadsSurviveARestart(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	h.send(t, "100.1", kindFiring)
	h.restart(t)
	h.clk.advance(time.Hour)
	if got := h.send(t, "101.1", kindFiring); got != "repeat" {
		t.Fatalf("fire again after a restart: %q; want repeat in the thread that was there", got)
	}
	h.restart(t)
	if got := h.send(t, "102.1", kindResolved); got != "closed" {
		t.Fatalf("resolve after a restart: %q; want closed", got)
	}
	h.restart(t)
	h.clk.advance(time.Hour)
	if got := h.send(t, "103.1", kindFiring); got != "repeat" {
		t.Fatalf("fire again after a close and a restart, inside the cooldown: %q; want repeat", got)
	}
	h.restart(t)
	h.send(t, "104.1", kindResolved)
	h.restart(t)
	h.clk.advance(3 * time.Hour)
	if got := h.send(t, "105.1", kindFiring); got != "started" {
		t.Fatalf("fire again after the cooldown and a restart: %q; want started", got)
	}
	if got, want := h.started(), []string{"100.1", "105.1"}; !slices.Equal(got, want) {
		t.Errorf("turns started for %v; want %v", got, want)
	}
	if _, threads := h.noted(); !slices.Equal(threads, []string{"100.1", "100.1", "100.1", "100.1"}) {
		t.Errorf("notes in %v; want all four in the first thread", threads)
	}
}

// Slack sends an event again when it has not been acknowledged. One Intake remembers the messages it took, and
// counts a second delivery as a dedup; no turn, no note.
func TestARedeliveredMessageIsDedupedByTheIntake(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	for i, want := range []string{"started", "dedup", "dedup"} {
		if got := h.send(t, "100.1", kindFiring); got != want {
			t.Fatalf("delivery %d: %q; want %q", i, got, want)
		}
	}
	h.send(t, "101.1", kindResolved)
	for range 2 {
		if got := h.send(t, "101.1", kindResolved); got != "dedup" {
			t.Fatalf("redelivered resolve: %q; want dedup", got)
		}
	}
	if got := h.started(); !slices.Equal(got, []string{"100.1"}) {
		t.Errorf("turns started for %v; want the one", got)
	}
	if kinds, _ := h.noted(); len(kinds) != 1 {
		t.Errorf("notes %v; want the one close", kinds)
	}
	// An unmatched message is remembered too, and is not a decision.
	h.send(t, "102.1", heartbeat)
	if got := h.send(t, "102.1", heartbeat); got != "" {
		t.Errorf("redelivered heartbeat: counted as %q; want it not a decision", got)
	}
}

// After a restart nothing is remembered but the database. A redelivered fire again or resolve asks for the
// same notice (the notice ID names the message), and a redelivered start, whose turn the store knows started,
// asks for nothing.
func TestARedeliveryAfterARestartAsksForTheSameNoticeAndNoNewTurn(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	h.send(t, "100.1", kindFiring)
	h.send(t, "101.1", kindFiring)
	h.send(t, "102.1", kindResolved)

	h.restart(t)
	if got := h.send(t, "100.1", kindFiring); got != "dedup" {
		t.Errorf("redelivered start: %q; want dedup", got)
	}
	if got := h.started(); !slices.Equal(got, []string{"100.1"}) {
		t.Errorf("start hook given %v; want the one ask: the turn started", got)
	}
	h.restart(t)
	if got := h.send(t, "101.1", kindFiring); got != "repeat" {
		t.Errorf("redelivered fire again: %q; want repeat", got)
	}
	var ids []string
	for _, n := range h.notes.got {
		ids = append(ids, n.ID)
	}
	if len(ids) != 3 || ids[0] != ids[2] || ids[0] == ids[1] {
		t.Errorf("notice IDs %v; want the fire again's asked for twice with one ID, and the close's its own", ids)
	}
	if got := h.started(); len(got) != 1 {
		t.Errorf("turns started for %v; want the one", got)
	}
}

// A start the hook could not take is not remembered, and its redelivery finds the thread already opened: it asks
// for the start again rather than counting it as a repeat of itself.
func TestAFailedStartIsStartedAgainByItsRedelivery(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	h.hookErr = errors.New("session store unavailable")
	err := h.in.Message(h.ctx, botInbound("C1", "100.1", kindFiring))
	if !errors.Is(err, h.hookErr) {
		t.Fatalf("Message = %v; want the hook's error, so that Slack sends it again", err)
	}
	h.hookErr = nil
	if got := h.send(t, "100.1", kindFiring); got != "dedup" {
		t.Errorf("redelivery: %q; want dedup", got)
	}
	if got := h.started(); !slices.Equal(got, []string{"100.1", "100.1"}) {
		t.Errorf("start hook given %v; want it asked twice for the one anchor", got)
	}
	if d := h.decisions(t); d["started"] != 1 || d["dedup"] != 1 {
		t.Errorf("decisions = %v; want the start counted once, when the thread was opened, and the redelivery a dedup", d)
	}
	if got := h.send(t, "100.1", kindFiring); got != "dedup" || len(h.got()) != 2 {
		t.Errorf("third delivery: %q, hook given %v; want the Intake to remember it", got, h.started())
	}
	if got := h.send(t, "101.1", kindFiring); got != "repeat" {
		t.Errorf("the next fire: %q; want it a repeat in that thread", got)
	}
}

// A note the platform queue could not take leaves the event unacknowledged, and changes nothing: the thread is
// not closed, so that the redelivery closes it and posts its note.
func TestAFailedNoticeLeavesTheThreadAsItWasForTheRedelivery(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	h.send(t, "100.1", kindFiring)
	h.notes.fail = []error{errors.New("outbox unavailable")}
	if err := h.in.Message(h.ctx, botInbound("C1", "101.1", kindResolved)); err == nil {
		t.Fatal("Message = nil; want the error of the notice, so that Slack sends the event again")
	}
	if d := h.decisions(t); d["closed"] != 0 {
		t.Errorf("decisions = %v; want a close that did not happen not counted", d)
	}
	if got := h.send(t, "101.1", kindResolved); got != "closed" {
		t.Errorf("redelivery: %q; want closed: the first try changed nothing", got)
	}
	h.notes.fail = []error{errors.New("outbox unavailable")}
	if err := h.in.Message(h.ctx, botInbound("C1", "102.1", kindFiring)); err == nil {
		t.Fatal("repeat: Message = nil; want the error of the notice")
	}
	if got := h.send(t, "102.1", kindFiring); got != "repeat" {
		t.Errorf("redelivered repeat: %q; want repeat", got)
	}
}

// The store's failures are the event's: unacknowledged, and nothing remembered.
func TestAStoreFailureLeavesTheEventToBeRedelivered(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	boom := errors.New("disk full")
	cfg := h.cfg
	cfg.TriggerThreads = failingThreads{h.st, boom}
	in := chat.NewIntake(cfg)
	for _, id := range []string{"100.1", "101.1"} {
		m := kindFiring
		if id == "101.1" {
			m = kindResolved
		}
		if err := in.Message(h.ctx, botInbound("C1", id, m)); !errors.Is(err, boom) {
			t.Errorf("message %s: %v; want the store's error", id, err)
		}
	}
	if len(h.got()) != 0 || len(h.notes.got) != 0 {
		t.Errorf("turns %v, notices %v after failures; want none", h.started(), h.notes.got)
	}
	if got := h.send(t, "100.1", kindFiring); got != "started" {
		t.Errorf("redelivery with a working store: %q; want started", got)
	}
}

// failingThreads is a TriggerThreads whose every call fails with err.
type failingThreads struct {
	store.TriggerThreads
	err error
}

func (f failingThreads) OpenTriggerThread(context.Context, store.TriggerThread, store.TriggerStale) (store.TriggerThread, bool, error) {
	return store.TriggerThread{}, false, f.err
}

func (f failingThreads) GetTriggerThread(context.Context, string, string) (store.TriggerThread, error) {
	return store.TriggerThread{}, f.err
}

// A trigger with no thread rules has no threads: every match starts a turn, and a close has no thread to close.
// The same goes for a keyed trigger whose key came out empty, which must not tie all such messages to one thread:
// each is an investigation of its own, counted as empty_key, with a warning once for the trigger.
func TestMatchesThatHaveNoKeyEachStartATurn(t *testing.T) {
	emptyKey := botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
		tr.Extract = map[string]v1alpha1.Extract{
			"key":   {Field: v1alpha1.FieldTitle, Regex: `^(?P<key>)`}, // takes part, and matches nothing
			"state": {Field: v1alpha1.FieldTitle, Regex: `(?P<state>FIRING|RESOLVED)`},
		}
		tr.Thread = v1alpha1.TriggerThread{By: v1alpha1.ThreadByKey, Cooldown: metav1.Duration{Duration: time.Hour}, CloseWhen: map[string]string{"state": "RESOLVED"}}
	})
	for _, tc := range []struct {
		name     string
		tr       v1alpha1.Trigger
		decision string
	}{{"no thread rules", kindOnly(), "started"}, {"an empty key", emptyKey, "empty_key"}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTriggered(t, tc.tr)
			for _, id := range []string{"100.1", "101.1"} {
				if got := h.send(t, id, kindFiring); got != tc.decision {
					t.Fatalf("message %s: %q; want %q", id, got, tc.decision)
				}
			}
			if got := h.started(); !slices.Equal(got, []string{"100.1", "101.1"}) {
				t.Errorf("turns started for %v; want both", got)
			}
			if kinds, _ := h.noted(); len(kinds) != 0 {
				t.Errorf("notes %v; want none", kinds)
			}
			if tc.decision == "empty_key" { // the other has no close rule, so a resolve is a fire like the rest
				if got := h.send(t, "102.1", kindResolved); got != "close_unknown" {
					t.Errorf("a close of a key with no thread: %q; want close_unknown", got)
				}
			}
			if n := strings.Count(h.logs.String(), "extracted an empty key"); n != map[string]int{"started": 0, "empty_key": 1}[tc.decision] {
				t.Errorf("the empty-key warning was logged %d times; want once for the trigger, and never without a key", n)
			}
		})
	}
}

// A turn that could not start does not hold its key: no cooldown was recorded for it, so once the reservation is
// stale the key's next fire starts a turn of its own, and the cooldown runs from that one. A fire inside the grace
// is a note in the thread, which a turn that is starting would be; a close of a thread with no turn is unknown.
func TestATurnThatNeverStartedDoesNotHoldItsKey(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	h.hookErr = errors.New("prompt refused")
	if err := h.in.Message(h.ctx, botInbound("C1", "100.1", kindFiring)); !errors.Is(err, h.hookErr) {
		t.Fatalf("Message = %v; want the hook's error", err)
	}
	h.hookErr = nil // it is not redelivered
	h.clk.advance(10 * time.Second)
	if got := h.send(t, "101.1", kindFiring); got != "repeat" {
		t.Errorf("a fire inside the grace: %q; want repeat", got)
	}
	if got := h.send(t, "102.1", kindResolved); got != "close_unknown" {
		t.Errorf("a close of a thread with no turn: %q; want close_unknown", got)
	}
	h.clk.advance(2 * time.Minute)
	if got := h.send(t, "103.1", kindFiring); got != "started" {
		t.Fatalf("a fire past the grace: %q; want started", got)
	}
	if got := h.started(); !slices.Equal(got, []string{"100.1", "103.1"}) {
		t.Errorf("turns asked for %v; want the refused one and the one that took over", got)
	}
	h.clk.advance(time.Hour)
	if got := h.send(t, "104.1", kindFiring); got != "repeat" {
		t.Errorf("a fire inside the new turn's cooldown: %q; want repeat", got)
	}
	if _, threads := h.noted(); !slices.Equal(threads, []string{"100.1", "103.1"}) {
		t.Errorf("notes in %v; want the first inside the grace in the reserved thread, then one in the new", threads)
	}
	// The cooldown is the started turn's: it ends four hours after 103.1, at 16:02, and not after 100.1, at 16:00.
	h.send(t, "105.1", kindResolved)
	h.clk.advance(2*time.Hour + 59*time.Minute) // 16:01
	if got := h.send(t, "106.1", kindFiring); got != "repeat" {
		t.Errorf("a fire a minute before the cooldown of the turn that started: %q; want repeat", got)
	}
	h.send(t, "107.1", kindResolved)
	h.clk.advance(2 * time.Minute) // 16:03
	if got := h.send(t, "108.1", kindFiring); got != "started" {
		t.Errorf("a fire after that cooldown: %q; want started", got)
	}
}

// markFailsOnce is a TriggerThreads whose first MarkTriggerStarted fails.
type markFailsOnce struct {
	store.TriggerThreads
	failed bool
}

func (m *markFailsOnce) MarkTriggerStarted(ctx context.Context, binding, keyHash, thread string, until time.Time) error {
	if !m.failed {
		m.failed = true
		return errors.New("disk full")
	}
	return m.TriggerThreads.MarkTriggerStarted(ctx, binding, keyHash, thread, until)
}

// A turn asked for whose mark did not get written is asked for again by the redelivery, which is idempotent, and
// marked then.
func TestAFailedMarkIsMendedByTheRedelivery(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	cfg := h.cfg
	cfg.TriggerThreads = &markFailsOnce{TriggerThreads: h.st}
	h.in = chat.NewIntake(cfg)
	if err := h.in.Message(h.ctx, botInbound("C1", "100.1", kindFiring)); err == nil {
		t.Fatal("Message = nil; want the mark's error, so that Slack sends it again")
	}
	if got := h.send(t, "100.1", kindFiring); got != "dedup" {
		t.Errorf("redelivery: %q; want dedup", got)
	}
	if got := h.started(); !slices.Equal(got, []string{"100.1", "100.1"}) {
		t.Errorf("turns asked for %v; want the same anchor twice", got)
	}
	if got := h.send(t, "101.1", kindResolved); got != "closed" {
		t.Errorf("a close of the marked thread: %q; want closed", got)
	}
}

// A resolve that never registers (Agent Kourier was down past Slack's retry window) must not leave the key with a thread
// that only takes notes for ever: an open thread that has had no message of its key for maxIdle (24h unless the
// trigger says) is abandoned, and the next fire starts a new investigation. Every message of the key counts as
// life, so an alert that Alertmanager keeps repeating stays one thread.
func TestAThreadWhoseResolveWasLostIsAbandonedAfterMaxIdle(t *testing.T) {
	short := trialTrigger()
	short.Thread.MaxIdle = metav1.Duration{Duration: 8 * time.Hour}
	for _, tc := range []struct {
		name  string
		tr    v1alpha1.Trigger
		gaps  []time.Duration // the clock moves this far before each fire after the first
		wants []string
	}{
		{"the default of 24h, fires inside it", trialTrigger(), []time.Duration{4 * time.Hour, 23 * time.Hour, 23 * time.Hour}, []string{"repeat", "repeat", "repeat"}},
		{"the default, a fire after it", trialTrigger(), []time.Duration{20 * time.Hour, 25 * time.Hour, time.Hour}, []string{"repeat", "started", "repeat"}},
		{"exactly the default", trialTrigger(), []time.Duration{24 * time.Hour}, []string{"repeat"}},
		{"a maxIdle of 8h, a fire inside it", short, []time.Duration{7 * time.Hour}, []string{"repeat"}},
		{"a maxIdle of 8h, a fire after it", short, []time.Duration{9 * time.Hour}, []string{"started"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTriggered(t, tc.tr)
			if got := h.send(t, "100.1", kindFiring); got != "started" {
				t.Fatalf("the first fire: %q", got)
			}
			for i, gap := range tc.gaps {
				h.clk.advance(gap)
				if got := h.send(t, "10"+string(rune('1'+i))+".1", kindFiring); got != tc.wants[i] {
					t.Fatalf("fire %d, %v on: %q; want %q", i+2, gap, got, tc.wants[i])
				}
			}
			var starts int
			for _, w := range tc.wants {
				if w == "started" {
					starts++
				}
			}
			if got := len(h.started()); got != 1+starts {
				t.Errorf("turns started for %v; want %d", h.started(), 1+starts)
			}
		})
	}
}

// The new thread after an abandoned one is the key's thread: its notes go there, and the abandoned thread's late
// mark of a turn cannot take the key back.
func TestAnAbandonedThreadIsReplacedByTheNewOne(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	h.send(t, "100.1", kindFiring)
	h.clk.advance(30 * time.Hour)
	if got := h.send(t, "101.1", kindFiring); got != "started" {
		t.Fatalf("a fire after maxIdle: %q; want started", got)
	}
	h.clk.advance(time.Hour)
	if got := h.send(t, "102.1", kindFiring); got != "repeat" {
		t.Fatalf("the next fire: %q; want repeat", got)
	}
	if got := h.send(t, "103.1", kindResolved); got != "closed" {
		t.Fatalf("the resolve: %q; want closed", got)
	}
	if _, threads := h.noted(); !slices.Equal(threads, []string{"101.1", "101.1"}) {
		t.Errorf("notes in %v; want both in the new thread", threads)
	}
}

// A resolve that arrives before its fire (the fire's event failed and was redelivered after it) closes nothing,
// and the fire then opens a thread whose resolve is already spent: it is open until maxIdle takes it.
func TestAResolveBeforeItsFireIsLostAndMaxIdleMendsIt(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	for _, s := range []struct {
		after    time.Duration
		id       string
		msg      chat.BotMessage
		decision string
	}{
		{0, "100.1", kindResolved, "close_unknown"},
		{time.Minute, "101.1", kindFiring, "started"},
		{time.Hour, "102.1", kindFiring, "repeat"}, // the alert repeats, and the thread is open
		{23 * time.Hour, "103.1", kindFiring, "repeat"},
		{25 * time.Hour, "104.1", kindFiring, "started"}, // days later, a real firing
	} {
		h.clk.advance(s.after)
		if got := h.send(t, s.id, s.msg); got != s.decision {
			t.Fatalf("message %s: %q; want %q", s.id, got, s.decision)
		}
	}
}

// OnStart has a third outcome. ErrNotStarted, wrapped as a hook would, is an event that is acknowledged and not
// redelivered, with no cooldown, a thread left unmarked for the key's next fire to take over, and the decision
// not_started; the hook's own error text is not in the log.
func TestOnStartThatSaysNotStartedIsAcknowledgedAndHoldsNothing(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	h.hookErr = fmt.Errorf("prompt refused, value SECRET-CANARY: %w", chat.ErrNotStarted)
	if got := h.send(t, "100.1", kindFiring); got != "not_started" {
		t.Fatalf("the refused fire: %q; want not_started", got)
	}
	if got := h.send(t, "100.1", kindFiring); got != "dedup" || len(h.got()) != 1 {
		t.Errorf("the same event again: %q, hook asked %d times; want the Intake to remember it", got, len(h.got()))
	}
	if strings.Contains(h.logs.String(), "SECRET-CANARY") {
		t.Errorf("the hook's error text is in the log:\n%s", h.logs.String())
	}
	h.hookErr = nil
	h.clk.advance(10 * time.Second)
	if got := h.send(t, "101.1", kindResolved); got != "close_unknown" {
		t.Errorf("a close of a thread with no turn: %q; want close_unknown", got)
	}
	h.clk.advance(2 * time.Minute)
	if got := h.send(t, "102.1", kindFiring); got != "started" {
		t.Fatalf("the next fire, past the grace: %q; want started: no cooldown was begun", got)
	}
	if got, want := h.started(), []string{"100.1", "102.1"}; !slices.Equal(got, want) {
		t.Errorf("turns asked for %v; want %v", got, want)
	}
	if got := h.send(t, "103.1", kindFiring); got != "repeat" {
		t.Errorf("a fire after the one that started: %q; want repeat", got)
	}
}

// ErrNotStarted means the same for a match with no thread to mark.
func TestOnStartThatSaysNotStartedForAnUnthreadedMatch(t *testing.T) {
	h := newTriggered(t, kindOnly())
	h.hookErr = chat.ErrNotStarted
	if got := h.send(t, "100.1", kindFiring); got != "not_started" {
		t.Errorf("a refused fire: %q; want not_started", got)
	}
}

// The note comes before the change it goes with: a repeat on a closed thread whose notice cannot be queued leaves
// the thread closed, so that the next resolve is a close of nothing (close_unknown), not a second close, and the
// redelivery of the fire reopens it.
func TestAFailedNoticeOfARepeatLeavesAClosedThreadClosed(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	h.send(t, "100.1", kindFiring)
	if got := h.send(t, "101.1", kindResolved); got != "closed" {
		t.Fatalf("the resolve: %q", got)
	}
	h.notes.fail = []error{errors.New("outbox unavailable")}
	if err := h.in.Message(h.ctx, botInbound("C1", "102.1", kindFiring)); err == nil {
		t.Fatal("the fire again: Message = nil; want the error of the notice")
	}
	if got := h.send(t, "103.1", kindResolved); got != "close_unknown" {
		t.Errorf("a resolve while the thread is still closed: %q; want close_unknown: the failed repeat reopened nothing", got)
	}
	if got := h.send(t, "102.1", kindFiring); got != "repeat" {
		t.Errorf("the fire's redelivery: %q; want repeat", got)
	}
	if got := h.send(t, "104.1", kindResolved); got != "closed" {
		t.Errorf("a resolve of the reopened thread: %q; want closed", got)
	}
}

// A trigger that threads by key needs the store: with none, the Intake says so, loudly, rather than start a
// turn for every repeat of a flapping alert.
func TestAKeyedTriggerWithoutAStoreIsAnError(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	cfg := h.cfg
	cfg.TriggerThreads = nil
	err := chat.NewIntake(cfg).Message(h.ctx, botInbound("C1", "100.1", kindFiring))
	if err == nil || len(h.got()) != 0 {
		t.Errorf("Message = %v, turns %v; want an error and no turn", err, h.started())
	}
}

// With no Notice sink there is no note, and the thread's state is still kept.
func TestWithoutANoticeSinkTheThreadsAreStillKept(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	cfg := h.cfg
	cfg.Notice = nil
	h.in = chat.NewIntake(cfg)
	h.send(t, "100.1", kindFiring)
	if got := h.send(t, "101.1", kindFiring); got != "repeat" {
		t.Errorf("fire again: %q; want repeat", got)
	}
	if got := h.send(t, "102.1", kindResolved); got != "closed" {
		t.Errorf("resolve: %q; want closed", got)
	}
}

// The key is untrusted text from a bot's message, and so is the rest of it: none of it is in a note, in the logs
// or in the database. The logs name the key by a short hash.
func TestNoAlertTextIsInNotesLogsOrTheDatabase(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	for i, s := range []struct {
		msg chat.BotMessage
	}{{kindFiring}, {kindFiring}, {kindResolved}, {kindFiring}, {kindGroup}, {kindResolved}, {heartbeat}} {
		h.send(t, "10"+string(rune('0'+i))+".1", s.msg)
		h.clk.advance(time.Minute)
	}
	for _, n := range h.notes.got {
		if n.Detail == "" || strings.ContainsAny(n.Detail, "<>*_`") {
			t.Errorf("notice detail %q; want the time and nothing else", n.Detail)
		}
		for _, w := range alertWords {
			if strings.Contains(n.ID+n.Detail+string(n.Kind)+n.MessageRef, w) {
				t.Errorf("notice %+v holds %q", n, w)
			}
		}
	}
	logs := h.logs.String()
	for _, w := range alertWords {
		if strings.Contains(logs, w) {
			t.Errorf("the log holds %q:\n%s", w, logs)
		}
	}
	if !regexp.MustCompile(`"keyHash":"[0-9a-f]{8}"`).MatchString(logs) || !strings.Contains(logs, `"decision":"repeat"`) {
		t.Errorf("the log names no key hash or decision:\n%s", logs)
	}
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{h.dbPath, h.dbPath + "-wal"} {
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		for _, w := range alertWords {
			if strings.Contains(string(raw), w) {
				t.Errorf("%s holds %q", path, w)
			}
		}
	}
	h.reopenStore(t) // for the cleanup
}

// The notes go to Slack through the outbox, and once: the fire again and the resolve are each delivered to the
// Intake several times, by one Intake that remembers and by ones that do not, and each is one post in the
// thread.
func TestNotesArePostedThroughTheOutboxExactlyOnce(t *testing.T) {
	h := newTriggered(t, trialTrigger())
	fake := chattest.NewFake()
	ob, err := outbox.New(outbox.Config{Store: h.st, Platform: fake, OnAbandon: outbox.NoAbandon, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("outbox.New = %v", err)
	}
	sink := live.NewSink(nil, ob, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.cfg.Notice = sink.QueueNotice
	h.in = chat.NewIntake(h.cfg)

	h.send(t, "100.1", kindFiring)
	for range 3 {
		h.clk.advance(time.Minute)
		h.send(t, "101.1", kindFiring)
		h.in = chat.NewIntake(h.cfg) // the Intake that remembers was lost
	}
	h.clk.advance(time.Hour)
	for range 3 {
		h.send(t, "102.1", kindResolved)
		h.in = chat.NewIntake(h.cfg)
	}
	if err := ob.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	var texts, threads []string
	for _, m := range fake.Messages() {
		texts, threads = append(texts, m.Out.Message.Fallback), append(threads, m.Ref.Thread)
		if m.Ref.Channel != "C1" {
			t.Errorf("a note went to %s", m.Ref.Channel)
		}
	}
	// The fire again was first asked for at 12:01; the resolve at 13:03: the redeliveries are the first one's.
	want := []string{"Fired again at 2026-10-04 12:01 UTC.", "Resolved at 2026-10-04 13:03 UTC."}
	if !slices.Equal(texts, want) || !slices.Equal(threads, []string{"100.1", "100.1"}) {
		t.Errorf("posted %q in %v; want %q in the first thread, once each", texts, threads, want)
	}
	if got := h.started(); !slices.Equal(got, []string{"100.1"}) {
		t.Errorf("turns started for %v; want one", got)
	}
	if rows, _ := h.st.ListUnposted(h.ctx); len(rows) != 0 {
		t.Errorf("%d rows are still unposted", len(rows))
	}
}
