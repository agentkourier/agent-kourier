package live_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// These are the golden wire comparisons of WP-37 S3: for a set of turn scripts, every request the renderer makes
// to Slack, in order, and the messages that Slack holds at the end, are kept in testdata/golden. They were
// recorded on the renderer before it was ported to the neutral model, and the port must leave them as they are,
// except where a golden says otherwise in a "changed" note of the commit that changes it. UPDATE_GOLDEN=1 rewrites them.

var (
	tsPattern  = regexp.MustCompile(`\d{10}\.\d{6}`)
	goldenKeys = []string{"channel", "text", "markdown_text", "thread_ts", "ts", "recipient_user_id", "unfurl_links", "unfurl_media", "parse", "link_names", "user"}
)

// goldenRequest is a request to Slack with what matters of its form.
type goldenRequest struct {
	Path string         `json:"path"`
	Form map[string]any `json:"form"`
}

type goldenMessage struct {
	Text      string `json:"text"`
	Blocks    any    `json:"blocks,omitempty"`
	Thread    bool   `json:"thread,omitempty"`
	Streaming bool   `json:"streaming,omitempty"`
	Tasks     []task `json:"tasks,omitempty"`
	Edited    bool   `json:"edited,omitempty"`
	OutboxID  string `json:"outbox_id,omitempty"`
}

// task is a task_update of a streamed message as it ended.
type task struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Details string `json:"details,omitempty"`
	Output  string `json:"output,omitempty"`
}

type golden struct {
	Requests  []goldenRequest     `json:"requests"`
	Thread    []goldenMessage     `json:"thread"`
	Reactions map[string][]string `json:"reactions,omitempty"`
}

// normalizer gives each timestamp that Slack made a name by its first appearance, so that goldens do not depend on them.
type normalizer struct {
	root  string
	names map[string]string
}

func (n *normalizer) text(s string) string {
	if s == n.root {
		return "ROOT"
	}
	return tsPattern.ReplaceAllStringFunc(s, func(ts string) string {
		if ts == n.root {
			return "ROOT"
		}
		if _, ok := n.names[ts]; !ok {
			n.names[ts] = "TS" + strconv.Itoa(len(n.names)+1)
		}
		return n.names[ts]
	})
}

func (n *normalizer) value(v any) any {
	switch v := v.(type) {
	case string:
		return n.text(v)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = n.value(e)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range v {
			out[k] = n.value(e)
		}
		return out
	}
	return v
}

// record is everything Slack received and now holds, normalized.
func (e *env) record(t *testing.T) golden {
	t.Helper()
	n := &normalizer{root: e.root, names: map[string]string{}}
	var g golden
	seen := map[string]int{}
	for _, path := range e.rec.orderOf() {
		if !strings.HasPrefix(path, "/chat.") && path != "/reactions.add" {
			continue
		}
		forms := e.rec.sent(path)
		form := forms[seen[path]]
		seen[path]++
		req := goldenRequest{Path: path, Form: map[string]any{}}
		for _, k := range goldenKeys {
			if form.Has(k) {
				req.Form[k] = n.text(form.Get(k))
			}
		}
		if raw := form.Get("blocks"); raw != "" {
			var blocks any
			if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
				t.Fatal(err)
			}
			req.Form["blocks"] = n.value(blocks)
		}
		if raw := form.Get("chunks"); raw != "" {
			var chunks any
			if err := json.Unmarshal([]byte(raw), &chunks); err != nil {
				t.Fatal(err)
			}
			req.Form["chunks"] = n.value(chunks)
		}
		if raw := form.Get("metadata"); raw != "" {
			var md struct {
				Payload map[string]any `json:"event_payload"`
			}
			_ = json.Unmarshal([]byte(raw), &md)
			req.Form["outbox_id"] = md.Payload["outbox_id"]
		}
		g.Requests = append(g.Requests, req)
	}
	g.Reactions = map[string][]string{}
	for _, m := range e.fake.Thread("C1", e.root) {
		gm := goldenMessage{Text: n.text(m.Text), Thread: m.ThreadTS != "", Streaming: m.Streaming, Edited: m.Edited != nil}
		if len(m.Blocks) > 0 {
			var blocks any
			if err := json.Unmarshal(m.Blocks, &blocks); err != nil {
				t.Fatal(err)
			}
			gm.Blocks = n.value(blocks)
		}
		for _, tk := range m.Tasks() {
			gm.Tasks = append(gm.Tasks, task(tk))
		}
		if m.Metadata != nil {
			gm.OutboxID, _ = m.Metadata.EventPayload["outbox_id"].(string)
		}
		g.Thread = append(g.Thread, gm)
		for _, r := range e.fake.Reactions("C1", m.TS) {
			g.Reactions[n.text(m.TS)] = append(g.Reactions[n.text(m.TS)], r.Name)
		}
	}
	if len(g.Reactions) == 0 {
		g.Reactions = nil
	}
	return g
}

// writes is how many requests the renderer has made to Slack.
func (e *env) writes() int {
	n := 0
	for _, path := range e.rec.orderOf() {
		if strings.HasPrefix(path, "/chat.") || path == "/reactions.add" {
			n++
		}
	}
	return n
}

// settle turns the clock for the renderer until it has made writes requests in all, and returns the moment it has.
// A script's steps come out in the same order every time because each waits for what the step before it makes,
// and never for a time to pass: the renderer's goroutine may be as slow as a loaded machine makes it, and an
// event that reached it before it had written the step before would be written with it, which is a different
// wire. The clock is turned only while the renderer waits for its window, and only until it has written what it
// owes, so that a window that opens does so for the events that were handled before it.
func (e *env) settle(t *testing.T, writes int) {
	t.Helper()
	eventually(t, "the renderer to make the requests of the step", func() bool {
		if e.writes() < writes && e.clk.waiting() > 0 {
			e.clk.advance(throttle)
		}
		return e.writes() >= writes
	})
}

func checkGolden(t *testing.T, name string, got golden) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".json")
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden %s: record it with UPDATE_GOLDEN=1 (%v)", path, err)
	}
	var wantG golden
	if err := json.Unmarshal(want, &wantG); err != nil {
		t.Fatal(err)
	}
	var gotG golden
	_ = json.Unmarshal(raw, &gotG)
	if !reflect.DeepEqual(gotG, wantG) {
		t.Errorf("the wire differs from the golden %s:\n--- got\n%s--- want\n%s", path, raw, want)
	}
}

// checkStepFieldsOnce fails if any step ID has its details, or its output, in more than one task_update chunk. Slack
// appends both to the card's (observed 2026-10-18), so a step that sent them twice shows them twice.
func checkStepFieldsOnce(t *testing.T, g golden) {
	t.Helper()
	details, output := map[string]int{}, map[string]int{}
	for _, r := range g.Requests {
		chunks, _ := r.Form["chunks"].([]any)
		for _, raw := range chunks {
			c, _ := raw.(map[string]any)
			if c["type"] != "task_update" {
				continue
			}
			id, _ := c["id"].(string)
			if d, _ := c["details"].(string); d != "" {
				details[id]++
			}
			if o, _ := c["output"].(string); o != "" {
				output[id]++
			}
		}
	}
	for id, n := range details {
		if n > 1 {
			t.Errorf("step %s sent its details in %d chunks; want one, since Slack appends them", id, n)
		}
	}
	for id, n := range output {
		if n > 1 {
			t.Errorf("step %s sent its output in %d chunks; want one, since Slack appends it", id, n)
		}
	}
}

// scripts are the turns that are recorded. Each is a function of the environment that drives a turn to its end.
var scripts = []struct {
	name string
	run  func(t *testing.T, e *env)
}{
	{"plain-answer-in-an-alert-thread", func(t *testing.T, e *env) { // live mode, no recipient: post, then updates
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
		run.Handle(text("a", "Looking into it"))
		e.settle(t, 1) // the post
		run.Handle(text("a", "Looking into it. The pod restarted twice."))
		e.settle(t, 2) // the update
		run.Handle(text("a", "Looking into it. The pod restarted twice.\n\nCause: OOM."))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"tools-with-secrets-in-their-arguments", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
		run.Handle(text("a", "Checking"))
		e.settle(t, 1) // the post
		run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "kubectl_get", Args: map[string]any{"namespace": "prod", "resource": "pods", "password": "hunter2", "url": "https://user:s3cr3t@host.test/x"}})
		e.settle(t, 2) // the update with the call's card, under way
		run.Handle(agent.ToolResultEvent{CallID: "c1", Name: "kubectl_get"})
		run.Handle(text("a", "Checking: three pods"))
		e.settle(t, 3) // the update with the card done
		run.Handle(agent.ToolCallEvent{CallID: "c2", Name: "search"})
		run.Handle(text("a", "Checking: three pods, all ready"))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"tools-with-secrets-on-a-platform-without-steps", func(t *testing.T, e *env) { // every write shows the status line
		e.spy(t, false, nil)
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
		run.Handle(text("a", "Checking"))
		e.settle(t, 1) // the post
		run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "kubectl_get", Args: map[string]any{"namespace": "prod", "resource": "pods", "password": "hunter2", "url": "https://user:s3cr3t@host.test/x"}})
		e.settle(t, 2) // the update with the tool line
		run.Handle(agent.ToolResultEvent{CallID: "c1", Name: "kubectl_get"})
		run.Handle(text("a", "Checking: three pods"))
		e.settle(t, 3) // the update without it
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"a-tool-call-alone-ends-as-its-card", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
		run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "slow_query", Args: map[string]any{"q": "select 1"}})
		e.settle(t, 1) // the post of the tool line alone
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"a-stream-in-a-chat-thread", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
		run.Handle(text("a", "Hel"))
		e.settle(t, 1) // the stream starts
		run.Handle(text("a", "Hello wor"))
		run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
		e.settle(t, 2) // the append
		run.Handle(text("a", "Hello world, and more"))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"a-stream-with-tool-steps", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
		run.Handle(text("a", "Checking the pods."))
		e.settle(t, 1) // the stream starts
		run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "kubectl_get", Args: map[string]any{"namespace": "prod", "resource": "pods", "password": "hunter2", "url": "https://user:s3cr3t@host.test/x"}})
		e.settle(t, 2) // the step begins, after the text that came before the call
		run.Handle(agent.ToolResultEvent{CallID: "c1", Name: "kubectl_get", Response: map[string]any{"output": "3 pods"}})
		run.Handle(text("b", "Three pods, all ready."))
		e.settle(t, 4)                                                // the step is done, and the text that came after it is appended
		run.Handle(agent.ToolCallEvent{CallID: "c2", Name: "search"}) // never answered: ended at the stop
		run.Handle(agent.ToolCallEvent{CallID: "c3", Name: "fetch"})
		run.Handle(agent.ToolResultEvent{CallID: "c3", Name: "fetch", Response: map[string]any{"error": "call rejected"}})
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"a-stream-with-a-hostile-tool-name", func(t *testing.T, e *env) {
		release := e.hold() // the first write is not throttled: it must not be made before the events are in
		t.Cleanup(release)
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
		run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "<!channel> [click](https://evil.test)\n\u202e*b* `c` & done", Args: map[string]any{"q": "<@U123> ![x](https://evil.test/p.png) <b>t</b>"}})
		run.Handle(agent.ToolResultEvent{CallID: "c1", Response: map[string]any{"output": "ok"}})
		run.Handle(text("a", "done"))
		run.Handle(agent.FinalEvent{})
		release()
		e.finish(t, run)
	}},
	{"a-stream-whose-text-diverged", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
		run.Handle(text("a", "The answer is 41"))
		e.settle(t, 1) // the stream starts
		run.Handle(text("a", "The answer is 42"))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"final-mode-posts-once", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputFinal, Recipient: "U1"})
		run.Handle(text("a", "Working")) // a final-mode turn writes nothing before its final: there is nothing to wait for
		run.Handle(text("a", "Working. Done."))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"final-mode-in-an-alert-thread-with-nothing-to-say", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputFinal})
		run.Handle(text("a", "\u200b  "))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"hostile-text-in-the-reply", func(t *testing.T, e *env) {
		release := e.hold() // the first write is not throttled: it must not be made before the events are in
		t.Cleanup(release)  // a failure before the release must not leave the renderer blocked in the clock
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
		run.Handle(text("a", "<!channel> ping <@U123> [click](https://evil.test) ![x](https://evil.test/p.png) *b* `c` <b>t</b> & \u202e done"))
		run.Handle(text("b", "second artifact: &lt;!here&gt;"))
		run.Handle(agent.FinalEvent{})
		release()
		e.finish(t, run)
	}},
	{"long-text-is-cut-and-marked", func(t *testing.T, e *env) {
		release := e.hold()
		t.Cleanup(release)
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
		run.Handle(text("a", strings.Repeat("The quick brown fox jumps over the lazy dog. ", 400)))
		run.Handle(agent.FinalEvent{})
		release()
		e.finish(t, run)
	}},
	{"notices-and-the-queued-reaction", func(t *testing.T, e *env) {
		sink := e.sink(t, e.ob)
		key := session.Key{Connection: "ns/conn", Channel: "C1", Thread: e.root}
		for _, kind := range []session.NoticeKind{session.NoticeRetrying, session.NoticeFailed, session.NoticeSessionGone, session.NoticeBindingGone, session.NoticeExpired, session.NoticeAnswerRejected, session.NoticeAnswerDropped, session.NoticeStopped, session.NoticeWedged, session.NoticeDuplicateRun} {
			sink.Notice(t.Context(), key, session.Notice{ID: "task:t1/" + string(kind), Kind: kind, Detail: "<!channel> boom *x* " + strings.Repeat("y", 300)})
		}
		sink.Notice(t.Context(), key, session.Notice{ID: "m/queued", Kind: session.NoticeQueued, MessageRef: e.root})
		sink.Notice(t.Context(), key, session.Notice{ID: "task:t2/failed", Kind: session.NoticeFailed})
		if err := e.ob.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}},
}

func TestTheWireIsWhatItWasBeforeThePort(t *testing.T) {
	for _, s := range scripts {
		t.Run(s.name, func(t *testing.T) {
			e := setup(t)
			s.run(t, e)
			got := e.record(t) // every script ends by waiting for its renderer to be done
			checkStepFieldsOnce(t, got)
			checkGolden(t, s.name, got)
		})
	}
}
