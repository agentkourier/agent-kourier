package chat_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// storm is the word every alert of these tests carries in its name, its labels and its detail, so that a test can
// say that none of it reached a log line, a note or a metric.
const storm = "KubeSecretBurn"

// alertN is the n-th distinct alert of a storm, firing: its own alert name, so its own key. state is FIRING or
// RESOLVED.
func alertN(n int, state string) chat.BotMessage {
	name := fmt.Sprintf("%s%d", storm, n)
	if state == "RESOLVED" {
		return chat.BotMessage{BotID: webhookBot, Title: "[kind] RESOLVED - " + name, TitleLink: kindLink,
			Fallback: "[RESOLVED] " + name + " shop (critical) | " + kindLink}
	}
	return chat.BotMessage{BotID: webhookBot, Title: "[kind] FIRING (1) - " + name, TitleLink: kindLink,
		Fallback:       "[FIRING:1] " + name + " shop (critical) | " + kindLink,
		AttachmentText: "*" + name + "* - critical\nPod " + storm + "-pod is down.\n"}
}

// newLimited is an Intake over the test's database with its limits enforced, as the broker makes it.
func newLimited(t *testing.T, triggers ...v1alpha1.Trigger) *triggered {
	t.Helper()
	h := newTriggered(t, triggers...)
	h.cfg.TriggerRuns = h.st
	h.in = chat.NewIntake(h.cfg)
	return h
}

// restartLimited is a restart of Agent Kourier: the database is closed and opened again, and a new Intake, which remembers no
// message and no count, is made over it.
func (h *triggered) restartLimited(t *testing.T) {
	t.Helper()
	h.reopenStore(t)
	h.cfg.TriggerThreads, h.cfg.TriggerRuns = h.st, h.st
	h.in = chat.NewIntake(h.cfg)
}

// limitedCount reads agentkourier_trigger_limited_total of the test's Binding and trigger index, by limit.
func (h *triggered) limitedCount(t *testing.T, trigger string) map[string]float64 {
	t.Helper()
	families, err := h.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "agentkourier_trigger_limited_total" {
			continue
		}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if len(labels) != 3 {
				t.Errorf("agentkourier_trigger_limited_total has labels %v; want binding, trigger and limit only", labels)
			}
			if labels["binding"] == bindingName && labels["trigger"] == trigger {
				out[labels["limit"]] += m.Counter.GetValue()
			}
		}
	}
	return out
}

// fire delivers the n-th alert, firing, as message ts, and returns the decision it was counted as.
func (h *triggered) fire(t *testing.T, n int, ts string) string {
	t.Helper()
	return h.send(t, ts, alertN(n, "FIRING"))
}

// beSilent requires that nothing of the alerts' text reached a log line, a note or a metric of the test.
func (h *triggered) beSilent(t *testing.T) {
	t.Helper()
	if logs := h.logs.String(); strings.Contains(logs, storm) {
		t.Errorf("the alerts' text reached the log:\n%s", logs)
	}
	h.notes.mu.Lock()
	defer h.notes.mu.Unlock()
	for _, n := range h.notes.got {
		if strings.Contains(n.Detail, storm) || strings.Contains(n.ID, storm) {
			t.Errorf("the alerts' text reached a notice: %+v", n)
		}
	}
	families, err := h.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, m := range f.Metric {
			for _, l := range m.Label {
				if strings.Contains(l.GetValue(), storm) {
					t.Errorf("the alerts' text reached the label %s=%q of %s", l.GetName(), l.GetValue(), f.GetName())
				}
			}
		}
	}
}

// running makes the investigation of the alert message ts have an active task, as the session manager does once
// the agent has taken the turn.
func (h *triggered) running(t *testing.T, ts string) {
	t.Helper()
	key := store.ConversationKey{Connection: conn, Channel: "C1", Thread: ts}
	if err := h.st.UpsertSession(t.Context(), store.Session{Key: key, Binding: bindingName, TaskID: "task-" + ts, LastActivity: h.clk.Now()}); err != nil {
		t.Fatal(err)
	}
}

func (h *triggered) digests(t *testing.T) []store.TriggerDigest {
	t.Helper()
	ds, err := h.st.ListTriggerDigests(t.Context(), h.clk.Now().Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// A storm of distinct alerts with the Alertmanager trigger's limits: five start in the window, the rest are limited, nothing is
// posted in any alert's thread, and what was not investigated is counted for one digest. Once the window has gone the
// same alerts, firing again, start: a limited start held no key.
func TestAStormOfDistinctAlertsStartsOnlyTheRateLimit(t *testing.T) {
	h := newLimited(t, trialTrigger())
	const alerts = 8
	var decisions []string
	for n := range alerts {
		decisions = append(decisions, h.fire(t, n, fmt.Sprintf("%d.1", 100+n)))
		h.clk.advance(time.Second)
	}
	want := []string{"started", "started", "started", "started", "started", "limited", "limited", "limited"}
	if !slices.Equal(decisions, want) {
		t.Fatalf("decisions = %v; want %v", decisions, want)
	}
	if got := h.started(); len(got) != 5 {
		t.Errorf("turns started for %v; want the first five only", got)
	}
	if kinds, _ := h.noted(); len(kinds) != 0 {
		t.Errorf("notes %v were posted; want none for a limited alert, or for a start", kinds)
	}
	if got := h.limitedCount(t, "0"); len(got) != 1 || got["rate"] != 3 {
		t.Errorf("agentkourier_trigger_limited_total = %v; want 3 for rate", got)
	}
	ds := h.digests(t)
	if len(ds) != 1 || ds[0].Counts[store.LimitRate] != 3 || len(ds[0].Counts) != 1 || ds[0].Binding != bindingName || ds[0].Channel != "C1" || ds[0].Connection != conn {
		t.Errorf("digests = %+v; want one window counting 3 for rate in the Binding's channel", ds)
	}

	// The window has passed: the alerts that were limited fire again (new messages, same keys), and they start. A
	// limited start held no key, so these are not repeats.
	h.clk.advance(11 * time.Minute)
	for n := 5; n < alerts; n++ {
		if got := h.fire(t, n, fmt.Sprintf("%d.1", 200+n)); got != "started" {
			t.Errorf("alert %d fired again after the window: %q; want started, since the limited start held no key", n, got)
		}
	}
	h.beSilent(t)
}

// Alerts that arrive at once are counted one by one against the window, and the window slides: places come back as the
// first starts leave it.
func TestTheRateWindowSlides(t *testing.T) {
	h := newLimited(t, trialTrigger())
	for n := range 5 {
		if got := h.fire(t, n, fmt.Sprintf("%d.1", 100+n)); got != "started" {
			t.Fatalf("alert %d: %q; want started", n, got)
		}
		h.clk.advance(time.Minute) // starts at 0, 1, 2, 3, 4 minutes
	}
	if got := h.fire(t, 5, "105.1"); got != "limited" { // at minute 5
		t.Fatalf("a sixth in the window: %q; want limited", got)
	}
	h.clk.advance(5*time.Minute + time.Second) // minute 10:00:01, so only the first start has left
	if got := h.fire(t, 6, "106.1"); got != "started" {
		t.Errorf("a start when one place has come back: %q; want started", got)
	}
	if got := h.fire(t, 7, "107.1"); got != "limited" {
		t.Errorf("another: %q; want limited, the rest are inside the window still", got)
	}
}

// Concurrency is the investigations running now, counted from the sessions in the database: with two running a third
// is limited, and when one ends a new fire starts.
func TestMaxConcurrentCountsRunningInvestigations(t *testing.T) {
	h := newLimited(t, trialTrigger())
	for n := range 2 {
		ts := fmt.Sprintf("%d.1", 100+n)
		if got := h.fire(t, n, ts); got != "started" {
			t.Fatalf("alert %d: %q; want started", n, got)
		}
		h.running(t, ts)
	}
	if got := h.fire(t, 2, "102.1"); got != "limited" {
		t.Fatalf("a third with two running: %q; want limited", got)
	}
	if got := h.limitedCount(t, "0"); got["concurrent"] != 1 || len(got) != 1 {
		t.Errorf("agentkourier_trigger_limited_total = %v; want 1 for concurrent", got)
	}
	// The first investigation ends: the manager clears its task.
	if err := h.st.SetSessionTask(t.Context(), store.ConversationKey{Connection: conn, Channel: "C1", Thread: "100.1"}, "", "", store.TaskTurn{}); err != nil {
		t.Fatal(err)
	}
	h.clk.advance(time.Second)
	if got := h.fire(t, 2, "103.1"); got != "started" {
		t.Fatalf("the limited alert firing again after one investigation ended: %q; want started", got)
	}
	h.running(t, "103.1")
	if got := h.fire(t, 3, "104.1"); got != "limited" {
		t.Errorf("a fourth with two running again: %q; want limited", got)
	}
}

// The daily cap holds across any rate window, and slides over 24 hours.
func TestMaxRunsPerDayCapsTheDay(t *testing.T) {
	tr := trialTrigger()
	tr.Limits = v1alpha1.TriggerLimits{
		RateLimit:     &v1alpha1.RateLimit{MaxRuns: 100, Per: metav1.Duration{Duration: time.Minute}, MaxConcurrent: 100},
		MaxRunsPerDay: new(int32(3)),
	}
	h := newLimited(t, tr)
	var decisions []string
	for n := range 5 {
		decisions = append(decisions, h.fire(t, n, fmt.Sprintf("%d.1", 100+n)))
		h.clk.advance(2 * time.Hour)
	}
	if want := []string{"started", "started", "started", "limited", "limited"}; !slices.Equal(decisions, want) {
		t.Fatalf("decisions = %v; want %v", decisions, want)
	}
	if got := h.limitedCount(t, "0"); got["daily"] != 2 || len(got) != 1 {
		t.Errorf("agentkourier_trigger_limited_total = %v; want 2 for daily", got)
	}
	h.clk.advance(24 * time.Hour)
	if got := h.fire(t, 9, "109.1"); got != "started" {
		t.Errorf("a start a day later: %q; want started", got)
	}
}

// The counts are the store's, so they hold across a restart: an Intake that remembers nothing finds the window full.
func TestLimitsSurviveARestart(t *testing.T) {
	h := newLimited(t, trialTrigger())
	for n := range 5 {
		if got := h.fire(t, n, fmt.Sprintf("%d.1", 100+n)); got != "started" {
			t.Fatalf("alert %d: %q; want started", n, got)
		}
	}
	h.restartLimited(t)
	h.clk.advance(time.Minute)
	if got := h.fire(t, 5, "105.1"); got != "limited" {
		t.Errorf("a sixth start after a restart: %q; want limited", got)
	}
	// The running investigations survive too: they are sessions in the database.
	h2 := newLimited(t, trialTrigger())
	h2.fire(t, 0, "100.1")
	h2.running(t, "100.1")
	h2.fire(t, 1, "101.1")
	h2.running(t, "101.1")
	h2.restartLimited(t)
	if got := h2.fire(t, 2, "102.1"); got != "limited" {
		t.Errorf("a third start after a restart, with two running: %q; want limited", got)
	}
}

// Only a start is limited. With the window full, a repeat of an alert that is investigated is a note in its thread, its
// resolve is a note, and a fire after the close is a repeat again: the limits do not touch them, and count nothing.
func TestRepeatsAndClosesAreNeverLimited(t *testing.T) {
	h := newLimited(t, trialTrigger())
	for n := range 5 {
		if got := h.fire(t, n, fmt.Sprintf("%d.1", 100+n)); got != "started" {
			t.Fatalf("alert %d: %q; want started", n, got)
		}
	}
	if got := h.fire(t, 6, "106.1"); got != "limited" {
		t.Fatalf("the sixth alert: %q; want limited", got)
	}
	steps := []struct {
		name  string
		state string
		ts    string
		want  string
	}{
		{"a repeat of a started alert", "FIRING", "110.1", "repeat"},
		{"its resolve", "RESOLVED", "111.1", "closed"},
		{"it fires again within the cooldown", "FIRING", "112.1", "repeat"},
		{"a resolve of an alert that was never started", "RESOLVED", "113.1", "close_unknown"},
	}
	for i, s := range steps {
		n := 0
		if i == len(steps)-1 {
			n = 6 // the limited alert: nothing to close
		}
		if got := h.send(t, s.ts, alertN(n, s.state)); got != s.want {
			t.Errorf("%s: %q; want %q", s.name, got, s.want)
		}
	}
	if kinds, _ := h.noted(); len(kinds) != 3 {
		t.Errorf("notes %v; want the three that a repeat, a close and a repeat make", kinds)
	}
	if got := h.limitedCount(t, "0"); got["rate"] != 1 || len(got) != 1 {
		t.Errorf("agentkourier_trigger_limited_total = %v; want only the one limited start", got)
	}
	if ds := h.digests(t); len(ds) != 1 || ds[0].Counts[store.LimitRate] != 1 {
		t.Errorf("digests = %+v; want one counting the limited start only", ds)
	}
}

// A start that the prompt check refused still counts against the rate and the daily cap: the refusal posts a note of its
// own, so a template that stops fitting during a storm must be held to the limits, not note every alert. The sixth
// refusal in a window is limited, silent, and never reaches the hook that would note it.
func TestARefusedStartCountsAgainstTheLimits(t *testing.T) {
	h := newLimited(t, trialTrigger())
	h.hookErr = chat.ErrNotStarted
	var decisions []string
	for n := range 8 {
		decisions = append(decisions, h.fire(t, n, fmt.Sprintf("%d.1", 100+n)))
		h.clk.advance(time.Second)
	}
	want := []string{"not_started", "not_started", "not_started", "not_started", "not_started", "limited", "limited", "limited"}
	if !slices.Equal(decisions, want) {
		t.Errorf("decisions = %v; want %v", decisions, want)
	}
	if got := h.started(); len(got) != 5 {
		t.Errorf("the start hook, which notes a refusal, was asked for %v; want the first five only", got)
	}
	if ds := h.digests(t); len(ds) != 1 || ds[0].Counts[store.LimitRate] != 3 {
		t.Errorf("digests = %+v; want the three limited refusals in one window", ds)
	}
	// A refusal is no running investigation: with only refusals, no concurrency place is used.
	tr := trialTrigger()
	tr.Limits = v1alpha1.TriggerLimits{RateLimit: &v1alpha1.RateLimit{MaxRuns: 100, Per: metav1.Duration{Duration: time.Hour}, MaxConcurrent: 1}, MaxRunsPerDay: new(int32(40))}
	h = newLimited(t, tr)
	h.hookErr = chat.ErrNotStarted
	for n := range 3 {
		if got := h.fire(t, n, fmt.Sprintf("%d.1", 100+n)); got != "not_started" {
			t.Errorf("refusal %d: %q; want not_started, since a refusal takes no concurrency place", n, got)
		}
	}
}

// A start that failed for a reason that will pass and is asked for again counts once, not once for each ask.
func TestARetriedStartCountsOnce(t *testing.T) {
	h := newLimited(t, trialTrigger())
	h.hookErr = errors.New("the queue is down")
	if err := h.in.Message(h.ctx, botInbound("C1", "200.1", alertN(10, "FIRING"))); err == nil {
		t.Fatal("a start that failed returned no error; want the event redelivered")
	}
	h.hookErr = nil
	if got := h.send(t, "200.1", alertN(10, "FIRING")); got != "dedup" { // the redelivery of the start that failed
		t.Fatalf("the redelivery: %q; want dedup", got)
	}
	var decisions []string
	for n := 11; n < 16; n++ { // the first of the five places was the retried start
		decisions = append(decisions, h.fire(t, n, fmt.Sprintf("%d.1", 200+n)))
	}
	if want := []string{"started", "started", "started", "started", "limited"}; !slices.Equal(decisions, want) {
		t.Errorf("decisions after the retried start = %v; want %v: it counted once", decisions, want)
	}
}

// A trigger with no thread rules starts a turn for every match, and its limits hold all the same.
func TestAnUnthreadedTriggerIsLimitedToo(t *testing.T) {
	tr := botTrigger(webhookBot, func(tr *v1alpha1.Trigger) {
		tr.Limits = v1alpha1.TriggerLimits{RateLimit: &v1alpha1.RateLimit{MaxRuns: 2, Per: metav1.Duration{Duration: time.Hour}, MaxConcurrent: 5}, MaxRunsPerDay: new(int32(0))}
	})
	h := newLimited(t, tr)
	var decisions []string
	for n := range 4 {
		decisions = append(decisions, h.fire(t, n, fmt.Sprintf("%d.1", 100+n)))
	}
	if want := []string{"started", "started", "limited", "limited"}; !slices.Equal(decisions, want) {
		t.Errorf("decisions = %v; want %v", decisions, want)
	}
	if got := h.started(); len(got) != 2 {
		t.Errorf("turns started for %v; want two", got)
	}
}

// A trigger whose limits nothing reaches starts everything, and so does an Intake that was given no store of
// starts.
func TestLimitsNothingReachesLetEveryStartGoAhead(t *testing.T) {
	for name, h := range map[string]*triggered{
		"limits nothing reaches": newLimited(t, botTrigger(webhookBot, nil)),
		"no store of starts":     newTriggered(t, trialTrigger()),
	} {
		for n := range 8 {
			if got := h.fire(t, n, fmt.Sprintf("%d.1", 100+n)); got != "started" {
				t.Errorf("%s: alert %d: %q; want started", name, n, got)
			}
		}
	}
}

// A message delivered again after it was limited is limited again and counted once for the digest: the digest says
// how many alerts were not investigated, not how often Slack said so.
func TestALimitedMessageDeliveredAgainIsOneInTheDigest(t *testing.T) {
	h := newLimited(t, trialTrigger())
	for n := range 5 {
		h.fire(t, n, fmt.Sprintf("%d.1", 100+n))
	}
	if got := h.fire(t, 5, "105.1"); got != "limited" {
		t.Fatalf("the sixth: %q; want limited", got)
	}
	h.restartLimited(t) // an Intake that remembers nothing is told again
	if got := h.fire(t, 5, "105.1"); got != "limited" {
		t.Fatalf("the sixth again: %q; want limited", got)
	}
	if ds := h.digests(t); len(ds) != 1 || ds[0].Counts[store.LimitRate] != 1 {
		t.Errorf("digests = %+v; want one counting 1", ds)
	}
}
