package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// pressedAt is the invoke of a press of the card message 1790000000200 in testChannel, in the thread of 1790000000100, by testUser,
// whose action's data is data.
func pressedAt(data map[string]any) Invoke {
	value, _ := json.Marshal(map[string]any{"action": map[string]any{"type": "Action.Execute", "title": "x", "data": data}, "trigger": "manual"})
	return Invoke{Name: actionInvoke, ID: "f:00000000-0000-4000-8000-0000000000c3", User: testUser, Value: value,
		At: chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: "1790000000200", Thread: "1790000000100"}}
}

func promptData(action string) map[string]any {
	return map[string]any{dataKey: map[string]any{"prompt": "ix-1", "action": action}}
}

// A press is the neutral Interaction its card names: a Submit reads the inputs as the controls they were asked in (a text's value
// whole, a choice of one as its index, a choice of several split at its commas), in the order of the questions, and leaves out an
// input that holds nothing; Approve all and Deny all read none; a button of a message names its value. The presser, the message and
// its thread come from the invoke, and the press is keyed by its activity.
func TestAPressIsTheInteractionItsCardNames(t *testing.T) {
	t.Parallel()
	c := newClientTest(t).c
	at := chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: "1790000000200", Thread: "1790000000100"}
	submit := promptData("submit")
	for k, v := range map[string]any{"c2": "0,2,", "t0": "a, b", "r1": "1", "t3": "", "r12": "4", "x0": "9", "t": "y", "r4": 7} {
		submit[k] = v
	}
	approve := promptData("approve_all")
	approve["r0"] = "1"
	for name, tc := range map[string]struct {
		data map[string]any
		want chat.Interaction
		kind pressKind
	}{
		"submit": {submit, chat.Interaction{PromptID: "ix-1", Action: chat.ActionSubmit, Answers: []chat.Answer{
			{Question: 0, Values: []string{"a, b"}}, {Question: 1, Values: []string{"1"}}, {Question: 2, Values: []string{"0", "2"}},
			{Question: 12, Values: []string{"4"}}}}, pressPrompt},
		"approve all": {approve, chat.Interaction{PromptID: "ix-1", Action: chat.ActionApproveAll}, pressPrompt},
		"deny all":    {promptData("deny_all"), chat.Interaction{PromptID: "ix-1", Action: chat.ActionDenyAll}, pressPrompt},
		"a button":    {map[string]any{dataKey: map[string]any{"button": "useful", "value": "fb1|x"}, "t0": "ignored"}, chat.Interaction{PromptID: "fb1|x"}, pressButton},
	} {
		got, kind, ok := c.press(pressedAt(tc.data))
		tc.want.User, tc.want.At = testUser, at
		tc.want.Delivery = got.Delivery
		if !ok || kind != tc.kind {
			t.Errorf("%s: ok %v, kind %v, want %v", name, ok, kind, tc.kind)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(tc.want)
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Errorf("%s:\n%s\nwant\n%s", name, gotJSON, wantJSON)
		}
		if len(got.Delivery) != 64 {
			t.Errorf("%s: the delivery key %q", name, got.Delivery)
		}
	}
}

// A press that names no button of Agent Kourier's is not one: no data of its own, data of another form, a prompt's button with an
// action it does not render, both a prompt and a button, neither, or an action that is not an Action.Execute; and so is a press by no
// one, or of no message.
func TestAPressThatNamesNoButtonIsNone(t *testing.T) {
	t.Parallel()
	c := newClientTest(t).c
	for name, inv := range map[string]Invoke{
		"no data of its own":   pressedAt(map[string]any{"nonce": "x"}),
		"data not an object":   pressedAt(map[string]any{dataKey: "ix-1"}),
		"an unknown action":    pressedAt(promptData("approve_some")),
		"no action":            pressedAt(map[string]any{dataKey: map[string]any{"prompt": "ix-1"}}),
		"a prompt and button":  pressedAt(map[string]any{dataKey: map[string]any{"prompt": "ix-1", "action": "submit", "value": "v"}}),
		"neither":              pressedAt(map[string]any{dataKey: map[string]any{}}),
		"a value not a string": pressedAt(map[string]any{dataKey: map[string]any{"value": 3}}),
		"by no one":            func() Invoke { i := pressedAt(promptData("submit")); i.User = ""; return i }(),
		"of no message":        func() Invoke { i := pressedAt(promptData("submit")); i.At.ID = ""; return i }(),
		"another action type": func() Invoke {
			i := pressedAt(nil)
			i.Value = json.RawMessage(`{"action":{"type":"Action.Submit","data":{"kourier":{"prompt":"ix-1","action":"submit"}}}}`)
			return i
		}(),
		"no JSON": func() Invoke { i := pressedAt(nil); i.Value = json.RawMessage(`{`); return i }(),
	} {
		if got, _, ok := c.press(inv); ok {
			t.Errorf("%s: taken as %+v", name, got)
		}
	}
}

// The key of a press is the same for the same activity, and another for another activity, presser or message.
func TestThePressKey(t *testing.T) {
	t.Parallel()
	c := newClientTest(t).c
	key := func(edit func(*Invoke)) string {
		inv := pressedAt(promptData("submit"))
		edit(&inv)
		i, _, _ := c.press(inv)
		return i.Delivery
	}
	first := key(func(*Invoke) {})
	if again := key(func(*Invoke) {}); again != first {
		t.Errorf("the same press has the keys %q and %q", first, again)
	}
	for name, edit := range map[string]func(*Invoke){
		"another activity": func(i *Invoke) { i.ID = "f:00000000-0000-4000-8000-0000000000c4" },
		"another presser":  func(i *Invoke) { i.User = "00000000-0000-4000-8000-000000000002" },
		"another message":  func(i *Invoke) { i.At.ID = "1790000000201" },
	} {
		if k := key(edit); k == first || k == "" {
			t.Errorf("%s: the key %q", name, k)
		}
	}
	if k := key(func(i *Invoke) { i.ID = "" }); k != "" {
		t.Errorf("a press with no activity ID has the key %q, want none", k)
	}
}

// pressSink is a chat.Sink that records the presses it is given, and refuses them with fail.
type pressSink struct {
	mu      sync.Mutex
	presses []chat.Interaction
	fail    error
}

func (s *pressSink) Message(context.Context, chat.Inbound) error { return nil }

func (s *pressSink) Interaction(_ context.Context, i chat.Interaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.presses = append(s.presses, i)
	return s.fail
}

func (s *pressSink) taken() []chat.Interaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]chat.Interaction(nil), s.presses...)
}

// postInvoke sends the invoke fixture, its value's action data set to data, to the Client's route, signed, and returns the answer.
func postInvoke(t *testing.T, ct *clientTest, id string, data map[string]any, edit func(map[string]any)) *httptest.ResponseRecorder {
	t.Helper()
	body := editFixture(t, "invoke_action", func(m map[string]any) {
		m["id"] = id
		m["value"].(map[string]any)["action"].(map[string]any)["data"] = data
		if edit != nil {
			edit(m)
		}
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat/teams/"+testConn, bytes.NewReader(body))
	req.Header.Set("Authorization", bearer(t, ct.key, claimsAt(time.Now())))
	rec := httptest.NewRecorder()
	h, _ := ct.c.HTTPHandler()
	h.ServeHTTP(rec, req)
	return rec
}

// told is the text of an invoke's answer that Teams shows the presser, and fails the test unless the answer is one.
func told(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var ans struct {
		StatusCode int    `json:"statusCode"`
		Type       string `json:"type"`
		Value      string `json:"value"`
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" || json.Unmarshal(rec.Body.Bytes(), &ans) != nil ||
		ans.StatusCode != http.StatusOK || ans.Type != "application/vnd.microsoft.activity.message" {
		t.Fatalf("answer %d %q %q, want a message for the presser", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	return ans.Value
}

// A press comes to the Client's route as an invoke, is handed to the sink of the run in the request, and is answered at once with a
// message for the presser: that it was received, that it could not be recorded (the sink's error), or that it names no button of
// Agent Kourier's, which the sink is not given. An invoke of another name is answered 200 with no body.
func TestAPressIsRecordedAndAnsweredInItsRequest(t *testing.T) {
	ct := newClientTest(t)
	sink := &pressSink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ct.c.recv.Serve(ctx, sink) }()
	t.Cleanup(func() { cancel(); _ = <-done }) // Serve says only that it was cancelled
	testutil.Eventually(t, "the receiver takes requests", func() bool { return ct.c.recv.live.Load() != nil })

	if got := told(t, postInvoke(t, ct, "f:1", promptData("deny_all"), nil)); got != textPromptReceived {
		t.Errorf("a prompt's press: told %q, want %q", got, textPromptReceived)
	}
	if got := told(t, postInvoke(t, ct, "f:2", map[string]any{dataKey: map[string]any{"button": "b", "value": "fb1|v"}}, nil)); got != textButtonReceived {
		t.Errorf("a button's press: told %q, want %q", got, textButtonReceived)
	}
	presses := sink.taken()
	if len(presses) != 2 || presses[0].Action != chat.ActionDenyAll || presses[0].PromptID != "ix-1" || presses[1].PromptID != "fb1|v" ||
		presses[0].User != testUser || presses[0].At.ID != "1790000000200" || presses[0].At.Channel != testChannel {
		t.Fatalf("the sink was given %+v", presses)
	}

	if got := told(t, postInvoke(t, ct, "f:3", map[string]any{"nonce": "x"}, nil)); got != textPressUnusable {
		t.Errorf("a press of no button: told %q, want %q", got, textPressUnusable)
	}
	if n := len(sink.taken()); n != 2 {
		t.Errorf("the sink was given a press of no button: %d presses", n)
	}

	sink.mu.Lock()
	sink.fail = errors.New("the store is down")
	sink.mu.Unlock()
	if got := told(t, postInvoke(t, ct, "f:4", promptData("submit"), nil)); got != textPressNotRecorded {
		t.Errorf("a press the sink refused: told %q, want %q", got, textPressNotRecorded)
	}

	rec := postInvoke(t, ct, "f:5", promptData("submit"), func(m map[string]any) { m["name"] = "task/fetch" })
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("another invoke: %d %q, want 200 and no body", rec.Code, rec.Body.String())
	}
	if n := len(sink.taken()); n != 3 {
		t.Errorf("the sink was given %d presses, want 3", n)
	}
}
