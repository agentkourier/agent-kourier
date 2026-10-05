// Package kindmanifests_test checks the manifests and config that the kind tiers apply, which live under
// hack/kind, so a mistake in them fails `make test` and not the next `make e2e-fast-up`. It reads them
// by relative path, as test/e2e/kube's runner_role_test.go does for the runner's Role.
package kindmanifests_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/test/e2e"
)

// secrets is what lib.sh's deploy_courier and up-fast.sh create in namespace courier (never committed): the keys, not the values.
type secrets map[string][]string

func (s secrets) Keys(_ context.Context, namespace, name string) ([]string, error) {
	if keys, ok := s[namespace+"/"+name]; ok {
		return keys, nil
	}
	return nil, fmt.Errorf("%s/%s: %w", namespace, name, config.ErrSecretNotFound)
}

// courierDir is the fast tier's courier config. Its Deployment, volume, Role and Service are the chart's (charts/courier, with
// ci/fast-values.yaml), which test/chart pins.
const courierDir = "../../hack/kind/fast/courier/"

var upFastSecrets = secrets{"courier/slack-fast": {"botToken", "appToken"}, "courier/courier-agent-token": {"token"}}

func TestShippedConfigLoadsAndResolves(t *testing.T) {
	set, err := config.Load(t.Context(), courierDir+"config")
	if err != nil || len(set.Bindings) != 11 || len(set.ChatConnections) != 1 || len(set.AgentBackends) != 1 {
		t.Fatalf("Load = %+v, %v; want a Binding for the smoke test and one for each of ten scenarios, and one connection and backend", set, err)
	}
	snap, err := config.Resolve(t.Context(), set, upFastSecrets)
	if err != nil {
		t.Fatal(err)
	}
	rb, ok := snap.BindingForChannel("courier", "slack-fast", "C1")
	if !ok || rb.Binding.Spec.Agent.Name != "test-agent" || rb.Backend.Spec.URL != "http://stubagent.e2e.svc.cluster.local:8080" {
		t.Errorf("channel C1 resolves to %+v, %v; want the Binding for test-agent on the stub agent's Service", rb, ok)
	}
	// The Secrets are the ones up-fast.sh makes: without either, the config does not resolve.
	for name, missing := range map[string]string{"the Slack tokens": "courier/slack-fast", "the agent token": "courier/courier-agent-token"} {
		partial := secrets{}
		for k, v := range upFastSecrets {
			if k != missing {
				partial[k] = v
			}
		}
		if _, err := config.Resolve(t.Context(), set, partial); err == nil {
			t.Errorf("config resolved without %s", name)
		}
	}
}

// objects decodes every document of a manifest file.
func objects(t *testing.T, file string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		out = append(out, doc)
	}
}

// load reads the object of the given kind in file into into.
func load(t *testing.T, file, kind string, into any) {
	t.Helper()
	for _, doc := range objects(t, file) {
		if doc["kind"] == kind {
			b, _ := json.Marshal(doc)
			if err := json.Unmarshal(b, into); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("%s has no %s", file, kind)
}

// TestScenarioBindings: each scenario has a channel of its own, bound to the stub agent it drives, with
// the interactions it needs: askUser for S3 (the agent asks), and neither for S11, whose tool approval
// courier must deny (Phase 1 has no write-capable agent). test/e2e names the channels.
func TestScenarioBindings(t *testing.T) {
	set, err := config.Load(t.Context(), courierDir+"config")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := config.Resolve(t.Context(), set, upFastSecrets)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tc := range []struct {
		scenario, channel, agent string
		askUser                  bool
		output                   v1alpha1.OutputMode // S5's is final: a reply to a person is streamed, and only a post goes through the outbox where the crash hook is
	}{
		{"smoke", e2e.ChannelSmoke, "test-agent", false, v1alpha1.OutputLive},
		{"S1", e2e.ChannelS1, "scenario-gated", false, v1alpha1.OutputLive},
		{"S2", e2e.ChannelS2, "scenario-memory", false, v1alpha1.OutputLive},
		{"S3", e2e.ChannelS3, "scenario-ask", true, v1alpha1.OutputLive},
		{"S4", e2e.ChannelS4, "scenario-gated", false, v1alpha1.OutputLive},
		{"S5", e2e.ChannelS5, "test-agent", false, v1alpha1.OutputFinal},
		{"S11", e2e.ChannelS11, "spike-approve", false, v1alpha1.OutputLive},
		{"S12", e2e.ChannelS12, "scenario-fails", false, v1alpha1.OutputLive},
		{"S13", e2e.ChannelS13, "scenario-tools", false, v1alpha1.OutputLive},
		{"S14", e2e.ChannelS14, "scenario-alert", false, v1alpha1.OutputLive},
		{"S15", e2e.ChannelS15, "scenario-alert", false, v1alpha1.OutputLive},
	} {
		rb, ok := snap.BindingForChannel("courier", "slack-fast", tc.channel)
		if !ok {
			t.Errorf("%s: channel %s resolves to no Binding", tc.scenario, tc.channel)
			continue
		}
		spec := rb.Binding.Spec
		if spec.Agent.Name != tc.agent || spec.Agent.Namespace != "kagent" || spec.Interactions.AskUser != tc.askUser || spec.Interactions.ToolApprovals {
			t.Errorf("%s: channel %s is bound to %s/%s with askUser %t and toolApprovals %t; want %s, askUser %t and toolApprovals off", tc.scenario, tc.channel,
				spec.Agent.Namespace, spec.Agent.Name, spec.Interactions.AskUser, spec.Interactions.ToolApprovals, tc.agent, tc.askUser)
		}
		if spec.Chat.Output != tc.output {
			t.Errorf("%s: channel %s has output %q; want %q", tc.scenario, tc.channel, spec.Chat.Output, tc.output)
		}
		if seen[tc.channel] {
			t.Errorf("%s: channel %s is another scenario's too; scenarios must not share a thread", tc.scenario, tc.channel)
		}
		seen[tc.channel] = true
	}
}

// TestS14TakesTheAlertBotByItsIDWithThePreset: S14's channel has a botMessage trigger for the bot ID the scenario posts
// as, with the alertmanager preset, narrowed to the kind receiver as the trial's is, and no mention trigger: only an
// alert starts a session there, and a person's reply in its thread continues it. The ID is the one test/e2e names.
func TestS14TakesTheAlertBotByItsIDWithThePreset(t *testing.T) {
	set, err := config.Load(t.Context(), courierDir+"config")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := config.Resolve(t.Context(), set, upFastSecrets)
	if err != nil {
		t.Fatal(err)
	}
	rb, ok := snap.BindingForChannel("courier", "slack-fast", e2e.ChannelS14)
	if !ok {
		t.Fatalf("channel %s resolves to no Binding", e2e.ChannelS14)
	}
	triggers := rb.Binding.Spec.Chat.Triggers
	if len(triggers) != 1 || triggers[0].Type != v1alpha1.TriggerBotMessage || triggers[0].From == nil || triggers[0].From.BotID != e2e.BotS14 ||
		triggers[0].Preset != v1alpha1.PresetAlertmanager || triggers[0].Match[v1alpha1.FieldTitleLink] != `receiver=slack-critical-kind$` {
		t.Errorf("triggers %+v; want the one botMessage trigger for %s with the alertmanager preset, narrowed to the kind receiver", triggers, e2e.BotS14)
	}
}

// TestS15LimitsTheStormToThreeStartsInTenSeconds: S15's trigger is S14's, for the bot the scenario posts as, with limits
// of its own: three starts in 10 seconds (the shortest window, so the digest comes within a minute), enough room to run
// at once, and no daily cap, so that runs of the scenario never meet the preset's 40 a day. The preset's thread rules
// stay. The numbers are the ones the scenario asserts.
func TestS15LimitsTheStormToThreeStartsInTenSeconds(t *testing.T) {
	set, err := config.Load(t.Context(), courierDir+"config")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := config.Resolve(t.Context(), set, upFastSecrets)
	if err != nil {
		t.Fatal(err)
	}
	rb, ok := snap.BindingForChannel("courier", "slack-fast", e2e.ChannelS15)
	if !ok {
		t.Fatalf("channel %s resolves to no Binding", e2e.ChannelS15)
	}
	triggers := rb.Binding.Spec.Chat.Triggers
	if len(triggers) != 1 || triggers[0].Type != v1alpha1.TriggerBotMessage || triggers[0].From == nil || triggers[0].From.BotID != e2e.BotS15 ||
		triggers[0].Preset != v1alpha1.PresetAlertmanager || triggers[0].Match[v1alpha1.FieldTitleLink] != `receiver=slack-critical-kind$` {
		t.Fatalf("triggers %+v; want the one botMessage trigger for %s with the alertmanager preset, narrowed to the kind receiver", triggers, e2e.BotS15)
	}
	l := triggers[0].Limits
	if l.RateLimit == nil || l.RateLimit.MaxRuns != 3 || l.RateLimit.Per.Duration != 10*time.Second || l.RateLimit.MaxConcurrent != 5 || l.MaxRunsPerDay == nil || *l.MaxRunsPerDay != 0 {
		t.Errorf("limits %+v (rate %+v); want 3 in 10s, 5 at once and no daily cap", l, l.RateLimit)
	}
	if triggers[0].Thread.Cooldown.Duration != 4*time.Hour {
		t.Errorf("thread %+v; want the preset's", triggers[0].Thread)
	}
}

// TestRunnerJobGetsItsTier: the Job runs the tests of one tier, which a test must be able to tell, and the
// scenarios' gates are released at the place that holds them in that tier: the stub agent in the fast tier, and
// the scenario model in the full one. The Job's template carries placeholders and e2e-run.sh fills them with the
// tier it was given and that tier's gate URL.
func TestRunnerJobGetsItsTier(t *testing.T) {
	var job batchv1.Job
	load(t, "../../hack/kind/e2e/runner.yaml", "Job", &job)
	var tier, stub string
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		switch e.Name {
		case "E2E_TIER":
			tier = e.Value
		case "E2E_STUB_URL":
			stub = e.Value
		}
	}
	script, err := os.ReadFile("../../hack/kind/e2e-run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if tier != "@TIER@" || !strings.Contains(string(script), `-e "s/@TIER@/${tier}/"`) {
		t.Errorf("E2E_TIER = %q in the Job; want the placeholder @TIER@, and e2e-run.sh to substitute the tier it runs", tier)
	}
	if stub != "@STUB_URL@" || !strings.Contains(string(script), `-e "s|@STUB_URL@|${gate_url}|"`) ||
		!strings.Contains(string(script), "fast) gate_url=http://stubagent.e2e.svc.cluster.local:8080") ||
		!strings.Contains(string(script), "full) gate_url=http://scenariollm.kagent.svc.cluster.local:8080") {
		t.Errorf("E2E_STUB_URL = %q in the Job; want the placeholder @STUB_URL@, which e2e-run.sh sets to the stub's Service in the fast tier and the scenario model's in the full tier", stub)
	}
}

// TestFullTierConfig: the full tier's courier config is the fast tier's, with the AgentBackend of kagent's front
// door in place of the stub's (lib.sh's deploy_courier assembles it the same way). Courier reaches kagent only
// through the door, on 4180, and never the controller, which does not verify tokens (spec D32, R4); and the
// Bindings, which are the fast tier's, find the backend by the name they share.
func TestFullTierConfig(t *testing.T) {
	dir := t.TempDir()
	files, err := filepath.Glob(courierDir + "config/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fast-tier config: %v", err)
	}
	for _, f := range append(files, "../../hack/kind/full/courier/agentbackend.yaml") { // the last wins, as in deploy_courier
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		if strings.HasSuffix(f, "full/courier/agentbackend.yaml") {
			name = "agentbackend.yaml"
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	set, err := config.Load(t.Context(), dir)
	if err != nil || len(set.AgentBackends) != 1 {
		t.Fatalf("Load = %+v, %v; want one AgentBackend", set, err)
	}
	snap, err := config.Resolve(t.Context(), set, upFastSecrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{e2e.ChannelSmoke, e2e.ChannelS1, e2e.ChannelS2, e2e.ChannelS3, e2e.ChannelS4, e2e.ChannelS5, e2e.ChannelS11, e2e.ChannelS13} {
		rb, ok := snap.BindingForChannel("courier", "slack-fast", channel)
		if !ok {
			t.Errorf("channel %s resolves to no Binding in the full tier", channel)
			continue
		}
		if got := rb.Backend.Spec.URL; got != "http://kagent-frontdoor.kagent.svc.cluster.local:4180" || rb.Backend.Spec.Dialect != "kagent-v1" {
			t.Errorf("channel %s goes to %s (%s); want kagent's front door on 4180, never the controller on 8083", channel, got, rb.Backend.Spec.Dialect)
		}
		if rb.Binding.Spec.Agent.Namespace != "kagent" {
			t.Errorf("channel %s names agent namespace %q; the full tier's agents are in kagent", channel, rb.Binding.Spec.Agent.Namespace)
		}
	}
}

// TestScenarioServersAreNotPublished: the scenario model's /_control/release/ and the MCP tool server have no
// authentication, like the fake Slack's and the stub agent's, so their Services are ClusterIP with no node port or
// external address and their pods publish no host port (full/verify.sh proves the same against the cluster).
func TestScenarioServersAreNotPublished(t *testing.T) {
	// The servers are in servers.yaml and the agents in scenarios.yaml; both are read, and each check says what it
	// expects to find, so that moving an object between the files cannot make the check pass by finding nothing.
	var docs []map[string]any
	for _, file := range []string{"../../hack/kind/full/scenarios/servers.yaml", "../../hack/kind/full/scenarios/scenarios.yaml"} {
		docs = append(docs, objects(t, file)...)
	}
	for _, name := range []string{"scenariollm", "scenariomcp"} {
		var svc corev1.Service
		var found bool
		for _, doc := range docs {
			meta, _ := doc["metadata"].(map[string]any)
			if doc["kind"] == "Service" && meta["name"] == name {
				b, _ := json.Marshal(doc)
				if err := json.Unmarshal(b, &svc); err != nil {
					t.Fatal(err)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("the scenario manifests have no Service %s", name)
		}
		if svc.Spec.Type != "" && svc.Spec.Type != corev1.ServiceTypeClusterIP || len(svc.Spec.ExternalIPs) != 0 || svc.Spec.LoadBalancerIP != "" {
			t.Errorf("Service %s is %q with external IPs %v; want ClusterIP and nothing published", name, svc.Spec.Type, svc.Spec.ExternalIPs)
		}
		for _, p := range svc.Spec.Ports {
			if p.NodePort != 0 {
				t.Errorf("Service %s publishes node port %d", name, p.NodePort)
			}
		}
	}
	deployments := map[string]bool{}
	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		b, _ := json.Marshal(doc)
		var d appsv1.Deployment
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		deployments[d.Name] = true
		pod := d.Spec.Template.Spec
		if pod.HostNetwork {
			t.Errorf("Deployment %s uses the host network", d.Name)
		}
		for _, c := range pod.Containers {
			for _, p := range c.Ports {
				if p.HostPort != 0 {
					t.Errorf("Deployment %s publishes host port %d", d.Name, p.HostPort)
				}
			}
		}
	}
	for _, name := range []string{"scenariollm", "scenariomcp"} {
		if !deployments[name] {
			t.Errorf("the scenario manifests have no Deployment %s, so its host ports were not checked", name)
		}
	}
}

// TestTheControllerRestartTouchesOnlyTheController: settle_full_tier deletes the kagent controller's pod by two
// labels, its component and its name, so that no other pod of kagent that is labelled as a controller goes with it.
func TestTheControllerRestartTouchesOnlyTheController(t *testing.T) {
	raw, err := os.ReadFile("../../hack/kind/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `delete pod -l app.kubernetes.io/component=controller,app.kubernetes.io/name=kagent `) {
		t.Error("lib.sh's settle_full_tier does not delete the controller's pod by its component and name labels together")
	}
}

// TestFullTierHasTheToolsScenarioAgent: S13 runs in both tiers, so the full tier's kagent has an agent of the name its
// Binding gives (scenario-tools), whose model is the scenario model playing the "tools" scenario, and whose tools are the
// scenario MCP server's, run without approval (the calls are the scenario, and S13's Binding does not allow approvals).
// The server has the two tools the scenario model calls.
func TestFullTierHasTheToolsScenarioAgent(t *testing.T) {
	var agent, model map[string]any
	for _, doc := range objects(t, "../../hack/kind/full/scenarios/scenarios.yaml") {
		meta, _ := doc["metadata"].(map[string]any)
		if meta["name"] != "scenario-tools" {
			continue
		}
		switch doc["kind"] {
		case "Agent":
			agent = doc
		case "ModelConfig":
			model = doc
		}
	}
	if agent == nil || model == nil {
		t.Fatalf("scenarios.yaml has Agent %v and ModelConfig %v named scenario-tools; want both", agent != nil, model != nil)
	}
	b, _ := json.Marshal(agent)
	var a struct {
		Spec struct {
			Template struct {
				ModelConfig struct{ Name string }
				Tools       []struct {
					MCP struct {
						Server          struct{ Kind, Name string }
						RequireApproval bool
					}
				}
			}
		}
	}
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	tpl := a.Spec.Template
	if tpl.ModelConfig.Name != "scenario-tools" || len(tpl.Tools) != 1 || tpl.Tools[0].MCP.Server.Kind != "RemoteMCPServer" ||
		tpl.Tools[0].MCP.Server.Name != "scenariomcp" || tpl.Tools[0].MCP.RequireApproval {
		t.Errorf("agent %+v; want the model scenario-tools and the one tool server scenariomcp, with no approval", tpl)
	}
	spec, _ := model["spec"].(map[string]any)
	if headers, _ := spec["defaultHeaders"].(map[string]any); headers["X-Scenario"] != "tools" {
		t.Errorf("the model sends %v; want X-Scenario: tools, the scenario that scenariollm.py plays for this agent", headers)
	}
	for file, wants := range map[string][]string{
		"../../hack/kind/full/scenarios/scenariollm.py": {`scenario == "tools"`},
		"../../hack/kind/full/scenarios/scenariomcp.py": {`"check_status"`, `"restart_service"`},
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s has no %s", filepath.Base(file), want)
			}
		}
	}
}
