package slack_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// chunkList is the chunks parameter of a streaming request.
func chunkList(t *testing.T, form map[string][]string) []map[string]any {
	t.Helper()
	var out []map[string]any
	raw := form["chunks"]
	if len(raw) != 1 {
		t.Fatalf("a request has %d chunks parameters, want one: %v", len(raw), form)
	}
	if err := json.Unmarshal([]byte(raw[0]), &out); err != nil {
		t.Fatalf("chunks %s: %v", raw[0], err)
	}
	return out
}

// TestStreamTextGoesAsMarkdownTextChunksAndNeverAsMarkdownText: a stream that is to carry steps is in chunks mode
// from its start, since Slack documents that a stream keeps the mode it was started in (chat.stopStream: "If you
// started the stream with markdown_text, you must append with markdown_text"), so all its text is chunks.
func TestStreamTextGoesAsMarkdownTextChunksAndNeverAsMarkdownText(t *testing.T) {
	c, _, calls, root := updateClient(t)
	ctx := t.Context()
	ts, err := c.StartStream(ctx, "C1", root, "U1", "Hel", slack.ChunksMode)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AppendStream(ctx, "C1", ts, "lo", slack.ChunksMode); err != nil {
		t.Fatal(err)
	}
	if err := c.StopStream(ctx, "C1", ts, "!", nil, "ob-1", slack.ChunksMode); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/chat.startStream": "Hel", "/chat.appendStream": "lo", "/chat.stopStream": "!"} {
		forms := calls.sent(path)
		if len(forms) != 1 {
			t.Fatalf("%s: %d requests, want one", path, len(forms))
		}
		if forms[0].Has("markdown_text") {
			t.Errorf("%s sent markdown_text %q; want the text in a chunk", path, forms[0].Get("markdown_text"))
		}
		got := chunkList(t, forms[0])
		if len(got) != 1 || got[0]["type"] != "markdown_text" || got[0]["text"] != want {
			t.Errorf("%s sent chunks %v; want one markdown_text chunk of %q", path, got, want)
		}
	}
}

// A stream that starts with no text, or stops with none, sends no chunks for it, as it sent no markdown_text.
func TestAStreamWithNoTextSendsNoTextChunk(t *testing.T) {
	c, _, calls, root := updateClient(t)
	ts, err := c.StartStream(t.Context(), "C1", root, "U1", "", slack.ChunksMode)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StopStream(t.Context(), "C1", ts, "", nil, "ob-1", slack.ChunksMode); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/chat.startStream", "/chat.stopStream"} {
		if f := calls.sent(path)[0]; f.Has("chunks") || f.Has("markdown_text") {
			t.Errorf("%s sent %v; want no text of either kind", path, f)
		}
	}
}

func TestAppendStepSendsATaskUpdateChunkInTheSameModeAsTheText(t *testing.T) {
	c, fake, calls, root := updateClient(t)
	ctx := t.Context()
	ts, err := c.StartStream(ctx, "C1", root, "U1", "Hel", slack.ChunksMode)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AppendStep(ctx, "C1", ts, slack.TaskUpdate{ID: "s1", Title: "search", Status: "in_progress", Details: "q=up"}); err != nil {
		t.Fatal(err)
	}
	if err := c.AppendStep(ctx, "C1", ts, slack.TaskUpdate{ID: "s1", Title: "search", Status: "complete", Output: "found"}); err != nil {
		t.Fatal(err)
	}
	forms := calls.sent("/chat.appendStream")
	if len(forms) != 2 {
		t.Fatalf("%d appends, want 2", len(forms))
	}
	first := chunkList(t, forms[0])
	want := map[string]any{"type": "task_update", "id": "s1", "title": "search", "status": "in_progress", "details": "q=up"}
	if len(first) != 1 || !mapsEqual(first[0], want) {
		t.Errorf("first step = %v; want %v", first, want)
	}
	for _, f := range forms {
		if f.Has("markdown_text") || f.Get("unfurl_links") != "false" || f.Get("unfurl_media") != "false" || f.Has("link_names") || f.Get("parse") == "full" {
			t.Errorf("a step request is not inert, or is in the other mode: %v", f)
		}
	}
	th := fake.Thread("C1", root)
	if got, want := th[len(th)-1].Tasks(), []fakeslack.Task{{ID: "s1", Title: "search", Status: "complete", Details: "q=up", Output: "found"}}; !slices.Equal(got, want) {
		t.Errorf("Tasks = %+v, want %+v", got, want)
	}
}

func mapsEqual(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestAStepOfAMessageThatIsNotStreamingIsNotStreaming(t *testing.T) {
	w := newWorld(t)
	st, err := w.p.Stream(t.Context(), w.target, "U1", "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Stop(t.Context(), "", chat.Message{}, "ob-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.Step(t.Context(), chat.Step{ID: "s1", Title: "search"}); !errors.Is(err, chat.ErrNotStreaming) {
		t.Errorf("Step after the stop = %v, want ErrNotStreaming", err)
	}
}

func TestSlackShowsSteps(t *testing.T) {
	if c := newWorld(t).p.Capabilities(); !c.Steps || !c.Streams {
		t.Errorf("Capabilities = %+v, want steps, which need streams", c)
	}
}

// A step begins in progress and is finished by the state of the same ID; the states are Slack's.
func TestAStepIsBegunInProgressAndMovedToItsOutcome(t *testing.T) {
	for _, tc := range []struct {
		state      chat.StepState
		status     string
		wantOutput bool
	}{
		{chat.StepRunning, "in_progress", false},
		{chat.StepDone, "complete", false},
		{chat.StepFailed, "error", false},
		{chat.StepEnded, "complete", true},
	} {
		t.Run(tc.state.String(), func(t *testing.T) {
			w := newWorld(t)
			st, err := w.p.Stream(t.Context(), w.target, "U1", "x")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Step(t.Context(), chat.Step{ID: "s1", Title: "kubectl", Detail: `ns="prod"`, State: tc.state}); err != nil {
				t.Fatal(err)
			}
			m, _ := w.last(t)
			got := m.Tasks()
			if len(got) != 1 || got[0].ID != "s1" || got[0].Title != "kubectl" || got[0].Details != `ns="prod"` || got[0].Status != tc.status {
				t.Fatalf("Tasks = %+v; want s1, kubectl, %q, status %s", got, `ns="prod"`, tc.status)
			}
			if has := got[0].Output != ""; has != tc.wantOutput {
				t.Errorf("output = %q; want one only for a step that ended with no outcome", got[0].Output)
			}
			if tc.wantOutput && !strings.Contains(got[0].Output, "No result") {
				t.Errorf("output = %q; want it to say that no result was seen, so that the card does not read as a success", got[0].Output)
			}
		})
	}
}

// What the agent wrote in a tool's name and arguments reaches Slack as the characters it is made of: nothing in it is a
// mention, a link or a broadcast, and a title that is nothing once cleaned is shown as blank, since a step needs one.
func TestAStepTitleAndDetailAreCleanedEscapedAndCapped(t *testing.T) {
	long := strings.Repeat("x", 600)
	for _, tc := range []struct {
		name, title, detail string
		wantTitle           string
		wantDetail          string
	}{
		{"a broadcast", "<!channel>", "<!here>", "&lt;!channel&gt;", "&lt;!here&gt;"},
		{"a mention", "<@U0123456789>", "<@U0123456789>", "&lt;@U0123456789&gt;", "&lt;@U0123456789&gt;"},
		{"a markdown link", "[x](https://evil.test)", "![y](https://evil.test/a.png)", `\[x]\(https://evil.test)`, `![y]\(https://evil.test/a.png)`},
		{"a link reference definition", "  [x]: https://evil.test", "a [x]: https://evil.test", `\[x]: https://evil.test`, `a [x]: https://evil.test`},
		{"a backslash that would undo an escape", `[x]\(y)`, `a]\(b`, `\[x]\\(y)`, `a]\\(b`},
		{"a redacted argument is left as it is", "run", `password=[redacted], list=[1, 2]`, "run", `password=[redacted], list=[1, 2]`},
		{"a mrkdwn link", "<https://evil.test|click>", "", "&lt;https://evil.test|click&gt;", ""},
		{"an ampersand", "a&b", "", "a&amp;b", ""},
		{"a line break and a hidden character", "a\nb\u200b\u202ec", "d\r\ne", "a bc", "d e"},
		{"nothing once cleaned", "\u200b\u200d", "", "(blank)", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			st, err := w.p.Stream(t.Context(), w.target, "U1", "x")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Step(t.Context(), chat.Step{ID: "s1", Title: tc.title, Detail: tc.detail}); err != nil {
				t.Fatal(err)
			}
			m, _ := w.last(t)
			got := m.Tasks()
			if len(got) != 1 || got[0].Title != tc.wantTitle || got[0].Details != tc.wantDetail {
				t.Errorf("Tasks = %+v; want title %q and details %q", got, tc.wantTitle, tc.wantDetail)
			}
		})
	}
	t.Run("capped", func(t *testing.T) {
		w := newWorld(t)
		st, err := w.p.Stream(t.Context(), w.target, "U1", "x")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Step(t.Context(), chat.Step{ID: "s1", Title: long, Detail: long}); err != nil {
			t.Fatal(err)
		}
		m, _ := w.last(t)
		got := m.Tasks()[0]
		limit := w.p.Capabilities().Limit(chat.WhereStepTitle)
		if limit != 256 || w.p.Capabilities().Limit(chat.WhereStepDetail) != 256 {
			t.Fatalf("limits %d and %d; want 256, the documented cap of a task_update's text", limit, w.p.Capabilities().Limit(chat.WhereStepDetail))
		}
		for name, s := range map[string]string{"title": got.Title, "details": got.Details} {
			if chat.Units(s) > 256 || !strings.HasSuffix(s, chat.Truncated) {
				t.Errorf("%s is %d units, ends %q; want at most 256, cut and marked", name, chat.Units(s), s[max(0, len(s)-6):])
			}
		}
	})
}

func TestAStepNeedsAnID(t *testing.T) {
	w := newWorld(t)
	st, err := w.p.Stream(t.Context(), w.target, "U1", "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Step(t.Context(), chat.Step{Title: "search"}); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("Step with no ID = %v, want ErrInvalid", err)
	}
}

// A step is a chat.appendStream call, so a rate limit or a failure of it is classified as any call of a stream is.
func TestAStepThatSlackRefusesIsAnError(t *testing.T) {
	c, _, calls, root := updateClient(t)
	p := slack.NewPlatform(c, "agent-kourier-system/slack-main")
	st, err := p.Stream(t.Context(), chat.Target{Channel: "C1", Thread: root}, "U1", "x")
	if err != nil {
		t.Fatal(err)
	}
	calls.status, calls.body = http.StatusOK, `{"ok":false,"error":"invalid_chunks"}`
	if err := st.Step(t.Context(), chat.Step{ID: "s1", Title: "search"}); err == nil || !strings.Contains(err.Error(), "invalid_chunks") {
		t.Fatalf("Step = %v; want Slack's error", err)
	}
}

// taskFields counts, for each task ID in the task_update chunks of forms, the chunks that carry details and the
// chunks that carry output.
func taskFields(t *testing.T, forms []url.Values) (details, output map[string]int) {
	t.Helper()
	details, output = map[string]int{}, map[string]int{}
	for _, f := range forms {
		for _, c := range chunkList(t, f) {
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
	return details, output
}

// Slack appends a task_update's details and output to the card's, and keeps them when a chunk leaves them out
// (observed 2026-10-18), so a step's details and its output go in one chunk each, at most: a step moved to its
// outcome sends its title and status alone, and the card still shows its details once.
func TestAStepSendsItsDetailsAndItsOutputOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		states  []chat.StepState
		output  int
		status  string
		wantOut bool
	}{
		{"begun and done", []chat.StepState{chat.StepRunning, chat.StepDone}, 0, "complete", false},
		{"begun and failed", []chat.StepState{chat.StepRunning, chat.StepFailed}, 0, "error", false},
		{"begun and ended with no result", []chat.StepState{chat.StepRunning, chat.StepEnded}, 1, "complete", true},
		{"ended when first sent", []chat.StepState{chat.StepEnded}, 1, "complete", true},
		{"sent in one state twice", []chat.StepState{chat.StepRunning, chat.StepRunning, chat.StepEnded, chat.StepEnded}, 1, "complete", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, fake, calls, root := updateClient(t)
			p := slack.NewPlatform(c, "agent-kourier-system/slack-main")
			st, err := p.Stream(t.Context(), chat.Target{Channel: "C1", Thread: root}, "U1", "x")
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tc.states {
				if err := st.Step(t.Context(), chat.Step{ID: "s1", Title: "kubectl", Detail: `ns="prod"`, State: s}); err != nil {
					t.Fatal(err)
				}
			}
			forms := calls.sent("/chat.appendStream")
			if len(forms) != len(tc.states) {
				t.Fatalf("%d appends; want one for each step sent", len(forms))
			}
			for i, f := range forms {
				if c := chunkList(t, f)[0]; c["title"] != "kubectl" || c["status"] == "" {
					t.Errorf("chunk %d = %v; want the title and the status in every chunk", i, c)
				}
			}
			details, output := taskFields(t, forms)
			if details["s1"] != 1 || output["s1"] != tc.output {
				t.Errorf("details in %d chunks and output in %d; want details in one and output in %d", details["s1"], output["s1"], tc.output)
			}
			th := fake.Thread("C1", root)
			got := th[len(th)-1].Tasks()
			if len(got) != 1 || got[0].Details != `ns="prod"` || got[0].Status != tc.status || (got[0].Output != "") != tc.wantOut {
				t.Errorf("Tasks = %+v; want the details once, status %s, and output only for a step with no result", got, tc.status)
			}
		})
	}
}

// A step that Slack refused was not applied, so its details go with the next chunk for it; one whose call was lost may
// have been, and its details do not go again, for a card that shows them twice is what sending them once prevents.
func TestAStepsDetailsGoAgainOnlyAfterSlackRefusedTheChunk(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  func(*counter)
		again bool
	}{
		{"refused", func(c *counter) { c.status, c.body = http.StatusOK, `{"ok":false,"error":"internal_error"}` }, true},
		{"lost", func(c *counter) { c.err = errors.New("connection reset") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, calls, root := updateClient(t)
			p := slack.NewPlatform(c, "agent-kourier-system/slack-main")
			st, err := p.Stream(t.Context(), chat.Target{Channel: "C1", Thread: root}, "U1", "x")
			if err != nil {
				t.Fatal(err)
			}
			calls.mu.Lock()
			tc.fail(calls)
			calls.mu.Unlock()
			if err := st.Step(t.Context(), chat.Step{ID: "s1", Title: "kubectl", Detail: `ns="prod"`}); err == nil {
				t.Fatal("Step = nil; want the failure")
			}
			calls.mu.Lock()
			calls.status, calls.body, calls.err = 0, "", nil
			calls.mu.Unlock()
			if err := st.Step(t.Context(), chat.Step{ID: "s1", Title: "kubectl", Detail: `ns="prod"`, State: chat.StepDone}); err != nil {
				t.Fatal(err)
			}
			forms := calls.sent("/chat.appendStream")
			last := chunkList(t, forms[len(forms)-1])[0]
			if _, has := last["details"]; has != tc.again {
				t.Errorf("the chunk after the failure = %v; want details in it: %v", last, tc.again)
			}
		})
	}
}

// The steps of a message are task_card blocks, before the parts that follow them, each in the status a stream's step of
// that state has, and a step that ended with no outcome says so in its output, as the stream's card does. An update
// shows them as a post does: an update is how a message that replaces a stream keeps the stream's cards.
func TestTheStepsOfAMessageAreTaskCardsBeforeItsText(t *testing.T) {
	w := newWorld(t)
	msg := chat.Message{Fallback: "the answer", Parts: []chat.Part{
		chat.Steps{Steps: []chat.Step{
			{ID: "s1", Title: "kubectl", Detail: `ns="prod"`}, {ID: "s2", Title: "search", State: chat.StepDone},
			{ID: "s3", Title: "fetch", State: chat.StepFailed}, {ID: "s4", Title: "ping", State: chat.StepEnded},
		}},
		chat.Body{Markdown: "the answer"},
	}}
	ref, err := w.p.Post(t.Context(), w.target, chat.Say(chat.Message{Fallback: "first"}), "tag-steps")
	if err != nil {
		t.Fatal(err)
	}
	for name, send := range map[string]func() error{
		"post":   func() error { _, err := w.p.Post(t.Context(), w.target, chat.Say(msg), "tag-steps-2"); return err },
		"update": func() error { return w.p.Update(t.Context(), ref, chat.Say(msg)) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := send(); err != nil {
				t.Fatal(err)
			}
			m, blocks := w.last(t)
			if name == "update" {
				m, blocks = w.fake.Thread("C1", w.target.Thread)[1], nil
				if err := json.Unmarshal(m.Blocks, &blocks); err != nil {
					t.Fatal(err)
				}
			}
			if got := strings.Join(types(blocks), ","); got != "task_card,task_card,task_card,task_card,markdown" {
				t.Fatalf("blocks %s; want the four cards and then the text", got)
			}
			want := []fakeslack.Task{
				{ID: "s1", Title: "kubectl", Status: "in_progress", Details: `ns="prod"`}, {ID: "s2", Title: "search", Status: "complete"},
				{ID: "s3", Title: "fetch", Status: "error"}, {ID: "s4", Title: "ping", Status: "complete", Output: "No result was reported."},
			}
			if got := m.Tasks(); !slices.Equal(got, want) || m.Text != "the answer" {
				t.Errorf("Tasks %+v, text %q; want %+v and the fallback", got, m.Text, want)
			}
		})
	}
}

// A card's title and the text of its details are shown by Slack as the characters they hold (probed 2026-10-21), so each is
// cleaned onto one line and cut at the cap of a step's text, and not escaped: an escape would be shown.
func TestTheCardsOfAMessageAreCleanedAndCappedAndNotEscaped(t *testing.T) {
	long := strings.Repeat("x", 600)
	for _, tc := range []struct {
		name, title, detail   string
		wantTitle, wantDetail string
	}{
		{"a broadcast", "<!channel>", "<!here>", "<!channel>", "<!here>"},
		{"a markdown link", "[x](https://evil.test)", "[y](https://evil.test)", "[x](https://evil.test)", "[y](https://evil.test)"},
		{"an ampersand and a mention", "a & <@U0123456789>", "", "a & <@U0123456789>", ""},
		{"a line break and a hidden character", "a\nb\u200b\u202ec", "d\r\ne", "a bc", "d e"},
		{"nothing once cleaned", "\u200b\u200d", "", "(blank)", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.post(t, chat.Say(chat.Message{Fallback: "f", Parts: []chat.Part{chat.Steps{Steps: []chat.Step{{ID: "s1", Title: tc.title, Detail: tc.detail}}}}}))
			m, _ := w.last(t)
			if got := m.Tasks(); len(got) != 1 || got[0].Title != tc.wantTitle || got[0].Details != tc.wantDetail {
				t.Errorf("Tasks = %+v; want title %q and details %q", got, tc.wantTitle, tc.wantDetail)
			}
		})
	}
	t.Run("capped", func(t *testing.T) {
		w := newWorld(t)
		w.post(t, chat.Say(chat.Message{Fallback: "f", Parts: []chat.Part{chat.Steps{Steps: []chat.Step{{ID: "s1", Title: long, Detail: long}}}}}))
		m, _ := w.last(t)
		got := m.Tasks()[0]
		for name, s := range map[string]string{"title": got.Title, "details": got.Details} {
			if chat.Units(s) > 256 || !strings.HasSuffix(s, chat.Truncated) {
				t.Errorf("%s is %d units; want at most 256, cut and marked", name, chat.Units(s))
			}
		}
	})
}

// One title, two places: in a task_card block it is the characters it holds, which is how Slack shows a card's title, and in
// a stream's task_update chunk it is escaped still, since how Slack reads a chunk's text is not known.
func TestACardTitleIsLiteralAndAStreamStepTitleIsEscaped(t *testing.T) {
	const title = "<!here> & [x](u)"
	w := newWorld(t)
	blocks := w.post(t, chat.Say(chat.Message{Fallback: "f", Parts: []chat.Part{chat.Steps{Steps: []chat.Step{{ID: "s1", Title: title}}}}}))
	if len(blocks) != 1 || blocks[0]["type"] != "task_card" || blocks[0]["title"] != title {
		t.Errorf("blocks %v; want one task_card titled %q, unescaped", blocks, title)
	}
	st, err := w.p.Stream(t.Context(), w.target, "U1", "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Step(t.Context(), chat.Step{ID: "s1", Title: title}); err != nil {
		t.Fatal(err)
	}
	m, _ := w.last(t)
	if got := m.Tasks(); len(got) != 1 || got[0].Title != `&lt;!here&gt; &amp; [x]\(u)` {
		t.Errorf("stream tasks %+v; want the chunk's title escaped as before", got)
	}
}

// A message holds 50 blocks, and its steps have what room the rest leaves: the cards that do not fit are counted on a line
// of their own after the last one shown, and the text and the buttons are all there.
func TestTheStepsOfAMessageFitInWhatTheRestLeavesOfFiftyBlocks(t *testing.T) {
	w := newWorld(t)
	var steps []chat.Step
	for i := range 60 {
		steps = append(steps, chat.Step{ID: "s" + strconv.Itoa(i+1), Title: "tool", State: chat.StepDone})
	}
	blocks := w.post(t, chat.Say(chat.Message{Fallback: "f", Parts: []chat.Part{
		chat.Steps{Steps: steps}, chat.Body{Markdown: "the answer"},
		chat.Buttons{Buttons: []chat.Button{{ID: "up", Label: "Useful", Value: "v"}}},
	}}))
	got := types(blocks)
	if len(got) != 50 || got[46] != "task_card" || got[47] != "context" || got[48] != "markdown" || got[49] != "actions" {
		t.Fatalf("%d blocks %v; want 47 cards, the line that counts the rest, the text and the buttons", len(got), got)
	}
	elements, _ := blocks[47]["elements"].([]any)
	if line, _ := elements[0].(map[string]any)["text"].(string); line != "13 more steps are not shown." {
		t.Errorf("the line says %q; want the count of the cards left out", line)
	}
	if m, _ := w.last(t); m.Tasks()[46].ID != "s47" {
		t.Errorf("the last card shown is %+v; want the first 47 in order", m.Tasks()[46])
	}
}
