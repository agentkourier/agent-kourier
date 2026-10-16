package kindmanifests_test

import (
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// These tests pin the alert trial's Binding (hack/kind/sandbox/agent-kourier/alert-trial): the second Binding of the sandbox's Agent Kourier, which
// watches the real alert channel and investigates the [kind] alerts of kind's Alertmanager, and nothing else. A Binding that
// strays (a mention trigger, a looser receiver, another bot, a question to people) acts in the production channel, so each rule is a test.
// Every test builds the config with up.sh's own render_agentkourier_config (env.sh), not a copy of its steps.

const (
	// The alert channel and the incoming webhook's bot, as the trial is meant to be pointed at them (chat-triggers.md, "The trial channel").
	trialChannel = "C0123456789"
	trialBot     = "B0123456789"
	// trialReceiver is the one receiver the trial takes: kind's copy of the owner's, anchored so that slack-critical (production) does not match.
	trialReceiver = `receiver=slack-critical-kind$`
)

// trialConfig renders the sandbox's Agent Kourier config as up.sh does, with the alert source on ("1") or off ("0"), and returns the directory.
func trialConfig(t *testing.T, alertSource string) string {
	t.Helper()
	sbx, err := filepath.Abs(strings.TrimSuffix(sandboxDir, "/"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := `set -eu
. "$1/env.sh"
. "$1/webhook.sh"
SBX=$1
HERE=$(dirname "$1")
MODEL_NAME=claude-fake-model
SLACK_CHANNEL_ID=C0SANDBOX01
ALERT_SOURCE=$2
render_agentkourier_config "$3"`
	var out bytes.Buffer
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", sbx, alertSource, dir)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("render_agentkourier_config: %v\n%s", err, out.String())
	}
	return dir
}

// trialBinding is the alert trial's Binding as the real loader gives it from the rendered config, with the config it came from.
func trialBinding(t *testing.T) (*v1alpha1.Binding, *config.Set) {
	t.Helper()
	set, err := config.Load(t.Context(), trialConfig(t, "1"))
	if err != nil {
		t.Fatalf("the sandbox config with the alert trial does not load: %v", err)
	}
	for i := range set.Bindings {
		if set.Bindings[i].Name == "alert-trial" {
			return &set.Bindings[i], set
		}
	}
	t.Fatalf("the config holds no Binding alert-trial: %+v", set.Bindings)
	return nil, nil
}

// TestTrialBindingLoadsThroughTheRealLoader: with the alert source on, the config holds the dogfood Binding and the trial's, both loadable and
// resolvable against the one connection, each on its own channel; the trial's channel is the alert channel.
func TestTrialBindingLoadsThroughTheRealLoader(t *testing.T) {
	b, set := trialBinding(t)
	if len(set.Bindings) != 2 || len(set.ChatConnections) != 1 || len(set.AgentBackends) != 1 {
		t.Fatalf("Load = %d Bindings, %d connections, %d backends; want 2, 1, 1", len(set.Bindings), len(set.ChatConnections), len(set.AgentBackends))
	}
	if b.Namespace != "agent-kourier" || b.Spec.Chat.Channel != trialChannel {
		t.Errorf("trial Binding %s/%s on %q; want agent-kourier/alert-trial on %s", b.Namespace, b.Name, b.Spec.Chat.Channel, trialChannel)
	}
	snap, err := config.Resolve(t.Context(), set, secrets{"agent-kourier/sandbox-slack": {"botToken", "appToken"}, "agent-kourier/agent-kourier-agent-token": {"token"}})
	if err != nil {
		t.Fatalf("the config does not resolve: %v", err)
	}
	if rb, ok := snap.BindingForChannel("agent-kourier", "sandbox-slack", trialChannel); !ok || rb.Binding.Name != "alert-trial" {
		t.Errorf("the alert channel resolves to %+v (%t); want the Binding alert-trial", rb.Binding, ok)
	}
	if rb, ok := snap.BindingForChannel("agent-kourier", "sandbox-slack", "C0SANDBOX01"); !ok || rb.Binding.Name != "sandbox" {
		t.Errorf("the dogfood channel resolves to %+v (%t); want the Binding sandbox, as before", rb.Binding, ok)
	}
}

// TestTrialBindingHasNoMentionTrigger: the trigger list is authoritative, so a list of one botMessage trigger and no mention means nobody can
// @mention Agent Kourier into the production channel.
func TestTrialBindingHasNoMentionTrigger(t *testing.T) {
	b, _ := trialBinding(t)
	ts := b.Spec.Chat.Triggers
	if len(ts) != 1 || ts[0].Type != v1alpha1.TriggerBotMessage {
		t.Fatalf("triggers %+v; want exactly one, of type botMessage", ts)
	}
	raw, err := os.ReadFile(trialFile)
	if err != nil {
		t.Fatal(err)
	}
	// The loader defaults an empty list to the mention trigger, so the file must list the trigger itself: look at the file too.
	if regexp.MustCompile(`type:\s*mention`).Match(raw) || !regexp.MustCompile(`type:\s*botMessage`).Match(raw) {
		t.Errorf("%s: want a botMessage trigger and no mention one", trialFile)
	}
}

// TestTrialBindingMatchesOnlyTheKindReceiver: the match on the title link is exactly the anchored kind receiver, and the title and the fallback still have to look like
// Alertmanager's, as the Alertmanager trigger writes them.
func TestTrialBindingMatchesOnlyTheKindReceiver(t *testing.T) {
	b, _ := trialBinding(t)
	tr := b.Spec.Chat.Triggers[0]
	if got := tr.Match[v1alpha1.FieldTitleLink]; got != trialReceiver {
		t.Errorf("match titleLink = %q; want exactly %q (anchored: without the $, receiver=slack-critical-kind is the only one, but a longer name would match too)", got, trialReceiver)
	}
	if !isAlertmanagerTrigger(tr, heartbeatExclude) {
		t.Errorf("the trigger is not the Alertmanager trigger written in full, with only the heartbeat excluded: %+v", tr)
	}
	if l := tr.Limits; !limitsAreTheExamples(l) {
		t.Errorf("limits %+v (rate %+v); want the example's: 5 in 10m, 2 at once, 40 a day", l, l.RateLimit)
	}
}

// TestDryRunBindingsAreTheAlertmanagerTriggerForTheirReceiver: the two Bindings the sandbox README has the owner replay
// a channel through differ only in the receiver they take, so each is the Alertmanager trigger in full, with the
// example's limits and the heartbeat excluded.
func TestDryRunBindingsAreTheAlertmanagerTriggerForTheirReceiver(t *testing.T) {
	set, err := config.Load(t.Context(), sandboxDir+"dryrun")
	if err != nil {
		t.Fatal(err)
	}
	receivers := map[string]string{"trial": trialReceiver, "production": `receiver=slack-critical$`}
	if len(set.Bindings) != len(receivers) {
		t.Fatalf("loaded %d Bindings; want %d", len(set.Bindings), len(receivers))
	}
	for _, b := range set.Bindings {
		want, ok := receivers[b.Name]
		if !ok {
			t.Errorf("unexpected Binding %s", b.Name)
			continue
		}
		triggers := b.Spec.Chat.Triggers
		if len(triggers) != 2 || triggers[0].Type != v1alpha1.TriggerMention || triggers[1].Type != v1alpha1.TriggerBotMessage {
			t.Errorf("%s: triggers %+v; want a mention trigger and a botMessage trigger", b.Name, triggers)
			continue
		}
		tr := triggers[1]
		if !isAlertmanagerTrigger(tr, heartbeatExclude) || tr.From.BotID != trialBot || tr.Match[v1alpha1.FieldTitleLink] != want ||
			!limitsAreTheExamples(tr.Limits) {
			t.Errorf("%s: trigger %+v; want the Alertmanager trigger for %s with the receiver %q, the heartbeat excluded and the example's limits", b.Name, tr, trialBot, want)
		}
	}
}

// TestTrialBindingNamesOnlyTheAlertBot: one bot ID in the trigger and in the whole file: the incoming webhook's.
func TestTrialBindingNamesOnlyTheAlertBot(t *testing.T) {
	b, _ := trialBinding(t)
	if from := b.Spec.Chat.Triggers[0].From; from == nil || from.BotID != trialBot {
		t.Errorf("from = %+v; want bot %s", from, trialBot)
	}
	raw, err := os.ReadFile(trialFile)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, id := range regexp.MustCompile(`\bB[A-Z0-9]{8,}\b`).FindAllString(string(raw), -1) {
		ids[id] = true
	}
	if len(ids) != 1 || !ids[trialBot] {
		t.Errorf("%s names bot IDs %v; want only %s", trialFile, ids, trialBot)
	}
}

// TestTrialBindingAsksNoOneAnything: interactions.askUser is the field that lets the agent ask people a question in the thread; it is off, in the
// file and not by default alone. The output streams live.
func TestTrialBindingAsksNoOneAnything(t *testing.T) {
	b, _ := trialBinding(t)
	if b.Spec.Interactions.AskUser || b.Spec.Chat.Output != v1alpha1.OutputLive {
		t.Errorf("interactions %+v, output %q; want askUser off, output live", b.Spec.Interactions, b.Spec.Chat.Output)
	}
	raw, err := os.ReadFile(trialFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec struct {
			Interactions map[string]any `json:"interactions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if v, set := doc.Spec.Interactions["askUser"]; !set || v != false {
		t.Errorf("spec.interactions = %v; want askUser: false written out, so that it is a decision and not a default", doc.Spec.Interactions)
	}
}

// TestTrialBindingUsesTheDogfoodAgentAndIdentity: the same agent, backend, identity and connection as the dogfood Binding, so that the trial runs
// sandbox-sre through the verifying front door as the dogfood does. The config has no rule of one Binding per identity.
func TestTrialBindingUsesTheDogfoodAgentAndIdentity(t *testing.T) {
	trial, set := trialBinding(t)
	var dogfood *v1alpha1.Binding
	for i := range set.Bindings {
		if set.Bindings[i].Name == "sandbox" {
			dogfood = &set.Bindings[i]
		}
	}
	if dogfood == nil {
		t.Fatal("no dogfood Binding")
	}
	if !reflect.DeepEqual(trial.Spec.Agent, dogfood.Spec.Agent) || !reflect.DeepEqual(trial.Spec.Identity, dogfood.Spec.Identity) ||
		!reflect.DeepEqual(trial.Spec.Chat.ConnectionRef, dogfood.Spec.Chat.ConnectionRef) {
		t.Errorf("trial agent/identity/connection %+v, %+v, %+v; want the dogfood Binding's %+v, %+v, %+v",
			trial.Spec.Agent, trial.Spec.Identity, trial.Spec.Chat.ConnectionRef, dogfood.Spec.Agent, dogfood.Spec.Identity, dogfood.Spec.Chat.ConnectionRef)
	}
	if trial.Spec.Agent.Name != "sandbox-sre" || trial.Spec.Agent.Namespace != "kagent" {
		t.Errorf("agent %s/%s; want kagent/sandbox-sre", trial.Spec.Agent.Namespace, trial.Spec.Agent.Name)
	}
}

// trialFile is the trial Binding's manifest.
const trialFile = sandboxDir + "agent-kourier/alert-trial/alert-binding.yaml"

// TestTrialIsAppliedOnlyWithTheAlertSource: without the alert source (no webhook file) the Agent Kourier config is byte for byte what it was before the
// trial existed; with it, the trial's Binding is added and nothing else changes.
func TestTrialIsAppliedOnlyWithTheAlertSource(t *testing.T) {
	read := func(dir string) map[string]string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		files := map[string]string{}
		for _, e := range entries {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files[e.Name()] = string(raw)
		}
		return files
	}
	without, with := read(trialConfig(t, "0")), read(trialConfig(t, "1"))
	before := read(sandboxConfig(t))
	if !reflect.DeepEqual(without, before) {
		t.Errorf("without the alert source the config is %v, which differs from the dogfood-only config %v", slices.Sorted(maps.Keys(without)), slices.Sorted(maps.Keys(before)))
	}
	for name, content := range without {
		if with[name] != content {
			t.Errorf("%s changed when the alert source was turned on", name)
		}
	}
	if extra := len(with) - len(without); extra != 1 || with["alert-binding.yaml"] == "" {
		t.Errorf("the alert source adds %d files (%v); want only alert-binding.yaml", extra, slices.Sorted(maps.Keys(with)))
	}
	if strings.Contains(strings.Join(slices.Collect(maps.Values(without)), "\n"), trialChannel) || strings.Contains(strings.Join(slices.Collect(maps.Values(with)), "\n"), "__") {
		t.Error("the alert channel is in the config without the alert source, or a placeholder is left after rendering")
	}
	// up.sh builds the ConfigMap from render_agentkourier_config, after the alert source is decided and not before.
	raw, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if n := strings.Count(s, `render_agentkourier_config "${SCRATCH}"`); n != 1 {
		t.Errorf("up.sh calls render_agentkourier_config %d times; want once, into SCRATCH, which the ConfigMap is made from", n)
	}
	if strings.Index(s, `render_agentkourier_config "${SCRATCH}"`) < strings.Index(s, "ALERT_SOURCE=1") {
		t.Error("up.sh renders the config before it has decided on the alert source")
	}
	if strings.Contains(s, "alert-trial") && !strings.Contains(s, "render_agentkourier_config") {
		t.Error("up.sh renders the trial's files itself instead of through render_agentkourier_config")
	}
}

// alertChannelCheck runs webhook.sh's alert_channel_check with the given channel constant and dogfood channel, and returns its status and stderr.
func alertChannelCheck(t *testing.T, alertChannel, dogfoodChannel string) (ok bool, stderr string) {
	t.Helper()
	script := `set -eu
. "$1/env.sh"
. "$1/webhook.sh"
ALERT_CHANNEL_ID=$2
SLACK_CHANNEL_ID=$3
alert_channel_check`
	var errOut bytes.Buffer
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", strings.TrimSuffix(sandboxDir, "/"), alertChannel, dogfoodChannel)
	cmd.Stderr = &errOut
	return cmd.Run() == nil, errOut.String()
}

// TestDryRunVetsTheTriggerThatRuns: the dry run's trial Binding (dryrun/trial.yaml) has the same botMessage trigger, defaulted, as the live one, so
// that "0 matches on production history" is said of the trigger that will run. (The dry run keeps a mention trigger first, which it never reads.)
func TestDryRunVetsTheTriggerThatRuns(t *testing.T) {
	live, _ := trialBinding(t)
	set, err := config.Load(t.Context(), sandboxDir+"dryrun")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range set.Bindings {
		if b.Name != "trial" {
			continue
		}
		var bots []v1alpha1.Trigger
		for _, tr := range b.Spec.Chat.Triggers {
			if tr.Type == v1alpha1.TriggerBotMessage {
				bots = append(bots, tr)
			}
		}
		if len(bots) != 1 || !reflect.DeepEqual(bots[0], live.Spec.Chat.Triggers[0]) {
			t.Errorf("dry run trial triggers %+v; want the one botMessage trigger the live Binding has, %+v", bots, live.Spec.Chat.Triggers[0])
		}
		return
	}
	t.Fatal("dryrun/ has no Binding trial")
}

// TestTrialChannelIsValidated: the alert channel is a channel ID (not a name, and not empty), and it is not the dogfood channel, which the
// dogfood Binding already holds: two Bindings cannot share a channel, and Agent Kourier would refuse the config.
func TestTrialChannelIsValidated(t *testing.T) {
	for name, tc := range map[string]struct {
		alert, dogfood string
		ok             bool
		want           string
	}{
		"the alert channel":       {trialChannel, "C0SANDBOX01", true, ""},
		"a private channel's ID":  {"G0EXAMPLE01", "C0SANDBOX01", true, ""},
		"a name":                  {"alerts-critical", "C0SANDBOX01", false, "ALERT_CHANNEL_ID"},
		"lower case":              {"C0EXAMPLE01", "C0SANDBOX01", false, "ALERT_CHANNEL_ID"},
		"empty":                   {"", "C0SANDBOX01", false, "ALERT_CHANNEL_ID"},
		"the dogfood channel too": {trialChannel, trialChannel, false, "SLACK_CHANNEL_ID"},
	} {
		t.Run(name, func(t *testing.T) {
			ok, stderr := alertChannelCheck(t, tc.alert, tc.dogfood)
			if ok != tc.ok || (!tc.ok && !strings.Contains(stderr, tc.want)) {
				t.Errorf("ok = %t, stderr %q; want ok %t and, if refused, %q named", ok, stderr, tc.ok, tc.want)
			}
		})
	}
}

// TestUpRefusesADogfoodChannelThatIsTheAlertChannel: with a webhook file, a secrets file whose SLACK_CHANNEL_ID is the alert channel stops up.sh in
// its preflight, before Docker or any cluster tool is asked anything; without a webhook file the same secrets file is accepted.
func TestUpRefusesADogfoodChannelThatIsTheAlertChannel(t *testing.T) {
	env := withLine(goodEnv(), "SLACK_CHANNEL_ID", "SLACK_CHANNEL_ID="+trialChannel)
	out, ok, ran := upWithEnv(t, webhookFile(t, 0o600, fakeWebhook+"\n"), env...)
	if ok || !strings.Contains(out, "SLACK_CHANNEL_ID is the alert channel") || ran != "" {
		t.Errorf("up.sh with a dogfood channel that is the alert channel: ok %t, ran %q, output %q; want a refusal that names SLACK_CHANNEL_ID, before any tool", ok, ran, out)
	}
	noLeak(t, "up.sh output", out)
	out, _, ran = upWithEnv(t, filepath.Join(t.TempDir(), "nowhere"), env...)
	if strings.Contains(out, "alert channel") || !strings.HasPrefix(ran, "docker") {
		t.Errorf("up.sh without a webhook file, ran %q, output %q; want it to go on to Docker with no word of the alert channel", ran, out)
	}
}

// TestTrialChannelIsOneConstantEverywhere: the channel the trial Binding is rendered with is the constant in webhook.sh, and it is the channel the dry
// run's Bindings and the runbook name, so that the trial, the dry run that vets it and the docs cannot point at different channels.
func TestTrialChannelIsOneConstantEverywhere(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "webhook.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ALERT_CHANNEL_ID=(\S+)`).FindAllStringSubmatch(string(raw), -1)
	if len(m) != 1 || m[0][1] != trialChannel {
		t.Fatalf("webhook.sh sets ALERT_CHANNEL_ID %v; want once, to %s", m, trialChannel)
	}
	for _, file := range []string{"dryrun/trial.yaml", "dryrun/production.yaml"} {
		set, err := config.Load(t.Context(), filepath.Join(sandboxDir, filepath.Dir(file)))
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range set.Bindings {
			if b.Spec.Chat.Channel != trialChannel {
				t.Errorf("%s: Binding %s is on %s; want the trial's channel %s", file, b.Name, b.Spec.Chat.Channel, trialChannel)
			}
		}
	}
	readme, err := os.ReadFile(sandboxDir + "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), trialChannel) {
		t.Errorf("the README does not name the alert channel %s", trialChannel)
	}
	// The placeholder is in the trial's file once, as the channel, and nowhere else in the sandbox's config.
	manifest, err := os.ReadFile(trialFile)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(regexp.MustCompile(`(?m)^\s+channel: __ALERT_CHANNEL_ID__(\s+#.*)?$`).FindAll(manifest, -1)); n != 1 || strings.Contains(string(manifest), trialChannel) {
		t.Errorf("%s: want the channel as the one value __ALERT_CHANNEL_ID__ and the ID itself nowhere (webhook.sh holds it)", trialFile)
	}
	dogfood, err := os.ReadFile(sandboxDir + "agent-kourier/config/binding.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dogfood), "__ALERT_CHANNEL_ID__") || strings.Contains(string(dogfood), trialChannel) {
		t.Error("the dogfood Binding carries the alert channel or its placeholder")
	}
}

// Alert-channel samples, as the adapter flattens them: kind's Alertmanager posts through the same incoming webhook as production's Alertmanager, Grafana's
// alerting and the Watchdog heartbeat. The shapes are the README's ("Alert trial") and the alert channel's (chat-triggers.md, "Facts gathered").
const (
	sampleKindLink = "http://alertmanager.kind.local/#/alerts?receiver=slack-critical-kind"
	sampleProdLink = "https://alertmanager.example.com/#/alerts?receiver=slack-critical"
	sampleBeatLink = "https://alertmanager.example.com/#/alerts?receiver=slack-heartbeat"
)

func sample(bot, title, link, fallback string) chat.BotMessage {
	return chat.BotMessage{BotID: bot, Title: title, TitleLink: link, Fallback: fallback, AttachmentText: "*" + title + "*"}
}

// TestTrialTriggerOverTheChannelsMessages runs the intake's matcher (chat.Replay, the dry run's engine) over the sandbox's real config files, with the
// kind alerts and what else is in the alert channel: only the kind alerts match.
func TestTrialTriggerOverTheChannelsMessages(t *testing.T) {
	b, _ := trialBinding(t)
	replay, err := chat.NewReplay(b)
	if err != nil {
		t.Fatal(err)
	}
	kindFiring := sample(trialBot, "[kind] FIRING (1) - KubePodCrashLooping", sampleKindLink, "[FIRING:1] KubePodCrashLooping shop (checkout-7d9f8b6c5-x2x4k critical) | "+sampleKindLink)
	kindResolved := sample(trialBot, "[kind] RESOLVED - KubePodCrashLooping", sampleKindLink, "[RESOLVED] KubePodCrashLooping shop (checkout-7d9f8b6c5-x2x4k critical) | "+sampleKindLink)
	kindGroup := sample(trialBot, "[kind] FIRING (3) - KubePodNotReady", sampleKindLink, "[FIRING:3] KubePodNotReady shop (critical) | "+sampleKindLink)

	otherBot := kindFiring
	otherBot.BotID = "B0OTHERBOT1"
	// A longer receiver name that merely starts with the kind one: the $ keeps it out.
	longer := kindFiring
	longer.TitleLink = sampleKindLink + "-staging"
	// A production alert that has the kind receiver's name somewhere other than the end of its link.
	decoy := sample(trialBot, "FIRING (1) - KubeAPIErrorBudgetBurn", sampleProdLink+"&note=receiver=slack-critical-kind-x",
		"[FIRING:1] KubeAPIErrorBudgetBurn kube-system (long 1h critical apiserver) | "+sampleProdLink)

	tests := []struct {
		name string
		msg  chat.BotMessage
		want chat.ReplayOutcome
		keys map[string]string
	}{
		{"kind firing", kindFiring, chat.ReplayMatched, map[string]string{"state": "FIRING", "alertname": "KubePodCrashLooping", "key": "KubePodCrashLooping shop"}},
		{"kind resolved", kindResolved, chat.ReplayMatched, map[string]string{"state": "RESOLVED", "alertname": "KubePodCrashLooping", "key": "KubePodCrashLooping shop"}},
		{"kind group of three", kindGroup, chat.ReplayMatched, map[string]string{"state": "FIRING", "alertname": "KubePodNotReady", "key": "KubePodNotReady shop"}},
		{"production firing", sample(trialBot, "FIRING (1) - KubeAPIErrorBudgetBurn", sampleProdLink,
			"[FIRING:1] KubeAPIErrorBudgetBurn kube-system (long 1h critical apiserver) | "+sampleProdLink), chat.ReplayUnmatched, nil},
		{"production resolved", sample(trialBot, "RESOLVED - KubeAPIErrorBudgetBurn", sampleProdLink,
			"[RESOLVED] KubeAPIErrorBudgetBurn kube-system (long 1h critical apiserver) | "+sampleProdLink), chat.ReplayUnmatched, nil},
		{"production alert that merely mentions the kind receiver", decoy, chat.ReplayUnmatched, nil},
		{"Grafana alerting", sample(trialBot, "FIRING: HighRequestLatency (payments)", "https://grafana.example.com/alerting/list",
			"FIRING: HighRequestLatency (payments)"), chat.ReplayUnmatched, nil},
		{"the Watchdog heartbeat", sample(trialBot, "Prometheus heartbeat", sampleBeatLink,
			"[FIRING:1] Watchdog  (none prometheus) | "+sampleBeatLink), chat.ReplayUnmatched, nil},
		{"a longer receiver name", longer, chat.ReplayUnmatched, nil},
		{"a kind alert from another bot", otherBot, chat.ReplayOtherBot, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := replay.Classify(chat.HistoryMessage{Message: chat.Inbound{At: chat.Ref{Platform: "slack", Conn: "agent-kourier/sandbox-slack", Channel: trialChannel, ID: "1.1"}, Bot: &tc.msg}})
			if v.Outcome != tc.want || !reflect.DeepEqual(v.Fields, tc.keys) {
				t.Errorf("outcome %s with %v (%s); want %s with %v", v.Outcome, v.Fields, v.Why, tc.want, tc.keys)
			}
		})
	}
	// A person's message, and an @mention of Agent Kourier in the channel, are no input of the trial: there is no mention trigger.
	if v := replay.Classify(chat.HistoryMessage{Message: chat.Inbound{At: chat.Ref{Platform: "slack", Channel: trialChannel, ID: "2.1"}, User: "U1", Text: "<@UAGENTKOURIER> look at this"}}); v.Outcome != chat.ReplayPerson {
		t.Errorf("a person's message: outcome %s; want %s", v.Outcome, chat.ReplayPerson)
	}
}

// TestAlertTrialRunbook: the README has the runbook, and it names what the trial depends on and where to look.
func TestAlertTrialRunbook(t *testing.T) {
	readme, err := os.ReadFile(sandboxDir + "README.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(readme)
	i := strings.Index(s, "\n## Run the alert trial\n")
	if i < 0 {
		t.Fatal("the README has no \"Run the alert trial\" section")
	}
	section := s[i+1:]
	if j := strings.Index(section[3:], "\n## "); j >= 0 {
		section = section[:j+3]
	}
	for _, want := range []string{
		"real alert channel", "make sandbox-up", "make sandbox-alert ALERT=KubePodCrashLooping", "STATE=resolved", "slack: socket mode connected",
		"Fired again at", "Resolved at", "[kind] FIRING (1) - KubePodCrashLooping", "dedup", "repeat_interval",
		"session turn started", "chat: trigger decided", "agentkourier_trigger_decisions_total", "agentkourier_trigger_refused_total", "agentkourier_trigger_unmatched_total",
		"agent-kourier dry-run", "agent-kourier/trial", "make sandbox-down", "alertmanager-webhook-url", "regenerate", "invite", "no mention", "askUser",
		"step", "separate thread", "KubePodNotReady",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the runbook does not mention %q", want)
		}
	}
	if !strings.Contains(strings.ToLower(section), "warning") {
		t.Error("the runbook has no warning that it posts into the real alert channel")
	}
}
