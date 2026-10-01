package slack_test

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// prompt posts an ask-user message the way WP-33 will render one: an input block for each
// question, and a Submit button. The inputs are a text field, a select and a multi-select.
func prompt(t *testing.T, fake *fakeslack.Server, opts ...slackgo.MsgOption) string {
	t.Helper()
	text := func(s string) *slackgo.TextBlockObject {
		return slackgo.NewTextBlockObject("plain_text", s, false, false)
	}
	opt := func(v string) *slackgo.OptionBlockObject { return slackgo.NewOptionBlockObject(v, text(v), nil) }
	blocks := slackgo.MsgOptionBlocks(
		slackgo.NewInputBlock("q1", text("Why?"), nil, slackgo.NewPlainTextInputBlockElement(nil, "a1")),
		slackgo.NewInputBlock("q2", text("Which?"), nil, slackgo.NewOptionsSelectBlockElement("static_select", nil, "a2", opt("x"), opt("y"))),
		slackgo.NewInputBlock("q3", text("Which all?"), nil, slackgo.NewOptionsMultiSelectBlockElement("multi_static_select", nil, "a3", opt("x"), opt("y"))),
		slackgo.NewActionBlock("act", slackgo.NewButtonBlockElement("submit", "int-1", text("Submit"))),
	)
	api := slackgo.New(botToken, slackgo.OptionAPIURL(fake.URL()+"/"))
	_, ts, err := api.PostMessage("C1", append(opts, blocks, slackgo.MsgOptionText("questions", false))...)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// sendInteraction delivers an interactive envelope and returns its envelope ID.
func sendInteraction(t *testing.T, fake *fakeslack.Server, payload map[string]any) string {
	t.Helper()
	id, err := fake.SendEnvelope("interactive", payload)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDeliversSubmittedInputs(t *testing.T) {
	fake, sk := setup(t)
	root, err := fake.Mention("C1", "U9", botMark+" ask me")
	if err != nil {
		t.Fatal(err)
	}
	ts := prompt(t, fake, slackgo.MsgOptionTS(root.TS)) // in the thread, as the renderer posts it
	press, err := fake.Click("U1", ts, "submit", "int-1")
	if err != nil {
		t.Fatal(err)
	}
	answer, err := fake.Submit("U2", ts, "submit", fakeslack.StateValues{
		"q1": {"a1": {"value": "because"}},
		"q2": {"a2": {"selected_option": map[string]any{"value": "y"}}},
		"q3": {"a3": {"selected_options": []any{map[string]any{"value": "x"}, map[string]any{"value": "y"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitAck(t, fake, press, answer)

	unanswered := map[string]map[string]slack.Input{
		"q1": {"a1": {Type: "plain_text_input"}}, "q2": {"a2": {Type: "static_select"}}, "q3": {"a3": {Type: "multi_static_select"}}}
	answered := map[string]map[string]slack.Input{
		"q1": {"a1": {Type: "plain_text_input", Values: []string{"because"}}},
		"q2": {"a2": {Type: "static_select", Values: []string{"y"}}},
		"q3": {"a3": {Type: "multi_static_select", Values: []string{"x", "y"}}}}
	want := []slack.Interaction{
		{User: "U1", Channel: "C1", MessageTS: ts, ThreadTS: root.TS, ActionID: "submit", BlockID: "act", Value: "int-1", Inputs: unanswered},
		{User: "U2", Channel: "C1", MessageTS: ts, ThreadTS: root.TS, ActionID: "submit", BlockID: "act", Value: "int-1", Inputs: answered},
	}
	if got := sk.interactions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("interactions =\n%+v\nwant\n%+v", got, want)
	}
}

// press returns a block_actions payload for a person's press of a button on a message;
// extra overrides its fields.
func press(extra map[string]any) map[string]any {
	p := map[string]any{
		"type": "block_actions", "user": map[string]any{"id": "U1"}, "channel": map[string]any{"id": "C1"},
		"container": map[string]any{"type": "message", "message_ts": "1.1", "channel_id": "C1"},
		"actions":   []any{map[string]any{"type": "button", "action_id": "go", "block_id": "b", "value": "v"}},
	}
	maps.Copy(p, extra)
	return p
}

// TestFiltersInteractions sends raw envelopes: each is acknowledged, and only a press by a
// person on a message reaches the sink.
func TestFiltersInteractions(t *testing.T) {
	fake, sk := setup(t)
	tests := []struct {
		name string
		p    map[string]any
		kept bool
	}{
		{"press", press(nil), true},
		{"by courier itself", press(map[string]any{"user": map[string]any{"id": "UFAKEBOT"}}), false},
		{"by no one", press(map[string]any{"user": map[string]any{}}), false},
		{"no action", press(map[string]any{"actions": []any{}}), false},
		{"not on a message", press(map[string]any{"container": map[string]any{"type": "view", "view_id": "V1"}}), false},
		{"no channel_id", press(map[string]any{"container": map[string]any{"type": "message", "message_ts": "1.1"}}), false},
		{"no message_ts", press(map[string]any{"container": map[string]any{"type": "message", "channel_id": "C1"}}), false},
		{"a modal submission", press(map[string]any{"type": "view_submission"}), false},
	}
	for _, tt := range tests {
		before := len(sk.interactions())
		waitAck(t, fake, sendInteraction(t, fake, tt.p))
		if got := len(sk.interactions()) - before; (got == 1) != tt.kept || got > 1 {
			t.Errorf("%s: sink took %d interactions; kept = %v", tt.name, got, tt.kept)
		}
	}
}

// TestInputsAreReadByType sends the state of one input element, as the types courier renders
// and as shapes that must not be read as answers: a field of another type's, or a type courier
// does not render. Each element gives only its own type's answer, or none.
// UNCONFIRMED(WP-04): the shape of each element's state, which is the fake's guess.
func TestInputsAreReadByType(t *testing.T) {
	fake, sk := setup(t)
	opts := func(vs ...string) []any {
		var out []any
		for _, v := range vs {
			out = append(out, map[string]any{"value": v})
		}
		return out
	}
	tests := []struct {
		name  string
		state map[string]any
		want  []string
	}{
		{"text", map[string]any{"type": "plain_text_input", "value": "t"}, []string{"t"}},
		{"select", map[string]any{"type": "static_select", "selected_option": map[string]any{"value": "p"}}, []string{"p"}},
		{"radio", map[string]any{"type": "radio_buttons", "selected_option": map[string]any{"value": "p"}}, []string{"p"}},
		{"multi-select", map[string]any{"type": "multi_static_select", "selected_options": opts("p", "q")}, []string{"p", "q"}},
		{"checkboxes", map[string]any{"type": "checkboxes", "selected_options": opts("p")}, []string{"p"}},
		{"unanswered select", map[string]any{"type": "static_select", "selected_option": nil}, nil},
		{"number input", map[string]any{"type": "number_input", "value": "42"}, nil},
		{"rich text input", map[string]any{"type": "rich_text_input", "rich_text_value": map[string]any{"type": "rich_text"}, "value": "x"}, nil},
		{"select with a stray value", map[string]any{"type": "static_select", "value": "stray", "selected_option": map[string]any{"value": "p"}}, []string{"p"}},
		{"select with options", map[string]any{"type": "static_select", "selected_options": opts("p", "q")}, nil},
		{"text with options", map[string]any{"type": "plain_text_input", "selected_options": opts("z")}, nil},
		{"multi-select with one option", map[string]any{"type": "multi_static_select", "selected_option": map[string]any{"value": "w"}}, nil},
	}
	for _, tt := range tests {
		state := map[string]any{"values": map[string]any{"q": map[string]any{"a": tt.state}}}
		waitAck(t, fake, sendInteraction(t, fake, press(map[string]any{"state": state})))
		got := sk.interactions()
		if last := got[len(got)-1].Inputs["q"]["a"]; !slices.Equal(last.Values, tt.want) || last.Type != tt.state["type"] {
			t.Errorf("%s: %+v; want %v values of type %v", tt.name, last, tt.want, tt.state["type"])
		}
	}
}

// TestInteractionAckFollowsTheSink: as for messages, a press is acknowledged once the sink
// has taken it, and not when the sink failed.
func TestInteractionAckFollowsTheSink(t *testing.T) {
	fake, sk := setup(t)
	ts := prompt(t, fake)
	entered, release := make(chan struct{}), make(chan struct{})
	open := sync.OnceFunc(func() { close(release) })
	t.Cleanup(open)
	sk.ifn = func(i slack.Interaction) error {
		if i.User != "U1" {
			return nil
		}
		close(entered)
		<-release
		return errors.New("store unavailable")
	}
	first, err := fake.Click("U1", ts, "submit", "int-1")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sink was not called")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	if err := fake.WaitAck(ctx, first); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitAck while the sink holds the press = %v; want it to time out", err)
	}
	open()
	second, _ := fake.Click("U2", ts, "submit", "int-1")
	waitAck(t, fake, second)
	if _, acked := fake.Ack(first); acked {
		t.Fatal("a press the sink failed on was acknowledged")
	}
}
