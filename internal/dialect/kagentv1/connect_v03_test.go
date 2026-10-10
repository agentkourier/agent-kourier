package kagentv1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// v03Card is the card kagent 0.9.4 serves: A2A 0.3, so one `url` and a `preferredTransport`, and
// no protocolVersion and no supportedInterfaces. The URL is the controller's, which the dialect
// must not follow.
func v03Card(transport string) http.HandlerFunc { return v03CardListing(transport) }

// v03CardListing is v03Card with the extensions its capabilities list.
func v03CardListing(transport string, extensions ...string) http.HandlerFunc {
	exts := make([]string, len(extensions))
	for i, e := range extensions {
		exts[i] = fmt.Sprintf(`{"uri": %q}`, e)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
  "name": "sre_investigator",
  "description": "A read-only SRE assistant",
  "url": "http://kagent-controller.invalid:8083%s/",
  "preferredTransport": %q,
  "version": "",
  "capabilities": {"streaming": true, "pushNotifications": false, "stateTransitionHistory": true, "extensions": [%s]},
  "defaultInputModes": ["text"],
  "defaultOutputModes": ["text"],
  "skills": [{"id": "investigate-alert", "name": "Investigate an alert", "description": "reads the cluster", "tags": ["sre"]}]
}`, agentPath, transport, strings.Join(exts, ","))
	}
}

// v03Agent answers over A2A 0.3 JSON-RPC (message/stream), as kagent 0.9.4 does, with one artifact
// and a completed task.
func v03Agent(answer string) http.HandlerFunc {
	exec := a2asrv.AgentExecutorFunc(func(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			for _, ev := range []a2a.Event{
				a2a.NewSubmittedTask(ec, ec.Message),
				a2a.NewArtifactEvent(ec, a2a.NewTextPart(answer)),
				a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil),
			} {
				if !yield(ev, nil) {
					return
				}
			}
		}
	})
	return a2av0.NewJSONRPCHandler(a2asrv.NewHandler(exec)).ServeHTTP
}

// TestConnectSpeaksA2AV03: kagent 0.9.4 cannot be reached by the 1.0 transport alone. Its card lists no
// supportedInterfaces, which made the dialect refuse it ("agent card lists no JSON-RPC interface"),
// and it expects 0.3 method names. The card's own URL must still not be followed.
func TestConnectSpeaksA2AV03(t *testing.T) {
	const answer = "the pods in kagent are healthy"
	fd := newFrontDoor(t, false, v03Card("JSONRPC"), v03Agent(answer))

	client, err := New(0).Connect(t.Context(), fd.backend(), agent, dialect.Credentials{Token: secret.New(canary)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	var text strings.Builder
	var final a2a.TaskState
	for ev, err := range client.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("are the pods healthy?")),
	}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		switch e := ev.(type) {
		case *a2a.TaskArtifactUpdateEvent:
			for _, p := range e.Artifact.Parts {
				text.WriteString(p.Text())
			}
		case *a2a.TaskStatusUpdateEvent:
			final = e.Status.State
		}
	}
	if text.String() != answer {
		t.Errorf("artifact text = %q, want %q", text.String(), answer)
	}
	if final != a2a.TaskStateCompleted {
		t.Errorf("final state = %v, want completed", final)
	}
	if n := len(fd.seen(http.MethodPost)); n != 1 {
		t.Errorf("front door got %d sends, want 1 (the card's :8083 URL was followed, or the send was retried)", n)
	}
	// The identity goes on the 0.3 request too, to the front door only.
	if got := fd.seen(http.MethodPost)[0].Get("Authorization"); got != "Bearer "+canary {
		t.Errorf("Authorization on the 0.3 send = %q, want the Binding's bearer token", got)
	}
}

// TestConnectRejectsAnA2AV03CardWithoutJSONRPC keeps the guard: a 0.3 card whose only transport is
// something else (here gRPC) lists no JSON-RPC interface and is refused as before.
func TestConnectRejectsAnA2AV03CardWithoutJSONRPC(t *testing.T) {
	fd := newFrontDoor(t, false, v03Card("GRPC"), v03Agent("unused"))
	_, err := New(0).Connect(t.Context(), fd.backend(), agent, dialect.Credentials{Token: secret.New(canary)})
	if err == nil || !strings.Contains(err.Error(), "no JSON-RPC interface") {
		t.Fatalf("Connect error = %v, want %q", err, "agent card lists no JSON-RPC interface")
	}
}

// TestConnectPrefersTheA2AV1InterfaceOfACardThatListsBoth: an agent may serve a card that is a union of both
// formats (the SDK's own server does). It carries supportedInterfaces, so it is read as 1.0 and talks 1.0;
// the 0.3 fields beside them are not what picks the transport. The front door here only answers 1.0.
func TestConnectPrefersTheA2AV1InterfaceOfACardThatListsBoth(t *testing.T) {
	union := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
  "name": "spike-ask", "version": "1",
  "supportedInterfaces": [{"url": "http://kagent-controller.invalid:8083%s", "protocolBinding": "JSONRPC", "protocolVersion": "1.0"}],
  "url": "http://kagent-controller.invalid:8083%s", "preferredTransport": "JSONRPC", "protocolVersion": "0.3.0",
  "capabilities": {"streaming": true}
}`, agentPath, agentPath)
	}
	fd := newFrontDoor(t, false, union, nil) // the default send path replays a 1.0 stream
	task, err := ask(t.Context(), fd, dialect.Credentials{Token: secret.New(canary)})
	if err != nil {
		t.Fatalf("ask: %v (the 0.3 transport was chosen, or the card was refused)", err)
	}
	if task == nil || task.Status.Message == nil {
		t.Errorf("task = %+v, want the 1.0 stream's final task", task)
	}
}

// TestA2AV03TransportCarriesTheExtensionsTheCardLists: the dialect asks for the extensions it understands
// whenever the card lists them. The 0.3 transport must carry that request too, in the legacy header name, or
// an agent that lists the HITL extension would never be asked for its payload.
func TestA2AV03TransportCarriesTheExtensionsTheCardLists(t *testing.T) {
	for _, tc := range []struct {
		name string
		card http.HandlerFunc
		want bool
	}{
		{"card lists the extension", v03CardListing("JSONRPC", HITLExtension), true},
		{"card lists none", v03CardListing("JSONRPC"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd := newFrontDoor(t, false, tc.card, v03Agent("ok"))
			client, err := New(0).Connect(t.Context(), fd.backend(), agent, dialect.Credentials{Token: secret.New(canary)})
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range client.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{
				Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi")),
			}) {
				if err != nil {
					t.Fatal(err)
				}
			}
			var got bool
			for k, vs := range fd.seen(http.MethodPost)[0] {
				if strings.Contains(strings.ToLower(k), "a2a-extensions") && strings.Contains(strings.Join(vs, ","), HITLExtension) {
					got = true
				}
			}
			if got != tc.want {
				t.Errorf("extension requested on the 0.3 send = %v, want %v (headers %v)", got, tc.want, fd.seen(http.MethodPost)[0])
			}
		})
	}
}

// TestListTasksRefusalIsLocalOn03AndAWireErrorOn10 pins, on the real client, the line internal/agent relies on to
// tell "this agent has no task list" from "this agent is busy": the 0.3 transport refuses the list itself, with
// the bare sentinel; a 1.0 agent's -32004 arrives as an *a2a.Error wrapping the same sentinel.
func TestListTasksRefusalIsLocalOn03AndAWireErrorOn10(t *testing.T) {
	list := func(t *testing.T, fd *frontDoor) error {
		t.Helper()
		client, err := New(0).Connect(t.Context(), fd.backend(), agent, dialect.Credentials{Token: secret.New(canary)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ListTasks(t.Context(), &a2a.ListTasksRequest{ContextID: "c1"})
		return err
	}
	t.Run("0.3: refused by the client, no request sent", func(t *testing.T) {
		fd := newFrontDoor(t, false, v03Card("JSONRPC"), v03Agent("unused"))
		err := list(t, fd)
		var wire *a2a.Error
		if !errors.Is(err, a2a.ErrUnsupportedOperation) || errors.As(err, &wire) {
			t.Fatalf("err = %v (%T), want the bare ErrUnsupportedOperation", err, err)
		}
		if n := len(fd.seen(http.MethodPost)); n != 0 {
			t.Errorf("%d requests sent, want none: the 0.3 transport has no tasks/list", n)
		}
	})
	t.Run("1.0: -32004 on the wire is an *a2a.Error", func(t *testing.T) {
		busy := func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req) // the response echoes the request's id, whatever type it has
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32004,"message":"task is not accepting requests"}}`, req.ID)
		}
		fd := newFrontDoor(t, false, nil, busy)
		err := list(t, fd)
		var wire *a2a.Error
		if !errors.Is(err, a2a.ErrUnsupportedOperation) || !errors.As(err, &wire) {
			t.Fatalf("err = %v (%T), want an *a2a.Error wrapping ErrUnsupportedOperation", err, err)
		}
	})
}
