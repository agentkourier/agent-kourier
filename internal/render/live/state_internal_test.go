package live

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// TestHandleAfterTheEndStoresNothing: events that arrive once the run is over are dropped, not kept.
func TestHandleAfterTheEndStoresNothing(t *testing.T) {
	u := &Run{texts: map[a2a.ArtifactID]string{}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	close(u.done)
	u.Handle(agent.TextEvent{ArtifactID: "a", Text: "late"})
	if len(u.texts) != 0 || len(u.order) != 0 {
		t.Fatalf("a run that ended kept %v", u.texts)
	}
}

// TestAToolCallIsNoStreamContent: with only a tool call to show, the update path has a message (its
// status line) and a stream has nothing, because a stream has no tool lines (v1) and must not start
// on an empty text.
func TestAToolCallIsNoStreamContent(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		u := &Run{streaming: streaming, texts: map[a2a.ArtifactID]string{}, wake: make(chan struct{}, 1), done: make(chan struct{})}
		u.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
		if _, _, _, ok := u.snapshot(); ok == streaming {
			t.Errorf("streaming=%v: a lone tool call gives content: %v; want it only on the update path", streaming, ok)
		}
	}
}
