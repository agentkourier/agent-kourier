package fakeslack_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// postAsk posts, as the bot, an ask-user prompt in a thread: a question input,
// a select, an Approve button and a Submit button. It returns the thread root and the
// prompt's ts.
func postAsk(t *testing.T, fake *fakeslack.Server) (root, ask string) {
	t.Helper()
	bot := slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/"))
	root = post(t, bot, text("investigating"))
	ask = post(t, bot, slack.MsgOptionTS(root), text("a question"), slack.MsgOptionBlocks(
		slack.NewInputBlock("q1", plain("Which cluster?"), nil, slack.NewPlainTextInputBlockElement(nil, "answer_1")),
		slack.NewInputBlock("q2", plain("Which env?"), nil, slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, plain("Pick"), "pick",
			slack.NewOptionBlockObject("a", plain("A"), nil), slack.NewOptionBlockObject("b", plain("B"), nil))),
		slack.NewActionBlock("buttons",
			slack.NewButtonBlockElement("approve", "yes", plain("Approve")),
			slack.NewButtonBlockElement("submit", "go", plain("Submit")))))
	return root, ask
}

// nextInteraction waits for an interactive envelope, acks it, and returns its
// envelope_id and payload.
func nextInteraction(t *testing.T, smc *socketmode.Client) (string, slack.InteractionCallback) {
	t.Helper()
	ev := waitEvent(t, smc, socketmode.EventTypeInteractive)
	if err := smc.Ack(*ev.Request); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	return ev.Request.EnvelopeID, ev.Data.(slack.InteractionCallback)
}

func TestClickDeliversBlockActions(t *testing.T) {
	fake, smc := connected(t)
	root, ask := postAsk(t, fake)

	id, err := fake.Click("U1", ask, "approve", "yes")
	if err != nil || id == "" {
		t.Fatalf("Click = %q, %v", id, err)
	}
	gotID, cb := nextInteraction(t, smc)
	act := cb.ActionCallback.BlockActions
	if gotID != id || cb.Type != slack.InteractionTypeBlockActions || cb.User.ID != "U1" || cb.Channel.ID != "C1" ||
		cb.Container.MessageTs != ask || cb.Container.ThreadTs != root || cb.Container.ChannelID != "C1" ||
		len(act) != 1 || act[0].ActionID != "approve" || act[0].BlockID != "buttons" || act[0].Value != "yes" || act[0].Text.Text != "Approve" {
		t.Fatalf("envelope %q, payload %+v; want a block_actions click on approve in %s", gotID, cb, ask)
	}
	if cb.Message.Timestamp != ask || cb.Message.BotID != "BFAKEBOT" || len(cb.Message.Blocks.BlockSet) != 3 {
		t.Fatalf("payload message = %+v; want the stored bot message with its blocks", cb.Message)
	}
	waitAcked(t, fake, id)
}

func TestSubmitSendsEveryInputWithTheAnswersOverlaid(t *testing.T) {
	fake, smc := connected(t)
	_, ask := postAsk(t, fake)

	// A partial answer: the select goes out unanswered, with its type from the message.
	id, err := fake.Submit("U1", ask, "submit", fakeslack.StateValues{"q1": {"answer_1": {"value": "prod-east"}}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	gotID, cb := nextInteraction(t, smc)
	act := cb.ActionCallback.BlockActions
	text, pick := cb.BlockActionState.Values["q1"]["answer_1"], cb.BlockActionState.Values["q2"]["pick"]
	if gotID != id || len(act) != 1 || act[0].ActionID != "submit" || act[0].Value != "go" ||
		text.Type != "plain_text_input" || text.Value != "prod-east" ||
		pick.Type != "static_select" || pick.SelectedOption.Value != "" || len(cb.BlockActionState.Values) != 2 {
		t.Fatalf("envelope %q, payload %+v, state %+v; want the answered input and the unanswered select", gotID, cb, cb.BlockActionState)
	}
	waitAcked(t, fake, id)

	// A select answer, and a click, which also carries the full state.
	if _, err := fake.Submit("U1", ask, "submit", fakeslack.StateValues{"q2": {"pick": {"selected_option": map[string]any{"value": "b"}}}}); err != nil {
		t.Fatalf("Submit select: %v", err)
	}
	if _, cb := nextInteraction(t, smc); cb.BlockActionState.Values["q2"]["pick"].SelectedOption.Value != "b" {
		t.Fatalf("state = %+v; want the select answered b", cb.BlockActionState)
	}
	if _, err := fake.Click("U1", ask, "approve", "yes"); err != nil {
		t.Fatalf("Click: %v", err)
	}
	if _, cb := nextInteraction(t, smc); cb.BlockActionState.Values["q1"]["answer_1"].Type != "plain_text_input" {
		t.Fatalf("click state = %+v; want every input, unanswered", cb.BlockActionState)
	}
}

func TestInteractionsRejectImpossibleUserActions(t *testing.T) {
	fake, _ := connected(t)
	_, ask := postAsk(t, fake)
	submit := func(values fakeslack.StateValues) func() (string, error) {
		return func() (string, error) { return fake.Submit("U1", ask, "submit", values) }
	}
	tests := []struct {
		name string
		do   func() (string, error)
	}{
		{"click on a message that is not there", func() (string, error) { return fake.Click("U1", "1.000001", "approve", "yes") }},
		{"click on an action that is not there", func() (string, error) { return fake.Click("U1", ask, "nope", "yes") }},
		{"click on an input", func() (string, error) { return fake.Click("U1", ask, "answer_1", "") }},
		{"click with the wrong value", func() (string, error) { return fake.Click("U1", ask, "approve", "no") }},
		{"submit with a button that is not there", func() (string, error) {
			return fake.Submit("U1", ask, "nope", fakeslack.StateValues{"q1": {"answer_1": {"value": "x"}}})
		}},
		{"submit an input that is not there", submit(fakeslack.StateValues{"q1": {"nope": {}}})},
		{"submit an input in the wrong block", submit(fakeslack.StateValues{"buttons": {"answer_1": {}}})},
		{"submit state for a button", submit(fakeslack.StateValues{"buttons": {"approve": {"value": "x"}}})},
		{"submit with a type the element does not have", submit(fakeslack.StateValues{"q1": {"answer_1": {"type": "static_select"}}})},
		{"submit an option the select does not offer", submit(fakeslack.StateValues{"q2": {"pick": {"selected_option": map[string]any{"value": "zzz"}}}})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if id, err := tc.do(); err == nil {
				t.Fatalf("succeeded with envelope %q; want an error", id)
			}
		})
	}
}

func TestInteractiveBlockWithoutABlockIDCannotBePressed(t *testing.T) {
	fake, _ := connected(t)
	bot := slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/"))
	ts := post(t, bot, text("no ids"), slack.MsgOptionBlocks(slack.NewActionBlock("", slack.NewButtonBlockElement("go", "v", plain("Go")))))
	if _, err := fake.Click("U1", ts, "go", "v"); err == nil || !strings.Contains(err.Error(), "block_id") {
		t.Fatalf("Click on a block without a block_id = %v; want an error that asks for explicit block IDs", err)
	}
}

// TestUnansweredInputsCarryTheirValueFieldOnTheWire reads the raw envelope, since
// slack-go decodes a missing value field and a null one to the same zero value.
func TestUnansweredInputsCarryTheirValueFieldOnTheWire(t *testing.T) {
	leakCheck(t)
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	ws := dial(t, fake)
	if _, _, err := ws.ReadMessage(); err != nil { // hello
		t.Fatalf("read hello: %v", err)
	}
	elements := []struct{ typ, field string }{
		{"plain_text_input", "value"}, {"rich_text_input", "rich_text_value"},
		{"static_select", "selected_option"}, {"external_select", "selected_option"}, {"radio_buttons", "selected_option"},
		{"multi_static_select", "selected_options"}, {"multi_external_select", "selected_options"}, {"checkboxes", "selected_options"},
		{"users_select", "selected_user"}, {"multi_users_select", "selected_users"},
		{"conversations_select", "selected_conversation"}, {"multi_conversations_select", "selected_conversations"},
		{"channels_select", "selected_channel"}, {"multi_channels_select", "selected_channels"},
		{"datepicker", "selected_date"}, {"timepicker", "selected_time"}, {"datetimepicker", "selected_date_time"},
	}
	blocks := make([]string, 0, len(elements)+1)
	for i, e := range elements {
		blocks = append(blocks, fmt.Sprintf(`{"type":"input","block_id":"b%d","label":{"type":"plain_text","text":"L"},"element":{"type":%q,"action_id":"a%d"}}`, i, e.typ, i))
	}
	blocks = append(blocks, `{"type":"actions","block_id":"btns","elements":[{"type":"button","action_id":"submit","value":"go","text":{"type":"plain_text","text":"Go"}}]}`)
	posted := rawBody(t, fake, "chat.postMessage", url.Values{"token": {testToken}, "channel": {"C1"}, "text": {"x"}, "blocks": {"[" + strings.Join(blocks, ",") + "]"}})
	var resp struct{ TS string }
	if err := json.Unmarshal([]byte(posted), &resp); err != nil || resp.TS == "" {
		t.Fatalf("post = %s, %v", posted, err)
	}

	if _, err := fake.Submit("U1", resp.TS, "submit", nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	var env struct {
		Payload struct {
			State struct {
				Values map[string]map[string]map[string]any
			}
		}
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	for i, e := range elements {
		var empty any // null, or an empty list for the multi-answer elements
		if strings.HasSuffix(e.field, "s") {
			empty = []any{}
		}
		want := map[string]any{"type": e.typ, e.field: empty}
		if got := env.Payload.State.Values[fmt.Sprintf("b%d", i)][fmt.Sprintf("a%d", i)]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s unanswered = %v; want %v, with the value field present", e.typ, got, want)
		}
	}
}
