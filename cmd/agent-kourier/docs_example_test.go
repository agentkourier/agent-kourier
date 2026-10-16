package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// alertmanagerHowTo is the page whose YAML example is the Alertmanager trigger, written in full.
const alertmanagerHowTo = "../../docs/how-to/investigate-alertmanager-alerts.md"

// howToBinding is the sandbox dry-run Binding with its chat block replaced by the page's first YAML example under
// "Add the trigger", which is a chat block, so that the example is loaded as a user would load it.
func howToBinding(t *testing.T) string {
	t.Helper()
	page, err := os.ReadFile(alertmanagerHowTo)
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(page), "\n## Add the trigger\n")
	if !ok {
		t.Fatalf("%s has no \"Add the trigger\" section", alertmanagerHowTo)
	}
	_, block, ok := strings.Cut(after, "\n```yaml\n")
	if !ok {
		t.Fatalf("%s: no YAML example under \"Add the trigger\"", alertmanagerHowTo)
	}
	example, _, ok := strings.Cut(block, "\n```\n")
	if !ok {
		t.Fatalf("%s: the YAML example is not closed", alertmanagerHowTo)
	}
	fixture, err := os.ReadFile(filepath.Join(dryRunConfig, "trial.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	head, _, ok := strings.Cut(string(fixture), "\n  chat:\n")
	if !ok {
		t.Fatal("the dry-run fixture has no chat block")
	}
	return head + "\n" + example + "\n"
}

// The how-to's example loads with the broker's loader, the way `agent-kourier dry-run` loads a Binding, and its
// botMessage trigger is the Alertmanager trigger the tests of the matcher use (chattest.AlertmanagerTrigger) with the
// optional receiver match: the page, the tests and the kind fixtures cannot drift apart without this failing.
func TestTheAlertmanagerHowToExampleLoadsAndIsTheTriggerTheTestsUse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "binding.yaml"), []byte(howToBinding(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := config.Load(t.Context(), dir)
	if err != nil {
		t.Fatalf("the how-to's example does not load: %v", err)
	}
	if len(set.Bindings) != 1 {
		t.Fatalf("loaded %d Bindings; want 1", len(set.Bindings))
	}
	ts := set.Bindings[0].Spec.Chat.Triggers
	if len(ts) != 2 || ts[0].Type != v1alpha1.TriggerMention || ts[1].Type != v1alpha1.TriggerBotMessage {
		t.Fatalf("triggers %+v; want a mention trigger and a botMessage trigger", ts)
	}
	want := chattest.AlertmanagerTrigger("B0123456789")
	want.Match[v1alpha1.FieldTitleLink] = `receiver=slack-critical$`
	if !reflect.DeepEqual(ts[1], want) {
		t.Errorf("the example's botMessage trigger differs from chattest.AlertmanagerTrigger:\n got  %+v\n want %+v", ts[1], want)
	}
}

// Replayed over the owner's channel, the how-to's example takes production's alerts and nothing else: the receiver is
// anchored, and the title and the fallback turn the heartbeat and Grafana away. The summary is the one the production
// dry-run Binding gets in TestDryRunReplaysTheOwnersChannelThroughTheTrialAndProduction.
func TestTheAlertmanagerHowToExampleTakesProductionAlerts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "binding.yaml"), []byte(howToBinding(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	fake, _ := ownersChannel(t)
	stdout, stderr, code := dry(t, fake, "--config", dir, "--binding", "agent-kourier/trial", "--json")
	if code != exitOK {
		t.Fatalf("exit code %d; stderr %q", code, stderr)
	}
	var r dryRunReport
	if err := json.Unmarshal([]byte(stdout), &r); err != nil {
		t.Fatalf("the output is no JSON report: %v\n%s", err, stdout)
	}
	want := dryRunSummary{Messages: 13,
		Outcomes: map[string]int{"matched": 2, "unmatched": 8, "person": 1, "own": 1, "other_bot": 1},
		Triggers: map[string]int{"1": 2},
		Keys:     []dryRunKey{{"KubeAPIErrorBudgetBurn kube-system", 2}}}
	if !summaryEqual(r.Summary, want) {
		t.Errorf("summary = %+v;\nwant %+v", r.Summary, want)
	}
	for _, m := range r.Messages {
		if m.Outcome == "matched" && (m.Fields["alertname"] != "KubeAPIErrorBudgetBurn" || m.Fields["key"] != "KubeAPIErrorBudgetBurn kube-system") {
			t.Errorf("a matched message extracted %v; want the production alert's name and key", m.Fields)
		}
	}
}
