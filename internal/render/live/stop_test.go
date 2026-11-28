package live_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

func hasStopButton(parts []chat.Part, wantValue string) bool {
	for _, p := range parts {
		if btns, ok := p.(chat.Buttons); ok {
			for _, b := range btns.Buttons {
				if b.ID == "stop-task" && b.Label == "Stop" && b.Value == wantValue {
					return true
				}
			}
		}
	}
	return false
}

// On the update path (such as an alert thread or a non-streaming platform), a running turn shows a Stop button bound
// to its task ID, and the final update removes it.
func TestUpdatePathShowsStopButtonWhileRunningAndRemovesItOnFinal(t *testing.T) {
	e := setup(t)
	release := e.hold()
	u := e.live.Start(t.Context(), live.Turn{
		Source: "turn-stop-1", Connection: "ns/conn", Channel: "C1", Thread: e.root, Mode: v1alpha1.OutputLive,
	})
	u.Handle(agent.StateEvent{TaskID: "task-42", ContextID: "c1", State: a2a.TaskStateWorking})
	u.Handle(agent.TextEvent{ArtifactID: "a1", Text: "Checking logs..."})
	release()

	wantVal := render.StopValue{Thread: e.root, TaskID: "task-42"}.String()
	testutil.Eventually(t, "initial post with Stop button", func() bool {
		posts := e.rec.sent("/chat.postMessage")
		return len(posts) == 1 && strings.Contains(posts[0].Get("blocks"), wantVal)
	})

	u.Handle(agent.FinalEvent{Task: &a2a.Task{ID: "task-42", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}})
	testutil.Eventually(t, "throttle timer armed", func() bool { return e.clk.waiting() > 0 })
	e.clk.advance(throttle)
	<-u.Done()

	updates := e.rec.sent("/chat.update")
	if len(updates) == 0 {
		t.Fatalf("expected a final chat.update to remove the Stop button")
	}
	lastBlocks := updates[len(updates)-1].Get("blocks")
	if strings.Contains(lastBlocks, "stop-task") || strings.Contains(lastBlocks, wantVal) {
		t.Errorf("final update still contains Stop button: %s", lastBlocks)
	}
}

// A turn on the update path whose context ends before a FinalEvent removes the Stop button from its message when cut.
func TestUpdatePathRemovesStopButtonWhenCutShort(t *testing.T) {
	e := setup(t)
	release := e.hold()
	ctx, cancel := context.WithCancel(t.Context())
	u := e.live.Start(ctx, live.Turn{
		Source: "turn-stop-cut", Connection: "ns/conn", Channel: "C1", Thread: e.root, Mode: v1alpha1.OutputLive,
	})
	u.Handle(agent.StateEvent{TaskID: "task-cut", ContextID: "c1", State: a2a.TaskStateWorking})
	u.Handle(agent.TextEvent{ArtifactID: "a1", Text: "Working..."})
	release()

	wantVal := render.StopValue{Thread: e.root, TaskID: "task-cut"}.String()
	testutil.Eventually(t, "initial post with Stop button", func() bool {
		posts := e.rec.sent("/chat.postMessage")
		return len(posts) == 1 && strings.Contains(posts[0].Get("blocks"), wantVal)
	})

	cancel()
	testutil.Eventually(t, "throttle timer armed", func() bool { return e.clk.waiting() > 0 })
	e.clk.advance(throttle)
	<-u.Done()

	updates := e.rec.sent("/chat.update")
	if len(updates) == 0 {
		t.Fatalf("expected cut-short chat.update to remove the Stop button")
	}
	if last := updates[len(updates)-1].Get("blocks"); strings.Contains(last, "stop-task") {
		t.Errorf("cut-short update still contains Stop button: %s", last)
	}
}

// A notice that names a stranded task carries a Stop button bound to that thread and task ID; one without a TaskID carries none.
func TestNoticeWithTaskIDCarriesStopButton(t *testing.T) {
	e := setup(t)
	sink := e.sink(t, newOutbox(t, outbox.Config{Store: e.store, Platforms: chattest.Same[outbox.Platform](noCalls{t})}))
	key := session.Key{Connection: "ns/conn", Channel: "C1", Thread: e.root}

	withTask := session.Notice{ID: "task:t99/failed", Kind: session.NoticeFailed, Detail: "the agent refused", TaskID: "t99"}
	sink.Notice(t.Context(), key, withTask)
	row, err := e.store.GetOutbox(t.Context(), outbox.ID(withTask.ID, "notice"))
	if err != nil {
		t.Fatal(err)
	}
	var out chat.Outgoing
	if err := json.Unmarshal([]byte(row.Payload), &out); err != nil {
		t.Fatal(err)
	}
	if out.Message == nil {
		t.Fatalf("out.Message = nil")
	}
	if err := out.Validate(); err != nil {
		t.Errorf("out.Validate() = %v", err)
	}
	wantVal := render.StopValue{Thread: e.root, TaskID: "t99"}.String()
	if !hasStopButton(out.Message.Parts, wantVal) {
		t.Errorf("notice parts = %#v; want Stop button with value %q", out.Message.Parts, wantVal)
	}

	withoutTask := session.Notice{ID: "turn:m1/failed", Kind: session.NoticeFailed, Detail: "the agent refused"}
	sink.Notice(t.Context(), key, withoutTask)
	row2, err := e.store.GetOutbox(t.Context(), outbox.ID(withoutTask.ID, "notice"))
	if err != nil {
		t.Fatal(err)
	}
	var out2 chat.Outgoing
	if err := json.Unmarshal([]byte(row2.Payload), &out2); err != nil {
		t.Fatal(err)
	}
	if hasStopButton(out2.Message.Parts, wantVal) {
		t.Errorf("notice without TaskID unexpectedly carried a Stop button: %#v", out2.Message.Parts)
	}
}
