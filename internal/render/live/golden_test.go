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
	"time"

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
	Edited    bool   `json:"edited,omitempty"`
	OutboxID  string `json:"outbox_id,omitempty"`
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

// settle turns the clock and waits until the renderer has stopped writing, so that a script's steps come out in the
// same order every time.
func (e *env) settle(t *testing.T) {
	t.Helper()
	stable := 0
	last := -1
	for deadline := time.Now().Add(5 * time.Second); stable < 4; time.Sleep(15 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the renderer never settled")
		}
		if e.clk.waiting() > 0 {
			e.clk.advance(throttle)
			stable = 0
			continue
		}
		if n := len(e.rec.orderOf()); n != last {
			last, stable = n, 0
		} else {
			stable++
		}
	}
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

// scripts are the turns that are recorded. Each is a function of the environment that drives a turn to its end.
var scripts = []struct {
	name string
	run  func(t *testing.T, e *env)
}{
	{"plain-answer-in-an-alert-thread", func(t *testing.T, e *env) { // live mode, no recipient: post, then updates
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
		run.Handle(text("a", "Looking into it"))
		e.settle(t)
		run.Handle(text("a", "Looking into it. The pod restarted twice."))
		e.settle(t)
		run.Handle(text("a", "Looking into it. The pod restarted twice.\n\nCause: OOM."))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"tools-with-secrets-in-their-arguments", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
		run.Handle(text("a", "Checking"))
		e.settle(t)
		run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "kubectl_get", Args: map[string]any{"namespace": "prod", "resource": "pods", "password": "hunter2", "url": "https://user:s3cr3t@host.test/x"}})
		e.settle(t)
		run.Handle(agent.ToolResultEvent{CallID: "c1", Name: "kubectl_get"})
		run.Handle(text("a", "Checking: three pods"))
		e.settle(t)
		run.Handle(agent.ToolCallEvent{CallID: "c2", Name: "search"})
		run.Handle(text("a", "Checking: three pods, all ready"))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"a-tool-line-alone-and-no-output", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
		run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "slow_query", Args: map[string]any{"q": "select 1"}})
		e.settle(t)
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"a-stream-in-a-chat-thread", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
		run.Handle(text("a", "Hel"))
		e.settle(t)
		run.Handle(text("a", "Hello wor"))
		run.Handle(agent.ToolCallEvent{CallID: "c1", Name: "search"})
		e.settle(t)
		run.Handle(text("a", "Hello world, and more"))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"a-stream-whose-text-diverged", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Recipient: "U1"})
		run.Handle(text("a", "The answer is 41"))
		e.settle(t)
		run.Handle(text("a", "The answer is 42"))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"final-mode-posts-once", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputFinal, Recipient: "U1"})
		run.Handle(text("a", "Working"))
		e.settle(t)
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
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
		run.Handle(text("a", "<!channel> ping <@U123> [click](https://evil.test) ![x](https://evil.test/p.png) *b* `c` <b>t</b> & \u202e done"))
		run.Handle(text("b", "second artifact: &lt;!here&gt;"))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}},
	{"long-text-is-cut-and-marked", func(t *testing.T, e *env) {
		run, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive})
		run.Handle(text("a", strings.Repeat("The quick brown fox jumps over the lazy dog. ", 400)))
		run.Handle(agent.FinalEvent{})
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
			e.settle(t)
			got := e.record(t)
			checkGolden(t, s.name, got)
		})
	}
}
