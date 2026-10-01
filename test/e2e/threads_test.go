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
	if got := OutboxID(view("U1", "", "z")); got != "" {
		t.Errorf("OutboxID of a message with no metadata = %q; want none", got)
	}
}

func TestAnswerStatesAreWhatTheFakeTakes(t *testing.T) {
	got, _ := json.Marshal(fakeslack.StateValues{"q0": {"a0": ChoicesState("0", "1")}, "q1": {"a1": TextState("blue")}})
	want := `{"q0":{"a0":{"selected_options":[{"value":"0"},{"value":"1"}],"type":"checkboxes"}},"q1":{"a1":{"type":"plain_text_input","value":"blue"}}}`
	if string(got) != want {
		t.Errorf("states = %s; want %s", got, want)
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
