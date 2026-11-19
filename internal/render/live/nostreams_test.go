package live_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// The tests below pin the step list of a platform that shows a message's steps and has no streams, as Microsoft Teams does:
// every turn is on the update path, a mention's as an alert's, so its first post shows the status line and each update after
// it shows the tool calls as steps, the latest 20 under a line that counts the earlier ones, the final update included.

// listFake is a FakePlatform with steps and no streams, which keeps every message that an update gave it, in order.
type listFake struct {
	*chattest.FakePlatform
	mu      sync.Mutex
	updates []chat.Message
}

func (f *listFake) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	err := f.FakePlatform.Update(ctx, r, o)
	if err == nil && o.Message != nil {
		f.mu.Lock()
		f.updates = append(f.updates, *o.Message)
		f.mu.Unlock()
	}
	return err
}

// shown is the messages that updates gave the platform so far.
func (f *listFake) shown() []chat.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.updates)
}

// last is the latest message an update gave the platform; the zero Message before the first.
func (f *listFake) last() chat.Message {
	if u := f.shown(); len(u) > 0 {
		return u[len(u)-1]
	}
	return chat.Message{}
}

// setupList is the renderer over a listFake, with the outbox over it too, so that the first post of a turn lands there.
func setupList(t *testing.T) (*env, *listFake) {
	t.Helper()
	e := setup(t)
	fake := chattest.NewFake()
	caps := fake.Capabilities()
	caps.Streams, caps.Steps = false, true
	fake.SetCaps(caps)
	lf := &listFake{FakePlatform: fake}
	e.use(lf)
	e.cfg.Sender = newOutbox(t, outbox.Config{Store: e.store, Platforms: chattest.Same[outbox.Platform](lf), Logger: testutil.DiscardLogger()})
	e.live = newRenderer(t, e.cfg)
	return e, lf
}

// listed is a message's step list: the line above the steps, if there is one, and the steps.
type listed struct {
	earlier string
	steps   []chat.Step
}

// listOf reads the step list of m: the Steps part, and a Context part just before it (chat.Steps). ok is false for a message
// with no Steps part.
func listOf(m chat.Message) (l listed, ok bool) {
	for i, p := range m.Parts {
		st, isSteps := p.(chat.Steps)
		if !isSteps {
			continue
		}
		if i > 0 {
			if c, isContext := m.Parts[i-1].(chat.Context); isContext {
				l.earlier = c.Text
			}
		}
		l.steps = st.Steps
		return l, true
	}
	return listed{}, false
}

// bodyOf is the text of m's Body, if it has one.
func bodyOf(m chat.Message) string {
	for _, p := range m.Parts {
		if b, ok := p.(chat.Body); ok {
			return b.Markdown
		}
	}
	return ""
}

// Each update shows the list in the state of the moment: a call under way is running, a call whose result came is done, or
// failed when the result says so, and the final keeps the list as a record beside the answer and its footer. The first post,
// which the outbox stores, carries no steps and shows the status line. Mentions and alerts are listed alike.
func TestAPlatformWithoutStreamsListsTheToolCallsInEachUpdate(t *testing.T) {
	for _, recipient := range []string{"U1", ""} {
		t.Run(fmt.Sprintf("recipient %q", recipient), func(t *testing.T) {
			e, lf := setupList(t)
			run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: v1alpha1.ProgressSteps, Recipient: recipient, Footer: footer})
			get := call("c1", "get_pods", map[string]any{"ns": "prod"})
			run.Handle(get)
			testutil.Eventually(t, "the first post", func() bool { return len(lf.Messages()) == 1 })
			post := lf.Messages()[0].Out.Message
			if _, ok := listOf(*post); ok || !slices.ContainsFunc(post.Parts, func(p chat.Part) bool {
				tl, isLine := p.(chat.ToolLine)
				return isLine && tl.Name == "get_pods" && tl.State == chat.ToolCalling
			}) {
				t.Fatalf("first post %+v; want the status line of the call under way and no steps: the outbox stores none", post.Parts)
			}

			e.window(t) // the update that puts the list in place of the line
			running := []chat.Step{render.Step("s1", get, chat.StepRunning)}
			testutil.Eventually(t, "the list with the call running", func() bool {
				l, ok := listOf(lf.last())
				return ok && slices.Equal(l.steps, running)
			})

			logs := call("c2", "get_logs", nil)
			run.Handle(result("c1", map[string]any{"items": 3}))
			run.Handle(logs)
			run.Handle(result("c2", map[string]any{"error": "forbidden"}))
			e.window(t)
			ended := []chat.Step{render.Step("s1", get, chat.StepDone), render.Step("s2", logs, chat.StepFailed)}
			testutil.Eventually(t, "the list with a call done and one failed", func() bool {
				l, ok := listOf(lf.last())
				return ok && slices.Equal(l.steps, ended)
			})

			run.Handle(text("a", "The pods cannot read their logs."))
			run.Handle(agent.FinalEvent{})
			e.finish(t, run)
			final := lf.last()
			l, ok := listOf(final)
			if !ok || !slices.Equal(l.steps, ended) || l.earlier != "" {
				t.Fatalf("final list %+v (%t); want %+v and no line of earlier steps: the list stays as a record", l, ok, ended)
			}
			if bodyOf(final) != "The pods cannot read their logs." {
				t.Errorf("final body %q; want the answer", bodyOf(final))
			}
			if !slices.ContainsFunc(final.Parts, func(p chat.Part) bool { _, isButtons := p.(chat.Buttons); return isButtons }) {
				t.Errorf("final %+v; want the footer's buttons", final.Parts)
			}
			for i, m := range lf.shown() {
				for _, p := range m.Parts {
					if _, isLine := p.(chat.ToolLine); isLine {
						t.Errorf("update %d shows a status line beside the list, which says the same: %+v", i, m.Parts)
					}
				}
			}
		})
	}
}

// A turn of more calls than the list holds shows the latest 20, each a call with its result, under a line that counts the
// ones it no longer shows; a step keeps its ID when the ones before it fold. The line is a Context part just before the
// steps, which a platform that moves the steps moves with them (chat.Steps).
func TestAPlatformWithoutStreamsFoldsTheEarlierSteps(t *testing.T) {
	const calls, listedMax = 25, 20 // listedMax is the stepsListed row of the limits page, which its guard holds to maxListed
	e, lf := setupList(t)
	run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: v1alpha1.ProgressSteps, Recipient: "U1"})
	var made []agent.ToolCallEvent
	for i := range calls {
		c := call(fmt.Sprintf("c%d", i+1), fmt.Sprintf("tool_%d", i+1), nil)
		made = append(made, c)
		run.Handle(c)
		run.Handle(result(c.CallID, map[string]any{"ok": true}))
	}
	run.Handle(text("a", "Done."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)

	var want []chat.Step
	for i := calls - listedMax; i < calls; i++ {
		want = append(want, render.Step(fmt.Sprintf("s%d", i+1), made[i], chat.StepDone))
	}
	l, ok := listOf(lf.last())
	if !ok || !slices.Equal(l.steps, want) {
		t.Fatalf("final list %+v (%t); want the latest %d calls, s%d to s%d, done", l.steps, ok, listedMax, calls-listedMax+1, calls)
	}
	if wantLine := render.EarlierSteps(calls - listedMax); l.earlier != wantLine {
		t.Errorf("the line above the steps is %q; want %q", l.earlier, wantLine)
	}
	if bodyOf(lf.last()) != "Done." {
		t.Errorf("final body %q; want the answer", bodyOf(lf.last()))
	}
}

// The activity mode names no tool on any platform: a platform without streams shows its one Working… line, and no list.
func TestAPlatformWithoutStreamsListsNothingInTheActivityMode(t *testing.T) {
	e, lf := setupList(t)
	run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: v1alpha1.ProgressActivity, Recipient: "U1"})
	run.Handle(call("c1", "get_pods", nil))
	run.Handle(result("c1", map[string]any{"items": 3}))
	run.Handle(text("a", "Three pods."))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	for i, m := range append(lf.shown(), *lf.Messages()[0].Out.Message) {
		if _, ok := listOf(m); ok {
			t.Errorf("message %d shows a list in the activity mode: %+v", i, m.Parts)
		}
	}
}
