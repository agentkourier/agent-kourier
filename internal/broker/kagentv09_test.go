package broker

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
)

// v09AgentPath is where kagent 0.9.x serves an agent: /api/a2a/{namespace}/{name}/, trailing slash included. The
// name is the one snapshot gives every Binding's agent.
const v09AgentPath = "/api/a2a/kagent/test-agent/"

// newKagentV09 is a server shaped like kagent 0.9.4's controller for one agent. It serves an A2A 0.3 card (one url
// and a preferredTransport, no supportedInterfaces, no HITL extension) under v09AgentPath, answers a POST to the
// path without its trailing slash with a 307, as the 0.9.x installs did, and answers message/stream and tasks/get
// with the turn recorded from kagent 0.9.4 (test/fixtures/kagent/v09-tools), whose tool call and result arrive only
// as status-update messages.
func newKagentV09(t *testing.T) *httptest.Server {
	t.Helper()
	fixture := func(name string) []byte {
		body, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "kagent", "v09-tools", name))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	stream, task := fixture("01-tool-turn.response.sse"), fixture("02-gettask-after-tool-turn.response.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == v09AgentPath+".well-known/agent-card.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"name": "test_agent", "description": "a read-only SRE assistant",
  "url": "http://kagent-controller.invalid:8083%s", "preferredTransport": "JSONRPC", "version": "",
  "capabilities": {"streaming": true, "pushNotifications": false, "stateTransitionHistory": true},
  "defaultInputModes": ["text"], "defaultOutputModes": ["text"], "skills": []}`, v09AgentPath)
		case r.Method == http.MethodPost && r.URL.Path == v09AgentPath[:len(v09AgentPath)-1]:
			http.Redirect(w, r, v09AgentPath, http.StatusTemporaryRedirect)
		case r.Method == http.MethodPost && r.URL.Path == v09AgentPath:
			var call struct{ Method string }
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &call); err != nil {
				t.Errorf("kagent 0.9.4: bad request: %v", err) // not Fatal: this runs on the server's goroutine
			}
			switch call.Method {
			case "message/stream":
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(stream)
			case "tasks/get":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(task)
			default:
				t.Errorf("kagent 0.9.4: unexpected method %q", call.Method)
				http.Error(w, "unexpected method", http.StatusBadRequest)
			}
		default:
			t.Errorf("kagent 0.9.4: unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A Binding whose backend is kagent 0.9.x, at the agent's URL as kagent serves it, gets the agent's tool calls as
// events (which the live renderer shows as step cards) with the kagent-v0.9 dialect. With the generic a2a dialect the
// same turn reaches the agent and shows only its answer: that dialect leaves kagent's tool parts opaque.
func TestKagentV09BindingStreamsToolCalls(t *testing.T) {
	kagent := newKagentV09(t)
	for _, tc := range []struct {
		dialect v1alpha1.Dialect
		want    []string
	}{
		{v1alpha1.DialectKagentV09, []string{"call k8s_get_resources", "result k8s_get_resources", "text The cluster has 46 namespaces."}},
		{v1alpha1.DialectA2A, []string{"text The cluster has 46 namespaces."}},
	} {
		t.Run(string(tc.dialect), func(t *testing.T) {
			f := newFixture(t)
			f.snaps.p.Store(snapshot(t, kagent.URL+v09AgentPath, tc.dialect, time.Hour))
			be, err := f.b.Backend("sre/oncall")
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			var final *a2a.Task
			err = be.Agent.Send(t.Context(), a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("How many namespaces does the cluster have?")), func(ev agent.Event) {
				switch e := ev.(type) {
				case agent.ToolCallEvent:
					got = append(got, "call "+e.Name)
				case agent.ToolResultEvent:
					got = append(got, "result "+e.Name)
				case agent.TextEvent:
					got = append(got, "text "+e.Text)
				case agent.FinalEvent:
					final = e.Task
				}
			})
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("events = %q, want %q", got, tc.want)
			}
			if final == nil || final.Status.State != a2a.TaskStateCompleted {
				t.Errorf("final task = %+v, want COMPLETED", final)
			}
		})
	}
}
