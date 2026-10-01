package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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
		t.Errorf("OutboxID of another app's tag = %q; want none: only courier's own tag names an outbox row", got)
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
