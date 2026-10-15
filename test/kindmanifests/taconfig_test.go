package kindmanifests_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const taConfigDir = "../../hack/kind/full/ta-config/"

func TestTAConfigManifestsParse(t *testing.T) {
	for _, file := range []string{"prompt.yaml", "capture.yaml", "model.yaml", "agent.yaml"} {
		docs := objects(t, filepath.Join(taConfigDir, file))
		if len(docs) == 0 {
			t.Errorf("%s holds no objects", file)
		}
		for _, doc := range docs {
			if doc["apiVersion"] == nil || doc["kind"] == nil || doc["metadata"] == nil {
				t.Errorf("%s: an object without apiVersion, kind or metadata: %v", file, doc)
			}
		}
	}
}

func TestTAConfigAgentUsesPromptConfigMap(t *testing.T) {
	var agent, prompt, model map[string]any
	for _, doc := range objects(t, filepath.Join(taConfigDir, "agent.yaml")) {
		if doc["kind"] == "Agent" {
			agent = doc
		}
	}
	for _, doc := range objects(t, filepath.Join(taConfigDir, "prompt.yaml")) {
		if doc["kind"] == "ConfigMap" {
			prompt = doc
		}
	}
	for _, doc := range objects(t, filepath.Join(taConfigDir, "model.yaml")) {
		if doc["kind"] == "ModelConfig" {
			model = doc
		}
	}
	if agent == nil || prompt == nil || model == nil {
		t.Fatalf("fixture has Agent %v, ConfigMap %v and ModelConfig %v; want all three", agent != nil, prompt != nil, model != nil)
	}
	var decoded struct {
		Spec struct {
			Template struct {
				ModelConfig      struct{ Name string } `json:"modelConfig"`
				SystemPrompt     string                `json:"systemPrompt"`
				SystemPromptFrom struct {
					Name string `json:"name"`
					Key  string `json:"key"`
				} `json:"systemPromptFrom"`
				Tools []any `json:"tools"`
			}
		}
	}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	tpl := decoded.Spec.Template
	if tpl.ModelConfig.Name != "ta-config-model" || tpl.SystemPrompt != "" || tpl.SystemPromptFrom.Name != "ta-config-prompt" || tpl.SystemPromptFrom.Key != "systemPrompt" || len(tpl.Tools) != 0 {
		t.Errorf("ta-config template %+v; want ta-config-model, ConfigMap reference, no inline prompt and no tools", tpl)
	}
	data, _ := prompt["data"].(map[string]any)
	if data["systemPrompt"] == "" {
		t.Error("ta-config-prompt has no non-empty systemPrompt key")
	}
	if meta, _ := prompt["metadata"].(map[string]any); meta["name"] != "ta-config-prompt" || meta["namespace"] != "kagent" {
		t.Errorf("prompt metadata %v; want ta-config-prompt in kagent", meta)
	}
	if meta, _ := model["metadata"].(map[string]any); meta["name"] != "ta-config-model" || meta["namespace"] != "kagent" {
		t.Errorf("model metadata %v; want ta-config-model in kagent", meta)
	}
	spec, _ := model["spec"].(map[string]any)
	openAI, _ := spec["openAI"].(map[string]any)
	if spec["provider"] != "OpenAI" || spec["model"] != "ta-config-model" || openAI["baseUrl"] != "http://ta-config-model.kagent.svc.cluster.local:8080/v1" {
		t.Errorf("model spec %v; want the dedicated OpenAI-compatible capture service", spec)
	}
}

func TestTAConfigCaptureKeepsFullSystemPrompt(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(taConfigDir, "capture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"system_prompt", "authorization_present", "/v1/chat/completions", "stream"} {
		if !strings.Contains(s, want) {
			t.Errorf("capture fixture has no %q", want)
		}
	}
	if strings.Contains(s, `"authorization": self.headers.get`) {
		t.Error("capture fixture records the Authorization value instead of a presence boolean")
	}
	if strings.Contains(s, "[:60]") || strings.Contains(s, "[:80]") {
		t.Error("capture fixture truncates the system prompt")
	}
	model, err := os.ReadFile(filepath.Join(taConfigDir, "model.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(model), "X-TA-Config-Capture") {
		t.Error("ta-config model does not identify capture requests")
	}
}

func TestTAConfigApplyInstallsDependenciesBeforeAgent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(taConfigDir, "apply.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	prompt := strings.Index(s, "prompt.yaml")
	agent := strings.Index(s, "agent.yaml")
	if prompt < 0 || agent < 0 || prompt > agent {
		t.Fatalf("apply.sh does not apply prompt dependencies before the Agent: prompt=%d agent=%d", prompt, agent)
	}
	if !strings.Contains(s, "wait --for=condition=Ready agent/ta-config") {
		t.Error("apply.sh does not wait for ta-config to become Ready")
	}
}

func TestTAConfigApplyGuardsFullTierBeforeApply(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(taConfigDir, "apply.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Contains(s, "KIND_CONTEXT") {
		t.Error("apply.sh accepts an arbitrary KIND_CONTEXT; the fixture must target the owned local full tier")
	}
	for _, want := range []string{
		`. "${HERE}/../../lib.sh"`,
		"use_tier full",
		"CTX=kind-${CLUSTER}",
		`K=(kubectl --context "${CTX}")`,
		"cluster_exists",
		"assert_owned",
		"assert_context full",
		"refuse_while_sandbox_up",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("apply.sh has no full-tier guard %q", want)
		}
	}
	firstApply := strings.Index(s, `"${K[@]}" apply`)
	if firstApply < 0 {
		t.Fatal("apply.sh has no kubectl apply")
	}
	for _, guard := range []string{"cluster_exists", "assert_owned", "assert_context full", "refuse_while_sandbox_up"} {
		if at := strings.Index(s, guard); at < 0 || at > firstApply {
			t.Errorf("apply.sh guard %q occurs at %d; want it before the first apply at %d", guard, at, firstApply)
		}
	}
}
