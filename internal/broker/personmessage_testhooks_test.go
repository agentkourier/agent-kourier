//go:build testhooks

package broker

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// personTriggerYAML is a personMessage trigger, put after the example Binding's mention trigger.
const personTriggerYAML = `      - type: personMessage
        exclude: { text: '^(?i:thanks|ok)\b' }
        limits:
          rateLimit: { maxRuns: 6, per: 30m, maxConcurrent: 2 }
          maxRunsPerDay: 50
        promptTemplate: |
          A person reported something in {{ .Channel }} ({{ .Source }}): {{ .Link }}.
`

// withPersonTrigger adds personTriggerYAML to the example Binding of the config in dir, whose threadReplies is all.
func withPersonTrigger(t *testing.T, dir string) {
	t.Helper()
	file := filepath.Join(dir, "binding.yaml")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	const mention = "      - type: mention\n"
	if !strings.Contains(string(raw), mention) || !strings.Contains(string(raw), "threadReplies: all") {
		t.Fatal("the fixture's binding.yaml has no mention trigger to add the personMessage trigger after, or is not threadReplies: all")
	}
	if err := os.WriteFile(file, []byte(strings.Replace(string(raw), mention, mention+personTriggerYAML, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The whole path of a person's message, through the real broker, on a Binding whose threadReplies is all: a person's message at
// the top level starts a turn of that person in its own thread. The agent sees the person's attribution in front of the
// template's output and what they wrote in the untrusted block, and the answer is streamed to them, so the update path's busy
// status is never set, and carries the rating buttons of an answer on a Binding with feedback.enabled and the line that a
// follow-up needs a mention. In that thread a reply without a mention is no turn, and the next mentioned reply carries it.
func TestRunAPersonsMessageIsTheirTurnAndItsThreadTakesOnlyMentions(t *testing.T) {
	t.Parallel()
	a := newAlerts(t)
	runsAlongside(&a.cfg)
	withPersonTrigger(t, a.cfg.ConfigDir)
	a.run(t)

	said, err := a.fake.Say(alertChannel, "U1", "checkout is down since the deploy")
	if err != nil {
		t.Fatal(err)
	}
	prompts := a.waitPrompts(t, 1)
	first := prompts[0]
	header, rest, _ := strings.Cut(first, "\n")
	if !strings.HasPrefix(header, "[Slack: ") || !strings.Contains(header, "(U1)") {
		t.Errorf("the first turn starts %q; want the person's attribution", header)
	}
	link := "https://fakeworkspace.slack.com/archives/" + alertChannel + "/p" + strings.Replace(said.TS, ".", "", 1)
	if want := "A person reported something in " + alertChannel + " (personMessage): " + link + "."; !strings.HasPrefix(rest, want) {
		t.Errorf("the first turn goes on %q; want the template's output %q", strings.SplitN(rest, "\n", 2)[0], want)
	}
	tag := regexp.MustCompile(`<untrusted-data-[0-9a-f]{12}>`).FindString(first)
	if before, inside, _ := strings.Cut(first, tag); tag == "" || strings.Contains(before, "since the deploy") || !strings.Contains(inside, "checkout is down since the deploy") {
		t.Errorf("what the person wrote is not in the untrusted block alone:\n%s", first)
	}
	a.inThread(t, said.TS, "the answer with rating buttons and the follow-up line", func(m fakeslack.Message) bool {
		return hasText("investigated")(m) && strings.Contains(string(m.Blocks), "feedback-useful") && strings.Contains(string(m.Blocks), "Mention me in this thread to ask a follow-up.")
	})
	for _, st := range a.fake.ThreadStatuses(alertChannel) {
		if st.ThreadTS == said.TS {
			t.Errorf("the thread was marked busy (%q): want the answer streamed to the person, which needs no status", st.Status)
		}
	}

	if _, err := a.fake.Reply(alertChannel, said.TS, "U2", "it started at 10:02"); err != nil {
		t.Fatal(err)
	}
	a.r.waitMetric(`agentkourier_thread_unmentioned_total{binding="sre/sre-alerts"} 1`)
	if got := a.agent.asked(); len(got) != 1 {
		t.Fatalf("a reply without a mention made %d turns; want none beyond the first", len(got)-1)
	}
	if _, err := a.fake.Reply(alertChannel, said.TS, "U1", "<@UFAKEBOT> what changed in the deploy?"); err != nil {
		t.Fatal(err)
	}
	prompts = a.waitPrompts(t, 2)
	if !strings.Contains(prompts[1], "what changed in the deploy?") || !strings.Contains(prompts[1], "it started at 10:02") {
		t.Errorf("the mentioned reply's turn is %q; want the reply and the unmentioned one before it", prompts[1])
	}
	a.agent.mu.Lock()
	priors := a.agent.priors[1]
	a.agent.mu.Unlock()
	if len(priors) != 1 || priors[0] != first {
		t.Errorf("the second turn's conversation had been asked %q; want the first prompt: one session", priors)
	}
	a.r.waitMetric(`agentkourier_trigger_decisions_total{binding="sre/sre-alerts",decision="started"} 1`)
}
