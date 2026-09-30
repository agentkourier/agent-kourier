package fakeslack_test

import (
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// postAsk posts, as the bot, an ask-user prompt in a thread: a question input,
// an Approve button and a Submit button. It returns the thread root and the
// prompt's ts.
func postAsk(t *testing.T, fake *fakeslack.Server) (root, ask string) {
	t.Helper()
	bot := slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/"))
	root = post(t, bot, text("investigating"))
	ask = post(t, bot, slack.MsgOptionTS(root), text("a question"), slack.MsgOptionBlocks(
		slack.NewInputBlock("q1", plain("Which cluster?"), nil, slack.NewPlainTextInputBlockElement(nil, "answer_1")),
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
	if cb.Message.Timestamp != ask || cb.Message.BotID != "BFAKEBOT" || len(cb.Message.Blocks.BlockSet) != 2 {
		t.Fatalf("payload message = %+v; want the stored bot message with its blocks", cb.Message)
	}
	waitAcked(t, fake, id)
}

func TestSubmitDeliversInputStateWithTheButton(t *testing.T) {
	fake, smc := connected(t)
	_, ask := postAsk(t, fake)
	answer := map[string]any{"type": "plain_text_input", "value": "prod-east"}

	id, err := fake.Submit("U1", ask, "submit", fakeslack.StateValues{"q1": {"answer_1": answer}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	gotID, cb := nextInteraction(t, smc)
	act := cb.ActionCallback.BlockActions
	got := cb.BlockActionState.Values["q1"]["answer_1"]
	if gotID != id || cb.Type != slack.InteractionTypeBlockActions || len(act) != 1 || act[0].ActionID != "submit" || act[0].Value != "go" ||
		got.Type != "plain_text_input" || got.Value != "prod-east" {
		t.Fatalf("envelope %q, payload %+v, state %+v; want the submit press carrying the answer", gotID, cb, cb.BlockActionState)
	}
	waitAcked(t, fake, id)
}

func TestInteractionsRejectImpossibleUserActions(t *testing.T) {
	fake, _ := connected(t)
	_, ask := postAsk(t, fake)
	ok := fakeslack.StateValues{"q1": {"answer_1": map[string]any{"type": "plain_text_input", "value": "x"}}}
	tests := []struct {
		name string
		do   func() (string, error)
	}{
		{"click on a message that is not there", func() (string, error) { return fake.Click("U1", "1.000001", "approve", "yes") }},
		{"click on an action that is not there", func() (string, error) { return fake.Click("U1", ask, "nope", "yes") }},
		{"click on an input", func() (string, error) { return fake.Click("U1", ask, "answer_1", "") }},
		{"click with the wrong value", func() (string, error) { return fake.Click("U1", ask, "approve", "no") }},
		{"submit with a button that is not there", func() (string, error) { return fake.Submit("U1", ask, "nope", ok) }},
		{"submit an input that is not there", func() (string, error) {
			return fake.Submit("U1", ask, "submit", fakeslack.StateValues{"q1": {"nope": map[string]any{}}})
		}},
		{"submit an input in the wrong block", func() (string, error) {
			return fake.Submit("U1", ask, "submit", fakeslack.StateValues{"buttons": {"answer_1": map[string]any{}}})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if id, err := tc.do(); err == nil {
				t.Fatalf("succeeded with envelope %q; want an error", id)
			}
		})
	}
}
