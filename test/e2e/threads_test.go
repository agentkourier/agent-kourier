package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	chatslack "github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

func view(user, botID, text string) fakeslack.MessageView {
	return fakeslack.MessageView{Message: fakeslack.Message{User: user, BotID: botID, Text: text}}
}

func TestBotMessagesKeepsTheBotsInOrder(t *testing.T) {
	thread := []fakeslack.MessageView{view("U1", "", "root"), view("UFAKEBOT", "B1", "one"), view("U2", "", "reply"), view("UFAKEBOT", "B1", "two")}
	got := BotMessages(thread)
	if len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" {
		t.Errorf("BotMessages = %+v; want the bot's two messages, in order", got)
	}
	if got := BotMessages(nil); len(got) != 0 {
		t.Errorf("BotMessages(nil) = %+v; want none", got)
	}
}

func TestHasActionFindsElementsAtAnyDepth(t *testing.T) {
	blocks := json.RawMessage(`[{"type":"header"},{"type":"input","block_id":"q0","element":{"type":"checkboxes","action_id":"a0"}},
		{"type":"actions","block_id":"submit","elements":[{"type":"button","action_id":"submit","value":"ix-1"}]}]`)
	for _, tc := range []struct {
		id         string
		has, isBtn bool
	}{{"a0", true, false}, {"submit", true, true}, {"nope", false, false}} {
		if got := HasAction(blocks, tc.id); got != tc.has {
			t.Errorf("HasAction(%q) = %v; want %v", tc.id, got, tc.has)
		}
		if got := HasButton(blocks, tc.id); got != tc.isBtn {
			t.Errorf("HasButton(%q) = %v; want %v", tc.id, got, tc.isBtn)
		}
	}
	if HasAction(nil, "a0") || HasAction(json.RawMessage(`not json`), "a0") || HasButton(nil, "submit") {
		t.Error("blocks that are absent or unreadable have no element")
	}
}

func TestHasBlockTextDecodesSlackEscapedMentions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks json.RawMessage
		want   bool
	}{
		{name: "plain mention", blocks: json.RawMessage(`[{"type":"section","text":{"type":"mrkdwn","text":"Approved <@US19>"}}]`), want: true},
		{name: "HTML escaped mention", blocks: json.RawMessage(`[{"type":"section","text":{"type":"mrkdwn","text":"Approved \u003c@US19\u003e"}}]`), want: true},
		{name: "absent actor", blocks: json.RawMessage(`[{"type":"section","text":{"type":"mrkdwn","text":"Approved"}}]`), want: false},
		{name: "wrong actor", blocks: json.RawMessage(`[{"type":"section","text":{"type":"mrkdwn","text":"Approved <@US20>"}}]`), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasBlockText(tc.blocks, "<@US19>"); got != tc.want {
				t.Errorf("HasBlockText = %v; want %v", got, tc.want)
			}
		})
	}
	if HasBlockText(json.RawMessage(`not json`), "<@US19>") || HasBlockText(nil, "<@US19>") {
		t.Error("HasBlockText found text in absent or unreadable blocks")
	}
}

func TestApprovalPromptFindsOnlyApprovalButtons(t *testing.T) {
	approval := fakeslack.MessageView{Message: fakeslack.Message{BotID: "B1", TS: "1.000001", Blocks: json.RawMessage(`[
		{"type":"actions","elements":[
			{"type":"button","action_id":"approve_all","value":"ix-1"},
			{"type":"button","action_id":"deny_all","value":"ix-1"}]}]`)}}
	question := fakeslack.MessageView{Message: fakeslack.Message{BotID: "B1", TS: "1.000000", Blocks: json.RawMessage(`[
		{"type":"actions","elements":[{"type":"button","action_id":"submit","value":"ix-2"}]}]`)}}
	if got, ok := ApprovalPrompt([]fakeslack.MessageView{question, approval}); !ok || got.TS != approval.TS {
		t.Errorf("ApprovalPrompt = %+v, %v; want the approval message", got, ok)
	}
	if got, ok := ApprovalPrompt([]fakeslack.MessageView{question}); ok || got.TS != "" {
		t.Errorf("ApprovalPrompt on a question = %+v, %v; want no approval", got, ok)
	}
}

func TestOutboxIDReadsTheMetadataTag(t *testing.T) {
	tagged := view("UFAKEBOT", "B1", "x")
	tagged.Metadata = &fakeslack.Metadata{EventType: chatslack.MetadataEventType, EventPayload: map[string]any{"outbox_id": "ob-1"}}
	other := view("UFAKEBOT", "B1", "y")
	other.Metadata = &fakeslack.Metadata{EventType: "other", EventPayload: map[string]any{"outbox_id": 7}}
	if got := OutboxID(tagged); got != "ob-1" {
		t.Errorf("OutboxID = %q; want ob-1", got)
	}
	if got := OutboxID(other); got != "" {
		t.Errorf("OutboxID of a tag that is not a string = %q; want none", got)
	}
	foreign := view("UFAKEBOT", "B1", "w")
	foreign.Metadata = &fakeslack.Metadata{EventType: "someone_elses", EventPayload: map[string]any{"outbox_id": "ob-2"}}
	if got := OutboxID(foreign); got != "" {
		t.Errorf("OutboxID of another app's tag = %q; want none: only Agent Kourier's own tag names an outbox row", got)
	}
	if got := OutboxID(view("U1", "", "z")); got != "" {
		t.Errorf("OutboxID of a message with no metadata = %q; want none", got)
	}
}

func TestAnswerStatesAreWhatTheFakeTakes(t *testing.T) {
	for _, tc := range []struct {
		typ    string
		values []string
		want   string
	}{
		{"checkboxes", []string{"0", "1"}, `{"selected_options":[{"value":"0"},{"value":"1"}],"type":"checkboxes"}`},
		{"multi_static_select", []string{"2"}, `{"selected_options":[{"value":"2"}],"type":"multi_static_select"}`},
		{"radio_buttons", []string{"1"}, `{"selected_option":{"value":"1"},"type":"radio_buttons"}`},
		{"static_select", []string{"0"}, `{"selected_option":{"value":"0"},"type":"static_select"}`},
		{"plain_text_input", []string{"blue"}, `{"type":"plain_text_input","value":"blue"}`},
	} {
		got, err := AnswerState(tc.typ, tc.values...)
		if err != nil {
			t.Errorf("AnswerState(%s) = %v", tc.typ, err)
			continue
		}
		if b, _ := json.Marshal(got); string(b) != tc.want {
			t.Errorf("AnswerState(%s, %v) = %s; want %s", tc.typ, tc.values, b, tc.want)
		}
	}
	for _, bad := range []struct {
		typ    string
		values []string
	}{{"datepicker", []string{"x"}}, {"plain_text_input", nil}, {"radio_buttons", []string{"0", "1"}}} {
		if _, err := AnswerState(bad.typ, bad.values...); err == nil {
			t.Errorf("AnswerState(%s, %v) = nil; want an error: it is not an answer the element takes", bad.typ, bad.values)
		}
	}
}

func TestElementTypeReadsTheTypeOfAnActionsElement(t *testing.T) {
	blocks := json.RawMessage(`[{"type":"input","block_id":"q0","element":{"type":"radio_buttons","action_id":"a0"}},
		{"type":"input","block_id":"q1","element":{"type":"plain_text_input","action_id":"a1"}}]`)
	if got := ElementType(blocks, "a0"); got != "radio_buttons" {
		t.Errorf("ElementType(a0) = %q; want radio_buttons", got)
	}
	if got := ElementType(blocks, "a1"); got != "plain_text_input" {
		t.Errorf("ElementType(a1) = %q; want plain_text_input", got)
	}
	if got := ElementType(blocks, "a9"); got != "" || ElementType(nil, "a0") != "" {
		t.Errorf("ElementType of an element that is not there = %q; want none", got)
	}
}

// ts values are seconds and microseconds, which are numbers: as strings "9.000001" is after "10.000000".
func TestCompareTSOrdersAsNumbers(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{{"9.000001", "10.000000", -1}, {"10.000000", "9.999999", 1}, {"5.000100", "5.000100", 0}, {"5.000009", "5.000010", -1}, {"5", "5.000001", -1}, {"5.5", "5.05", 1}} {
		if got := CompareTS(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareTS(%s, %s) = %d; want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestConsistentlyFailsAtOnceWhenTheConditionBreaks(t *testing.T) {
	n := 0
	err := Consistently(t.Context(), 5*time.Second, "n to stay small", func(context.Context) (bool, error) { n++; return n < 3, nil })
	if err == nil || !strings.Contains(err.Error(), "n to stay small") || n != 3 {
		t.Errorf("Consistently = %v after %d checks; want a failure on the third that names what was wanted", err, n)
	}
}

func TestConsistentlyHoldsForTheWholeWindow(t *testing.T) {
	start, calls := time.Now(), 0
	if err := Consistently(t.Context(), 120*time.Millisecond, "x", func(context.Context) (bool, error) { calls++; return true, nil }); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 120*time.Millisecond || calls < 2 {
		t.Errorf("returned after %v and %d checks; want it to watch the whole window", time.Since(start), calls)
	}
}

func TestConsistentlyReportsAnErrorFromTheCheck(t *testing.T) {
	boom := errors.New("boom")
	if err := Consistently(t.Context(), time.Second, "x", func(context.Context) (bool, error) { return true, boom }); !errors.Is(err, boom) {
		t.Errorf("Consistently = %v; want the check's error", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Consistently(ctx, time.Minute, "x", func(context.Context) (bool, error) { return true, nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("Consistently on a cancelled context = %v; want its error, at once", err)
	}
}

func TestQuestionControlReadsWhereAQuestionIsAnswered(t *testing.T) {
	blocks := json.RawMessage(`[{"type":"header"},{"type":"input","block_id":"q0","element":{"type":"checkboxes","action_id":"c0"}},
		{"type":"input","block_id":"q1","element":{"type":"plain_text_input","action_id":"t1"}},{"type":"actions","block_id":"submit","elements":[]}]`)
	if b, a, typ, ok := QuestionControl(blocks, 0); !ok || b != "q0" || a != "c0" || typ != "checkboxes" {
		t.Errorf("QuestionControl(0) = %q %q %q %v; want q0 c0 checkboxes", b, a, typ, ok)
	}
	if b, a, typ, ok := QuestionControl(blocks, 1); !ok || b != "q1" || a != "t1" || typ != "plain_text_input" {
		t.Errorf("QuestionControl(1) = %q %q %q %v; want q1 t1 plain_text_input", b, a, typ, ok)
	}
	if _, _, _, ok := QuestionControl(blocks, 2); ok {
		t.Error("QuestionControl found a question that is not there")
	}
	if _, _, _, ok := QuestionControl(json.RawMessage(`not json`), 0); ok {
		t.Error("QuestionControl read blocks that are not JSON")
	}
}

func TestApprovalControlsUseNeutralActionsAndToolVerdicts(t *testing.T) {
	blocks := json.RawMessage(`[{"type":"rich_text"},
		{"type":"input","block_id":"q0","element":{"type":"radio_buttons","action_id":"r0"}},
		{"type":"input","block_id":"q1","element":{"type":"radio_buttons","action_id":"r1"}},
		{"type":"actions","block_id":"submit","elements":[
			{"type":"button","action_id":"submit","value":"ix-1"},
			{"type":"button","action_id":"approve_all","value":"ix-1"},
			{"type":"button","action_id":"deny_all","value":"ix-1"}]}]`)
	for _, tc := range []struct {
		action chat.Action
		id     string
	}{
		{chat.ActionApproveAll, "approve_all"},
		{chat.ActionDenyAll, "deny_all"},
		{chat.ActionSubmit, "submit"},
	} {
		id, value, ok := approvalButton(blocks, tc.action)
		if !ok || id != tc.id || value != "ix-1" {
			t.Errorf("approvalButton(%s) = %q, %q, %v; want %q, ix-1, true", tc.action, id, value, ok, tc.id)
		}
	}
	values, err := approvalValues(blocks, []chat.ToolVerdict{chat.ToolApproved, chat.ToolDenied})
	if err != nil {
		t.Fatal(err)
	}
	want := fakeslack.StateValues{
		"q0": {"r0": {"type": "radio_buttons", "selected_option": map[string]string{"value": chat.ToolApproveValue}}},
		"q1": {"r1": {"type": "radio_buttons", "selected_option": map[string]string{"value": chat.ToolDenyValue}}},
	}
	if !reflect.DeepEqual(values, want) {
		t.Errorf("approvalValues = %v; want %v", values, want)
	}
}

func TestApprovalControlsRefuseMissingOrInvalidInputs(t *testing.T) {
	blocks := json.RawMessage(`[{"type":"actions","block_id":"submit","elements":[{"type":"button","action_id":"submit","value":"ix-1"}]}]`)
	if _, _, ok := approvalButton(blocks, chat.Action(99)); ok {
		t.Error("approvalButton with an unknown action succeeded")
	}
	if _, err := approvalValues(blocks, []chat.ToolVerdict{chat.ToolApproved}); err == nil {
		t.Error("approvalValues without a radio control succeeded")
	}
	if _, err := approvalValues(json.RawMessage(`not json`), []chat.ToolVerdict{chat.ToolApproved}); err == nil {
		t.Error("approvalValues with unreadable blocks succeeded")
	}
	radio := json.RawMessage(`[{"type":"input","block_id":"q0","element":{"type":"radio_buttons","action_id":"r0"}}]`)
	if _, err := approvalValues(radio, []chat.ToolVerdict{"unknown"}); err == nil {
		t.Error("approvalValues with an unknown verdict succeeded")
	}
}

// TestTasksReadsTheChunksOfTheView: a view decoded from the fake's control surface has its chunks on the view and none on
// the Message it embeds (JSON skips those), so the view's own Tasks method, which reads the Message's, finds none.
func TestTasksReadsTheChunksOfTheView(t *testing.T) {
	var v fakeslack.MessageView
	raw := `{"streaming":false,"chunks":[` +
		`{"type":"task_update","id":"s1","title":"check_status","status":"in_progress","details":"service=\"api\""},` +
		`{"type":"markdown_text","text":"hi"},` +
		`{"type":"task_update","id":"s1","title":"check_status","status":"complete","details":"service=\"api\""}]}`
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	if got := v.Tasks(); len(got) != 0 {
		t.Fatalf("the view's own Tasks = %+v; the premise of Tasks is that it finds none", got)
	}
	want := []fakeslack.Task{{ID: "s1", Title: "check_status", Status: "complete", Details: `service="api"`}}
	if got := Tasks(v); !slices.Equal(got, want) {
		t.Errorf("Tasks = %+v; want %+v", got, want)
	}
}
