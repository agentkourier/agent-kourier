package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
)

// wireToolArtifact is an artifact that holds one tool part: a call, or its result, as kagent 1.0 writes them.
func wireToolArtifact(id string, kind string, data map[string]any) map[string]any {
	return map[string]any{"artifactId": id, "metadata": map[string]any{}, "parts": []any{map[string]any{
		"data": data, "metadata": map[string]any{"kagent.dev/a2a/part-type": kind}}}}
}

// wireEvent is a JSON-RPC response of a stream, as the event that carries result.
func wireEvent(t *testing.T, result map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "1", "result": result})
	if err != nil {
		t.Fatal(err)
	}
	return []byte("data: " + string(b) + "\n\n")
}

// The final task of kagent 1.0 holds every tool call and result of the turn again, in its artifacts (read in
// test/fixtures/kagent/approval/02-approve.response.sse), so what must fit one event is the sum of the tool output of the turn and
// not the largest result: results that each pass one by one end the turn at the final event when they add up past the bound, with
// a reply of a few bytes. This is the real limit on a turn that uses tools, and the docs say so.
func TestFinalTaskRepeatsEveryToolResultSoTheirSumIsWhatMustFitAnEvent(t *testing.T) {
	const size = 128 << 10
	for _, tc := range []struct {
		name    string
		results int
		wantErr bool
	}{
		{"results that add up to less than an event", dialect.MaxEventBytes/size - 2, false},
		{"results that each fit and add up to more than an event", dialect.MaxEventBytes/size + 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := strings.Repeat("x", size)
			var arts []any
			for i := range tc.results {
				arts = append(arts,
					wireToolArtifact(fmt.Sprint("k", i), "function_call", map[string]any{"id": fmt.Sprint("c", i), "name": "kubectl", "args": map[string]any{"a": "b"}}),
					wireToolArtifact(fmt.Sprint("a", i), "function_response", map[string]any{"id": fmt.Sprint("c", i), "name": "kubectl", "response": map[string]any{"output": out}}))
			}
			arts = append(arts, map[string]any{"artifactId": "r", "parts": []any{map[string]any{"text": "done"}}})
			final := wireEvent(t, map[string]any{"task": map[string]any{"id": "t-1", "contextId": "c-1", "status": map[string]any{"state": "TASK_STATE_COMPLETED"}, "artifacts": arts}})
			var events [][]byte
			biggest := 0
			for _, a := range arts {
				ev := wireEvent(t, map[string]any{"artifactUpdate": map[string]any{"taskId": "t-1", "contextId": "c-1", "artifact": a}})
				biggest = max(biggest, len(ev))
				events = append(events, ev)
			}
			fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
				if call.Method != "SendStreamingMessage" {
					return
				}
				write(w, []byte(wireWorking))
				for _, ev := range events {
					write(w, ev)
				}
				write(w, final)
			})
			h, got := collector()
			err := fa.agentViaWith(t, generic.New(0), quick).Send(t.Context(), message("go"), h)
			if biggest >= dialect.MaxEventBytes {
				t.Fatalf("an event of %d bytes is over the bound by itself: the test would not be about the sum", biggest)
			}
			delivered := false
			for _, e := range *got {
				_, ok := e.(FinalEvent)
				delivered = delivered || ok
			}
			switch {
			case tc.wantErr && (!errors.Is(err, dialect.ErrResponseTooLarge) || delivered):
				t.Errorf("err = %v, final delivered = %v; want an event too large at the final task, which is %d bytes", err, delivered, len(final))
			case !tc.wantErr && (err != nil || !delivered):
				t.Errorf("err = %v, final delivered = %v; want the final task, of %d bytes, read", err, delivered, len(final))
			}
		})
	}
}
