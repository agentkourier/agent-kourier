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
