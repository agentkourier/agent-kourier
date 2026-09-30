package stubagent_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

type obj = map[string]any

// newAgent starts a stub with one agent, kagent/test, and returns a client for it.
func newAgent(t *testing.T, opts ...stubagent.Option) (*stubagent.Server, *stubagent.Agent, *client) {
	t.Helper()
	s := stubagent.New(opts...)
	t.Cleanup(s.Close)
	return s, s.Agent("kagent", "test"), &client{t: t, url: s.AgentURL("kagent", "test"), hdr: http.Header{}}
}

// client is a raw JSON-RPC client, so tests see exactly what is on the wire.
type client struct {
	t   *testing.T
	url string
	hdr http.Header
}

// stream reads the envelopes of one response as they arrive: one per SSE event, or the one
// JSON body.
type stream struct {
	t      *testing.T
	resp   *http.Response
	sc     *bufio.Scanner
	status int
	sse    bool
	read   bool
}

func (c *client) open(method string, params any) *stream {
	c.t.Helper()
	body := obj{"jsonrpc": "2.0", "id": "1", "method": method, "params": params}
	resp, err := http.DefaultClient.Do(request(c.t, http.MethodPost, c.url, body, c.hdr))
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	c.t.Cleanup(func() { _ = resp.Body.Close() })
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20) // a task snapshot can outgrow the default 64 KiB line
	return &stream{t: c.t, resp: resp, sc: sc, status: resp.StatusCode,
		sse: strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")}
}

// next returns the next envelope, or false at the end of the response.
func (s *stream) next() (obj, bool) {
	s.t.Helper()
	var raw []byte
	if s.sse {
		for s.sc.Scan() {
			if data, ok := strings.CutPrefix(s.sc.Text(), "data: "); ok {
				raw = []byte(data)
				break
			}
		}
		if err := s.sc.Err(); err != nil {
			s.t.Fatalf("read the stream: %v", err)
		}
	} else if !s.read {
		s.read = true
		var err error
		if raw, err = io.ReadAll(s.resp.Body); err != nil {
			s.t.Fatalf("read the body: %v", err)
		}
	}
	if raw == nil {
		return nil, false
	}
	var ev obj
	if err := json.Unmarshal(raw, &ev); err != nil {
		s.t.Fatalf("bad JSON %q: %v", raw, err)
	}
	return ev, true
}

// all reads the response to its end.
func (s *stream) all() []obj {
	var evs []obj
	for ev, ok := s.next(); ok; ev, ok = s.next() {
		evs = append(evs, ev)
	}
	return evs
}

// call sends a request that answers with one envelope, as JSON or as a single SSE event.
func (c *client) call(method string, params any) (int, obj) {
	c.t.Helper()
	st := c.open(method, params)
	evs := st.all()
	if len(evs) != 1 {
		c.t.Fatalf("%s answered with %d envelopes, want 1", method, len(evs))
	}
	return st.status, evs[0]
}

// send is a SendStreamingMessage call that must answer with a stream.
func (c *client) send(params obj) []obj {
	c.t.Helper()
	st := c.open("SendStreamingMessage", params)
	if !st.sse {
		c.t.Fatalf("SendStreamingMessage did not answer with SSE")
	}
	return st.all()
}

// request builds a request. A nil body sends none, a string is sent as it is, and anything
// else as JSON.
func request(t *testing.T, method, url string, body any, hdr http.Header) *http.Request {
	t.Helper()
	var r io.Reader = http.NoBody
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header = hdr.Clone()
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// do sends a request and returns the status and body.
func do(t *testing.T, method, url string, body any, hdr http.Header) (int, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(request(t, method, url, body, hdr))
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, raw
}

// msg builds message params; ctxID may be empty.
func msg(id, text, ctxID string) obj {
	m := obj{"messageId": id, "role": "ROLE_USER", "parts": []obj{{"text": text}}}
	if ctxID != "" {
		m["contextId"] = ctxID
	}
	return obj{"message": m}
}

// task returns the task under result.task of a SendMessage answer.
func task(t *testing.T, body obj) obj {
	t.Helper()
	tk, _ := dig(body, "result", "task").(obj)
	if tk == nil {
		t.Fatalf("no task in %v", body)
	}
	return tk
}

// dig walks nested maps by key; it returns nil when a key is missing.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(obj)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// body returns an event's kind ("task", "statusUpdate", "artifactUpdate", "error") and
// payload; GetTask and CancelTask results are the task itself.
func body(ev obj) (string, obj) {
	if _, ok := ev["error"]; ok {
		return "error", nil
	}
	res, _ := ev["result"].(obj)
	if _, ok := res["status"]; ok {
		return "task", res
	}
	for _, k := range []string{"task", "statusUpdate", "artifactUpdate"} {
		if e, ok := res[k].(obj); ok {
			return k, e
		}
	}
	return "", nil
}

// ids returns the task ID, context ID and state carried by an event.
func ids(ev obj) (task, ctxID, state string) {
	_, e := body(ev)
	task, _ = e["id"].(string)
	if task == "" {
		task, _ = e["taskId"].(string)
	}
	ctxID, _ = e["contextId"].(string)
	state, _ = dig(e, "status", "state").(string)
	return task, ctxID, state
}

func last(evs []obj) obj { return evs[len(evs)-1] }

// describe names one event as the WP-02 spike does: kind and state, or artifact flags.
func describe(ev obj) string {
	kind, e := body(ev)
	switch kind {
	case "error":
		return fmt.Sprintf("error:%v", dig(ev, "error", "code"))
	case "artifactUpdate":
		d := "artifact"
		if e["append"] == true {
			d += "+append"
		}
		if e["lastChunk"] == true {
			d += "+last"
		}
		return d
	}
	_, _, state := ids(ev)
	return kind + ":" + strings.TrimPrefix(state, "TASK_STATE_")
}

// shape run-length encodes a stream, for example "task:SUBMITTED x2, artifact+append x7".
func shape(evs []obj) string {
	var parts []string
	for i := 0; i < len(evs); {
		j := i
		for j < len(evs) && describe(evs[j]) == describe(evs[i]) {
			j++
		}
		p := describe(evs[i])
		if j-i > 1 {
			p += fmt.Sprintf(" x%d", j-i)
		}
		parts = append(parts, p)
		i = j
	}
	return strings.Join(parts, ", ")
}

// skeleton is a value's structure with the values erased: keys, and a type per leaf. Two
// events with the same skeleton have the same fields.
func skeleton(v any) string {
	switch v := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&sb, "%s:%s,", k, skeleton(v[k]))
		}
		return "{" + sb.String() + "}"
	case []any:
		items := make([]string, len(v)) // every element counts, so a list of the wrong length differs
		for i, item := range v {
			items[i] = skeleton(item)
		}
		return "[" + strings.Join(items, ";") + "]"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// fixture reads the envelopes of a recorded kagent response: the events of an .sse file, or
// the one body of a .json file.
func fixture(t *testing.T, name string) []obj {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "fixtures", "kagent", name))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var evs []obj
	if strings.HasSuffix(name, ".json") {
		var ev obj
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("fixture %s: %v", name, err)
		}
		return []obj{ev}
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var ev obj
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("fixture %s: %v", name, err)
			}
			evs = append(evs, ev)
		}
	}
	return evs
}

// text joins the text parts of a parts list.
func text(parts any) string {
	var sb strings.Builder
	ps, _ := parts.([]any)
	for _, p := range ps {
		if s, ok := dig(p, "text").(string); ok {
			sb.WriteString(s)
		}
	}
	return sb.String()
}

// taskText joins the text of every artifact of a task event (or a GetTask result).
func taskText(ev obj) string {
	_, e := body(ev)
	var sb strings.Builder
	arts, _ := e["artifacts"].([]any)
	for _, a := range arts {
		sb.WriteString(text(dig(a, "parts")))
	}
	return sb.String()
}

// assemble applies the client rule to a stream: an opening task snapshot seeds each artifact,
// then a chunk with append: true is added to its artifact and any other chunk replaces it.
// It skips lastChunk events, so a caller can compare the result with the lastChunk text.
func assemble(evs []obj) string {
	texts := map[string]string{}
	var order []string
	set := func(id, s string, add bool) {
		if _, ok := texts[id]; !ok {
			order = append(order, id)
		}
		if add {
			texts[id] += s
		} else {
			texts[id] = s
		}
	}
	for i, ev := range evs {
		kind, e := body(ev)
		switch {
		case kind == "task" && i == 0:
			arts, _ := e["artifacts"].([]any)
			for _, a := range arts {
				id, _ := dig(a, "artifactId").(string)
				set(id, text(dig(a, "parts")), false)
			}
		case kind == "artifactUpdate" && e["lastChunk"] != true:
			id, _ := dig(e, "artifact", "artifactId").(string)
			set(id, text(dig(e, "artifact", "parts")), e["append"] == true)
		}
	}
	var sb strings.Builder
	for _, id := range order {
		sb.WriteString(texts[id])
	}
	return sb.String()
}

// expectLike checks that an envelope has the same fields as the recorded one.
func expectLike(t *testing.T, label string, got, recorded obj) {
	t.Helper()
	if g, w := skeleton(got), skeleton(recorded); g != w {
		t.Errorf("%s has fields\n  %s\nthe fixture has\n  %s", label, g, w)
	}
}
