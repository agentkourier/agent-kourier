package docsagent_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool/mcptoolset"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsagent"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/scriptedmodel"
)

// startV03 starts the agent with its A2A 0.3 surface on, naming the server's own root.
func startV03(t *testing.T) *agentServer {
	t.Helper()
	var root string
	a := startAgentWith(t, &scriptedmodel.Model{}, func(c *docsagent.Config) {
		root = strings.TrimSuffix(c.PublicURL, "a2a")
		c.V03URL = root
	})
	if a.url+"/" != root {
		t.Fatalf("the test server is at %s, the card names %s", a.url, root)
	}
	return a
}

// TestV03CardIsWhereKagentReadsIt pins the card kagent 0.9's Python runtime and its readiness probe read: at the root,
// in the 0.3 shape, naming the 0.3 endpoint as url with JSON-RPC as the preferred transport.
func TestV03CardIsWhereKagentReadsIt(t *testing.T) {
	a := startV03(t)
	resp := get(t, a.url+"/.well-known/agent-card.json")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("card status = %d", resp.StatusCode)
	}
	var card map[string]any
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("card is not JSON: %v", err)
	}
	want := map[string]any{"url": a.url + "/", "preferredTransport": "JSONRPC", "protocolVersion": "0.3", "name": "docs-agent"}
	for k, v := range want {
		if card[k] != v {
			t.Errorf("card %s = %v, want %v", k, card[k], v)
		}
	}
	// The fields a2a-sdk 0.3's AgentCard requires.
	for _, k := range []string{"description", "version", "capabilities", "defaultInputModes", "defaultOutputModes", "skills"} {
		if _, ok := card[k]; !ok {
			t.Errorf("card lacks %s: %s", k, body)
		}
	}
}

// TestV03MessageSendAnswers sends what kagent 0.9 sends a sub-agent: a blocking 0.3 message/send with one text part
// and a context ID the agent has never seen. The answer must come back in a task's artifacts, with the kind fields
// that 0.3 clients use to tell a task from a message.
func TestV03MessageSendAnswers(t *testing.T) {
	a := startV03(t)
	body := `{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"configuration":{"blocking":true},` +
		`"message":{"kind":"message","messageId":"m1","role":"user","contextId":"parent-tool-ctx",` +
		`"parts":[{"kind":"text","text":"How do I connect an agent to the relay?"}]}}}`
	got := post(t, a.url+"/", body)
	var rpc struct {
		Result struct {
			ID        string `json:"id"`
			Kind      string `json:"kind"`
			ContextID string `json:"contextId"`
			Status    struct {
				State string `json:"state"`
			} `json:"status"`
			Artifacts []struct {
				Parts []struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"artifacts"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(got, &rpc); err != nil {
		t.Fatalf("response is not JSON-RPC: %v: %s", err, got)
	}
	if rpc.Error != nil {
		t.Fatalf("message/send failed: %s", got)
	}
	r := rpc.Result
	if r.Kind != "task" || r.Status.State != "completed" || r.ContextID != "parent-tool-ctx" {
		t.Fatalf("result kind %q, state %q, context %q; want a completed task in the caller's context: %s", r.Kind, r.Status.State, r.ContextID, got)
	}
	var text strings.Builder
	for _, art := range r.Artifacts {
		for _, p := range art.Parts {
			if p.Kind == "text" {
				text.WriteString(p.Text)
			}
		}
	}
	if !strings.Contains(text.String(), "https://docs.example.com/how-to/connect-agent/") {
		t.Errorf("the 0.3 answer %q does not cite the page", text.String())
	}
}

// TestV03AndV1ShareTasks pins that both protocol versions sit on one request handler: a task made over 0.3 is found
// over 1.0, so the two share the conversations and the task store.
func TestV03AndV1ShareTasks(t *testing.T) {
	a := startV03(t)
	got := post(t, a.url+"/", `{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"configuration":{"blocking":true},`+
		`"message":{"kind":"message","messageId":"m2","role":"user","parts":[{"kind":"text","text":"How do I connect an agent to the relay?"}]}}}`)
	var rpc struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(got, &rpc); err != nil || rpc.Result.ID == "" {
		t.Fatalf("no task ID in the 0.3 response (%v): %s", err, got)
	}
	task, err := a.client(t).GetTask(t.Context(), &a2a.GetTaskRequest{ID: a2a.TaskID(rpc.Result.ID)})
	if err != nil {
		t.Fatalf("the 0.3 task is not found over 1.0: %v", err)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("state over 1.0 = %s, want completed", task.Status.State)
	}
}

// TestV03MessageStreamAnswers covers a 0.3 client that streams, as a2a-sdk does when the card says streaming: the
// events carry kind fields, the stream ends with a final completed status, and the answer is in the artifacts.
func TestV03MessageStreamAnswers(t *testing.T) {
	a := startV03(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.url+"/", strings.NewReader(
		`{"jsonrpc":"2.0","id":"1","method":"message/stream","params":{"message":{"kind":"message","messageId":"m3",`+
			`"role":"user","parts":[{"kind":"text","text":"How do I connect an agent to the relay?"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var text strings.Builder
	var final bool
	for line := range strings.SplitSeq(string(body), "\n") {
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev struct {
			Result struct {
				Kind     string                 `json:"kind"`
				Final    bool                   `json:"final"`
				Status   struct{ State string } `json:"status"`
				Artifact struct {
					Parts []struct {
						Kind string `json:"kind"`
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"artifact"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("event is not JSON: %v: %s", err, data)
		}
		switch ev.Result.Kind {
		case "artifact-update":
			for _, p := range ev.Result.Artifact.Parts {
				if p.Kind == "text" {
					text.WriteString(p.Text)
				}
			}
		case "status-update":
			final = final || (ev.Result.Final && ev.Result.Status.State == "completed")
		case "task":
		default:
			t.Errorf("event without a known kind: %s", data)
		}
	}
	if !final {
		t.Errorf("the stream did not end with a final completed status: %s", body)
	}
	if !strings.Contains(text.String(), "https://docs.example.com/how-to/connect-agent/") {
		t.Errorf("the streamed 0.3 answer %q does not cite the page", text.String())
	}
}

// TestV1CardNamesOnlyPublicURL pins the 1.0 card at /a2a: one interface, PublicURL, with or without the 0.3 surface.
func TestV1CardNamesOnlyPublicURL(t *testing.T) {
	for name, a := range map[string]*agentServer{"0.3 off": startAgent(t, &scriptedmodel.Model{}), "0.3 on": startV03(t)} {
		t.Run(name, func(t *testing.T) {
			resp := get(t, a.url+"/a2a/.well-known/agent-card.json")
			defer func() { _ = resp.Body.Close() }()
			var card a2a.AgentCard
			if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
				t.Fatal(err)
			}
			if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].URL != a.url+"/a2a" {
				t.Errorf("1.0 card interfaces = %+v, want only %s/a2a", card.SupportedInterfaces, a.url)
			}
		})
	}
}

// TestV03IsOffByDefault keeps the root free when V03URL is empty: no card, no endpoint.
func TestV03IsOffByDefault(t *testing.T) {
	a := startAgent(t, &scriptedmodel.Model{})
	resp := get(t, a.url+"/.well-known/agent-card.json")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("root card status = %d, want 404", resp.StatusCode)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.url+"/", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST / status = %d, want 404", resp.StatusCode)
	}
}

func TestNewValidatesV03URL(t *testing.T) {
	for _, u := range []string{"/", "ftp://x/", "http://x/a2a", "http://x/?q=1", "http://user:pw@x/"} {
		t.Run(u, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.V03URL = u
			if _, err := docsagent.New(cfg); err == nil {
				t.Fatalf("New accepted V03URL %q", u)
			}
		})
	}
	for _, u := range []string{"http://docs-agent.kagent:8080/", "http://docs-agent.kagent:8080"} {
		cfg := validConfig(t)
		cfg.V03URL = u
		if _, err := docsagent.New(cfg); err != nil {
			t.Errorf("New refused V03URL %q: %v", u, err)
		}
	}
}

// validConfig is a Config that New accepts; its toolset points nowhere, which New does not check.
func validConfig(t *testing.T) docsagent.Config {
	t.Helper()
	tools, err := mcptoolset.New(mcptoolset.Config{Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	return docsagent.Config{Model: &scriptedmodel.Model{}, Tools: tools, PublicURL: "http://x/a2a"}
}

func post(t *testing.T, url, body string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d: %s", url, resp.StatusCode, got)
	}
	return got
}
