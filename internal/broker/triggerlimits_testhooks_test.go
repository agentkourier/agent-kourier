//go:build testhooks

package broker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// limitedBinding makes the example Binding's trigger allow two starts in 10 seconds, the shortest window there is, so
// that a storm is stopped after two and its digest comes when the window ends.
func (a *alerts) limitedBinding(t *testing.T) {
	t.Helper()
	file := filepath.Join(a.cfg.ConfigDir, "binding.yaml")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	const anchor = "rateLimit: { maxRuns: 5, per: 10m, maxConcurrent: 2 }"
	if !strings.Contains(string(raw), anchor) {
		t.Fatalf("the fixture's binding.yaml has no %q to replace", anchor)
	}
	limited := strings.Replace(string(raw), anchor, "rateLimit: { maxRuns: 2, per: 10s, maxConcurrent: 5 }", 1)
	if err := os.WriteFile(file, []byte(limited), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfg.digestEvery = 50 * time.Millisecond
}

// topLevel returns the messages Agent Kourier posted in the alert channel outside any thread.
func (a *alerts) topLevel() (out []fakeslack.Message) {
	for _, m := range a.fake.Messages(alertChannel) {
		if m.BotID == "BFAKEBOT" && m.ThreadTS == "" {
			out = append(out, m)
		}
	}
	return out
}

// A storm through the real broker: the first two alerts are investigated, the other three start nothing and get no
// note in their own thread, and when the window ends one line in the channel says how many were not investigated, once.
// The counters say the same. None of it names an alert.
func TestRunStopsAStormAtItsLimitsAndPostsOneDigest(t *testing.T) {
	a := newAlerts(t)
	a.limitedBinding(t)
	a.run(t)

	const alerts = 5
	var posted []fakeslack.UserMessage
	for i := range alerts {
		posted = append(posted, a.fire(t, post("FIRING", fmt.Sprintf("KubePodCrashLooping%d", i))))
	}
	a.waitPrompts(t, 2)
	for _, m := range posted[:2] {
		a.inThread(t, m.TS, "the agent's answer", hasText("investigated"))
	}
	a.r.waitMetric(`agentkourier_trigger_decisions_total{binding="sre/sre-alerts",decision="limited"} 3`)
	a.r.waitMetric(`agentkourier_trigger_decisions_total{binding="sre/sre-alerts",decision="started"} 2`)
	a.r.waitMetric(`agentkourier_trigger_limited_total{binding="sre/sre-alerts",limit="rate",trigger="1"} 3`)
	settle()
	if got := a.agent.asked(); len(got) != 2 {
		t.Errorf("the agent was asked %d times for a storm of %d alerts with two places; want 2", len(got), alerts)
	}
	for _, m := range posted[2:] {
		if reply, ok := a.threadOf(m.TS); ok {
			t.Errorf("a limited alert has a reply in its thread: %q; want nothing, the digest speaks for it", reply.Text)
		}
	}
	if got := a.topLevel(); len(got) != 0 {
		t.Fatalf("Agent Kourier posted %+v in the channel before the window ended; want the digest only then", got)
	}

	// The window ends ten seconds after the first limited alert. One line says so, and no other is posted.
	const want = "3 alert messages not investigated in the last 10s (rate limit)."
	var line fakeslack.Message
	for deadline := time.Now().Add(30 * time.Second); line.TS == ""; time.Sleep(50 * time.Millisecond) {
		for _, m := range a.topLevel() {
			line = m
		}
		if time.Now().After(deadline) {
			t.Fatalf("no digest in the channel: %+v", a.fake.Messages(alertChannel))
		}
	}
	if !strings.Contains(line.Text, want) {
		t.Errorf("the digest reads %q; want %q", line.Text, want)
	}
	time.Sleep(time.Second) // a second line would be posted within a few passes
	if got := a.topLevel(); len(got) != 1 {
		t.Errorf("Agent Kourier posted %d lines in the channel; want the one digest: %+v", len(got), got)
	}
	for _, w := range append(alertWords, "KubePodCrashLooping0") {
		if strings.Contains(line.Text, w) || strings.Contains(a.logs.String(), w) {
			t.Errorf("the digest or a log line holds %q of an alert", w)
		}
	}
}

// The digest of a window that ended while Agent Kourier was down is posted once it is up: the windows and their counts are in
// the database, not in memory.
func TestRunPostsTheDigestOfAWindowThatEndedDuringARestart(t *testing.T) {
	a := newAlerts(t)
	a.limitedBinding(t)
	a.run(t)
	for i := range 3 {
		a.fire(t, post("FIRING", fmt.Sprintf("KubePodCrashLooping%d", i)))
	}
	a.waitPrompts(t, 2)
	a.r.waitMetric(`agentkourier_trigger_limited_total{binding="sre/sre-alerts",limit="rate",trigger="1"} 1`)
	a.r.cancel()
	if err := a.r.wait(); err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Second) // the window ends while the broker is down
	a.cfg.Listener = valid(t).Listener
	a.run(t)
	for deadline := time.Now().Add(30 * time.Second); len(a.topLevel()) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no digest after the restart: %+v", a.fake.Messages(alertChannel))
		}
	}
	if got := a.topLevel(); len(got) != 1 || !strings.Contains(got[0].Text, "1 alert message not investigated in the last 10s (rate limit).") {
		t.Errorf("channel holds %+v; want the one digest", got)
	}
}
