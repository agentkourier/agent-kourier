package kindmanifests_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/config"
)

const fullApprovalConfigDir = "../../hack/kind/full/agent-kourier/config"

func approvalConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, glob := range []string{agentKourierDir + "config/*.yaml", fullApprovalConfigDir + "/*.yaml"} {
		files, err := filepath.Glob(glob)
		if err != nil || len(files) == 0 {
			t.Fatalf("no files match %s: %v", glob, err)
		}
		for _, file := range files {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, filepath.Base(file)), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	raw, err := os.ReadFile("../../hack/kind/full/agent-kourier/agentbackend.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agentbackend.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTA11aApprovalBindingsLoad(t *testing.T) {
	set, err := config.Load(t.Context(), approvalConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := config.Resolve(t.Context(), set, upFastSecrets)
	if err != nil {
		t.Fatal(err)
	}
	for channel, wantAgent := range map[string]string{
		"CS19": "scenario-approval",
		"CS20": "scenario-approval",
		"CS21": "scenario-approval",
		"CS22": "scenario-approval",
		"CS23": "scenario-approval-multi",
		"CS24": "scenario-approval",
	} {
		rb, ok := snap.BindingForChannel("agent-kourier", "slack-fast", channel)
		if !ok {
			t.Errorf("channel %s resolves to no Binding", channel)
			continue
		}
		if rb.Binding.Spec.Agent.Name != wantAgent {
			t.Errorf("channel %s resolves to agent %q; want %q", channel, rb.Binding.Spec.Agent.Name, wantAgent)
		}
		if !rb.Binding.Spec.Interactions.ToolApprovals {
			t.Errorf("channel %s does not enable tool approvals", channel)
		}
		if channel == "CS21" && rb.Binding.Spec.Interactions.Timeout.Duration != 10*time.Second {
			t.Errorf("S21 timeout is %v; want 10s so the timeout fits the E2E step budget", rb.Binding.Spec.Interactions.Timeout)
		}
		wantGroups := []string(nil)
		if channel == "CS22" {
			wantGroups = []string{"S0123456789"}
		}
		if got := rb.Binding.Spec.Interactions.ApproverGroups; !slices.Equal(got, wantGroups) {
			t.Errorf("channel %s has approver groups %v; want %v", channel, got, wantGroups)
		}
	}
	s11, ok := snap.BindingForChannel("agent-kourier", "slack-fast", "CS11")
	if !ok || s11.Binding.Spec.Agent.Name != "spike-approve" || s11.Binding.Spec.Interactions.ToolApprovals {
		t.Errorf("S11 binding changed: %+v, %v; want spike-approve with approvals disabled", s11.Binding, ok)
	}
}

func TestTA11aAgentsRequireApprovalWithDistinctToolServers(t *testing.T) {
	docs := objects(t, "../../hack/kind/full/scenarios/scenarios.yaml")
	for _, tc := range []struct {
		name, scenario, server string
	}{
		{"scenario-approval", "approval", "scenariomcp-approval"},
		{"scenario-approval-multi", "approval-multi", "scenariomcp-approval-multi"},
	} {
		var agent, model map[string]any
		for _, doc := range docs {
			meta, _ := doc["metadata"].(map[string]any)
			if meta["name"] != tc.name {
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
			t.Fatalf("scenarios.yaml has Agent %v and ModelConfig %v named %s; want both", agent != nil, model != nil, tc.name)
		}
		spec, _ := agent["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		tools, _ := template["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("agent %s has %d tools; want one approval-gated MCP server", tc.name, len(tools))
		}
		mcp, _ := tools[0].(map[string]any)["mcp"].(map[string]any)
		server, _ := mcp["server"].(map[string]any)
		if server["kind"] != "RemoteMCPServer" || server["name"] != tc.server || mcp["requireApproval"] != true {
			t.Errorf("agent %s tool is %v; want %s with requireApproval", tc.name, mcp, tc.server)
		}
		modelSpec, _ := model["spec"].(map[string]any)
		headers, _ := modelSpec["defaultHeaders"].(map[string]any)
		if headers["X-Scenario"] != tc.scenario {
			t.Errorf("model %s sends X-Scenario %v; want %s", tc.name, headers["X-Scenario"], tc.scenario)
		}
	}

	// Pin the existing S11 model and Agent while the approval-enabled fixtures are added beside them.
	var s11Agent, s11Model map[string]any
	for _, doc := range docs {
		meta, _ := doc["metadata"].(map[string]any)
		if meta["name"] != "spike-approve" {
			continue
		}
		switch doc["kind"] {
		case "Agent":
			s11Agent = doc
		case "ModelConfig":
			s11Model = doc
		}
	}
	if s11Agent == nil || s11Model == nil {
		t.Fatal("scenarios.yaml lost the existing S11 Agent or ModelConfig")
	}
	spec, _ := s11Agent["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	if template["modelConfig"].(map[string]any)["name"] != "spike-approve" ||
		template["description"] != "S11: calls a tool that needs approval, then says what became of it" ||
		template["systemPrompt"] != "Use the tool." {
		t.Errorf("S11 Agent changed: %v", template)
	}
	tools, _ := template["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("S11 has %d tools; want its original one", len(tools))
	}
	mcp, _ := tools[0].(map[string]any)["mcp"].(map[string]any)
	server, _ := mcp["server"].(map[string]any)
	if server["kind"] != "RemoteMCPServer" || server["name"] != "scenariomcp" || mcp["requireApproval"] != true {
		t.Errorf("S11 tool changed: %v", mcp)
	}
	modelSpec, _ := s11Model["spec"].(map[string]any)
	if modelSpec["defaultHeaders"].(map[string]any)["X-Scenario"] != "approve" {
		t.Errorf("S11 model header changed: %v", modelSpec["defaultHeaders"])
	}
}

func TestTA11aScriptHasDistinctToolIDsAndInspectableCounters(t *testing.T) {
	llm := readTA11aFile(t, "../../hack/kind/full/scenarios/scenariollm.py")
	mcp := readTA11aFile(t, "../../hack/kind/full/scenarios/scenariomcp.py")
	for _, want := range []string{`scenario == "approval"`, `scenario == "approval-multi"`, "script.tools", "index", "call_scenario_"} {
		if !strings.Contains(llm, want) {
			t.Errorf("scenariollm.py has no %q", want)
		}
	}
	for _, want := range []string{"X-Scenario", "_control/counters", "tools/call", "counters"} {
		if !strings.Contains(mcp, want) {
			t.Errorf("scenariomcp.py has no %q", want)
		}
	}
	if python, err := exec.LookPath("python3"); err == nil {
		probe := `import importlib.util, json
spec = importlib.util.spec_from_file_location("scenario", "../../hack/kind/full/scenarios/scenariollm.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
script = module.play("approval-multi", [{"role": "user", "content": "use the tools"}])
print(json.dumps(module.completion(script)["choices"][0]["message"]["tool_calls"]))`
		probeCmd := exec.CommandContext(t.Context(), python, "-c", probe)
		probeCmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		out, err := probeCmd.Output()
		if err != nil {
			t.Fatalf("multi-tool scripted model probe failed: %v", err)
		}
		var calls []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(out, &calls); err != nil {
			t.Fatal(err)
		}
		if len(calls) != 2 || calls[0].ID == "" || calls[0].ID == calls[1].ID {
			t.Errorf("multi-tool scripted model returned calls %v; want two distinct IDs", calls)
		}
	}

	var headers = map[string]string{}
	for _, doc := range objects(t, "../../hack/kind/full/scenarios/servers.yaml") {
		meta, _ := doc["metadata"].(map[string]any)
		if doc["kind"] != "RemoteMCPServer" || (meta["name"] != "scenariomcp-approval" && meta["name"] != "scenariomcp-approval-multi") {
			continue
		}
		spec, _ := doc["spec"].(map[string]any)
		from, _ := spec["headersFrom"].([]any)
		if len(from) != 1 {
			t.Fatalf("RemoteMCPServer %v has headersFrom %v; want one scenario header", meta["name"], spec["headersFrom"])
		}
		header, _ := from[0].(map[string]any)
		headers[meta["name"].(string)] = fmt.Sprint(header["value"])
	}
	if headers["scenariomcp-approval"] != "approval" || headers["scenariomcp-approval-multi"] != "approval-multi" ||
		headers["scenariomcp-approval"] == headers["scenariomcp-approval-multi"] {
		t.Errorf("approval MCP scenario headers are not distinct: %v", headers)
	}
}

func TestTA11aMCPCallCountersAreIsolatedBetweenScenarios(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to exercise the scenario MCP control endpoint")
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	cmd := exec.CommandContext(t.Context(), python, "../../hack/kind/full/scenarios/scenariomcp.py", strconv.Itoa(port))
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/_control/counters", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatal("scenario MCP did not expose its counter control endpoint")
	}
	for _, scenario := range []string{"approval", "approval", "approval-multi"} {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_numbers","arguments":{"a":3,"b":5}}}`)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/mcp", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scenario", scenario)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("tools/call for %s returned HTTP %d", scenario, resp.StatusCode)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/_control/counters", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var counters map[string]map[string]int
	if err := json.NewDecoder(resp.Body).Decode(&counters); err != nil {
		t.Fatal(err)
	}
	if counters["approval"]["add_numbers"] != 2 || counters["approval-multi"]["add_numbers"] != 1 {
		t.Errorf("counters %v; want approval=2 and approval-multi=1 with isolated buckets", counters)
	}
}

func readTA11aFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
