//go:build testhooks

package broker

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// The whole path of the thread context (spec 8.2, D38), through the real broker: in a Binding with chat.threadReplies:
// mention, the replies that people wrote without mentioning courier start no turn, and the next mention carries them to the
// agent after its own text, in the block of untrusted data, read from Slack's thread (the fake), without courier's own
// answer, the mention that was a turn, or the mention itself. A mention with nothing unmentioned before it carries no block.
func TestRunCarriesTheUnmentionedRepliesToTheNextMentionedTurn(t *testing.T) {
	w := newSlackWorld(t, "xoxb-fake-bot")
	w.cfg.sink = nil // the live renderer, so that courier's own answers are in the thread, to be left out of the context
	file := filepath.Join(w.cfg.ConfigDir, "binding.yaml")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	const all = "    threadReplies: all               # all | mention; see 8.2\n"
	if !strings.Contains(string(raw), all) {
		t.Fatal("the fixture's binding.yaml has no `threadReplies: all` line to change")
	}
	if err := os.WriteFile(file, []byte(strings.Replace(string(raw), all, "    threadReplies: mention\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var prompts []string
	w.stub.Agent("kagent", "k8s-agent").Reply(func(prompt string) []stubagent.Step {
		mu.Lock()
		prompts = append(prompts, prompt)
		mu.Unlock()
		return []stubagent.Step{stubagent.Text("the answer")}
	})
	sent := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), prompts...)
	}
	waitPrompts := func(n int) []string {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if got := sent(); len(got) >= n {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("the agent got %d turns, want %d: %q", len(sent()), n, sent())
			}
		}
	}
	answered := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			count := 0
			for _, m := range w.fake.Messages("C0123456789") {
				if m.BotID != "" && strings.Contains(m.Text, "the answer") && !m.Streaming {
					count++
				}
			}
			if count >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("courier posted fewer than %d answers", n)
			}
		}
	}
	w.fake.AddUser(fakeslack.User{ID: "U1", Name: "ada"})
	w.fake.AddUser(fakeslack.User{ID: "U2", Name: "grace"})
	w.fake.SetMembers("C0123456789", "U1", "U2")
	r := start(t, w.cfg)
	r.until("/readyz", http.StatusOK)
	waitConnected(t, w.fake)

	first, err := w.fake.Mention("C0123456789", "U1", "<@UFAKEBOT> why is it down?")
	if err != nil {
		t.Fatal(err)
	}
	waitPrompts(1)
	answered(1)
	for _, c := range []struct{ user, text string }{{"U2", "I think it is the node"}, {"U1", "no, the pod restarted"}} {
		if _, err := w.fake.Reply("C0123456789", first.TS, c.user, c.text); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.fake.Reply("C0123456789", first.TS, "U1", "<@UFAKEBOT> and now?"); err != nil {
		t.Fatal(err)
	}
	got := waitPrompts(2)
	answered(2)

	if strings.Contains(got[0], "thread-replies") {
		t.Errorf("the first turn of a thread carries a block: %q", got[0])
	}
	second := got[1]
	block := strings.Index(second, "\n<thread-replies-") // on a line of its own: the sentence before it names the tag too
	if block < 0 || strings.Index(second, "and now?") > block || strings.Count(second, "\n</thread-replies-") != 1 {
		t.Fatalf("the second turn lacks a block after its own text:\n%s", second)
	}
	for _, want := range []string{
		"<reply><from>grace (U2)</from><text>I think it is the node</text></reply>",
		"<reply><from>ada (U1)</from><text>no, the pod restarted</text></reply>",
	} {
		if !strings.Contains(second[block:], want) {
			t.Errorf("the block lacks %s:\n%s", want, second)
		}
	}
	if strings.Index(second, "grace (U2)") > strings.Index(second, "no, the pod restarted") {
		t.Errorf("the block is not oldest first:\n%s", second)
	}
	for _, out := range []string{"why is it down?", "the answer", "and now?"} {
		if strings.Contains(second[block:], out) {
			t.Errorf("the block holds %q, which is not an unmentioned reply of a person:\n%s", out, second)
		}
	}
	r.waitMetric(`courier_thread_context_total{binding="sre/sre-alerts",outcome="read"} 1`)

	// A mention with nothing unmentioned before it: the window holds courier's own answer alone.
	if _, err := w.fake.Reply("C0123456789", first.TS, "U2", "<@UFAKEBOT> anything else?"); err != nil {
		t.Fatal(err)
	}
	third := waitPrompts(3)[2]
	if strings.Contains(third, "thread-replies") {
		t.Errorf("a mention with no reply before it carries a block:\n%s", third)
	}
	r.waitMetric(`courier_thread_context_total{binding="sre/sre-alerts",outcome="empty"} 1`)
}
