// Package dialecttest reads the kagent recordings under test/fixtures/kagent for the dialects' tests. Nothing outside
// tests imports it.
package dialecttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// kagentFixture returns the contents of the recording name, a slash-separated path under test/fixtures/kagent.
func kagentFixture(t testing.TB, name string) []byte {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("dialecttest: cannot find the source of the package")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "..", "test", "fixtures", "kagent", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// KagentTask returns the task in the last event of the recorded SSE stream name, which must be a task.
func KagentTask(t testing.TB, name string) a2a.Task {
	t.Helper()
	var last string
	for line := range strings.SplitSeq(string(kagentFixture(t, name)), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			last = data
		}
	}
	var event struct {
		Result struct {
			Task *a2a.Task `json:"task"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(last), &event); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if event.Result.Task == nil {
		t.Fatalf("%s: the last event is not a task", name)
	}
	return *event.Result.Task
}

// KagentRequestMessage returns the message of the recorded request name, a JSON-RPC send.
func KagentRequestMessage(t testing.TB, name string) a2a.Message {
	t.Helper()
	var req struct {
		Params struct {
			Message *a2a.Message `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(kagentFixture(t, name), &req); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if req.Params.Message == nil {
		t.Fatalf("%s: the request has no message", name)
	}
	return *req.Params.Message
}
