package teams

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// actionInvoke is the name of the invoke that a press of an Action.Execute sends.
const actionInvoke = "adaptiveCard/action"

// messageAnswer is the type of an invoke's answer whose value is a text that Teams shows the presser.
const messageAnswer = "application/vnd.microsoft.activity.message"

// pressWait is how long a press's invoke waits for the press to be decided (chat.InlineDecider), so that the presser is told what it came
// to in the answer. It ends inside the invoke's deadline (invokeTimeout) with time left to answer: the Teams client gives up on an
// invoke whose answer comes between 12.5 and 13 seconds after it arrived, measured on a test tenant on 2026-11-18, well before Teams'
// service closes the request at about 13.9 (see invokeTimeout). A press not decided by then is answered as received, and the press
// worker decides it after, as it would have; the read of the channel's members that decides it is bounded at rosterTimeout on the
// worker's own context, so a slower read lands there.
const pressWait = 6 * time.Second

// What a press is answered with, in its request, when it is not decided within pressWait, or the sink does not wait for its decision:
// before it is decided, which the press worker does after the answer.
const (
	// textPromptReceived answers the press of a prompt's button. It says only what is known in the request: the press is recorded,
	// and the card is replaced for everyone once the question is decided, by this press or another, or ends.
	textPromptReceived = "Agent Kourier received your press. The card changes for everyone when the question is decided."
	// textButtonReceived answers the press of a message's button, such as a vote.
	textButtonReceived = "Agent Kourier received your press."
	// textPressNotRecorded answers a press whose recording returned an error: it may still have been recorded (the write can commit
	// and its answer be lost), so it says may not, and Teams does not send the press again. A second press is recorded on its own
	// and decided as any press is.
	textPressNotRecorded = "Agent Kourier may not have recorded your press. Please press again."
	// textPressUnusable answers a press that names no button of Agent Kourier's, or that came from no one or of no message.
	textPressUnusable = "Agent Kourier cannot take this press."
)

// pressKind is what a press is of: a prompt's button or a message's.
type pressKind int

const (
	pressPrompt pressKind = iota
	pressButton
)

// inputKey is the ID of an input of a prompt's card that a Submit reads: its control and the index of its question (inputID).
var inputKey = regexp.MustCompile(`^([` + string(inputText) + string(inputSingle) + string(inputMulti) + `])(\d{1,4})$`)

// actionValue is the value of an adaptiveCard/action invoke: the pressed action, and its data with the card's inputs merged in by ID.
type actionValue struct {
	Action struct {
		Type string                     `json:"type"`
		Data map[string]json.RawMessage `json:"data"`
	} `json:"action"`
}

// ownData is the data that the adapter put on an action (execute): a prompt's ID and its action, or a message's button, its value and,
// for a card posted under a text, the text's ID and the MAC that binds it to the card (buttonsCard, buttonMAC).
type ownData struct {
	Prompt *string `json:"prompt"`
	Action *string `json:"action"`
	Button *string `json:"button"`
	Value  *string `json:"value"`
	Of     *string `json:"of"`
	MAC    *string `json:"mac"`
}

// Invoke implements InvokeHandler. It answers a press of a card's Action.Execute in the request, with a message for the presser. A sink
// that decides presses in the request (chat.InlineDecider, the broker's) records the press and waits up to pressWait for the press worker
// to decide it: a press decided so is answered with what it came to (recorded, already recorded, or why it was refused) when the
// decision has words for the presser. A press not decided within the wait, or decided with nothing to tell, is answered as received, and
// one whose decision is to be tried again too; the worker decides it after the answer, and a refusal then reaches nobody. Any other sink
// records the press and returns, and the press is answered as received. A press whose recording returned an error is answered may not be
// recorded, since Teams does not send the press again, and one that names no button of Agent Kourier's, or has no presser or message,
// cannot be taken. The card changes for everyone when the question is decided, by its own update and not by this answer. Any other
// invoke is answered 200 with no body.
func (c *Client) Invoke(ctx context.Context, sink chat.Sink, inv Invoke) InvokeAnswer {
	if inv.Name != actionInvoke {
		c.log.DebugContext(ctx, "teams: an invoke was answered with no body; it is not a press", "name", quote(inv.Name))
		return InvokeAnswer{Status: http.StatusOK}
	}
	press, kind, ok := c.press(inv)
	if !ok {
		c.log.DebugContext(ctx, "teams: a press was not taken; it names no button of Agent Kourier's, or no presser or message")
		return toldPresser(textPressUnusable)
	}
	out, err := c.decide(ctx, sink, press)
	if err != nil {
		c.log.WarnContext(ctx, "teams: a press could not be recorded; the presser was told to press again", "error", err.Error())
		return toldPresser(textPressNotRecorded)
	}
	if out.Decided && out.Text != "" {
		return toldPresser(out.Text)
	}
	if kind == pressButton {
		return toldPresser(textButtonReceived)
	}
	return toldPresser(textPromptReceived)
}

// decide hands press to sink, and returns what it came to when sink decides it within pressWait (chat.InlineDecider); a sink that does
// not decide presses records it, and nothing is known of its outcome.
func (c *Client) decide(ctx context.Context, sink chat.Sink, press chat.Interaction) (chat.PressOutcome, error) {
	d, ok := sink.(chat.InlineDecider)
	if !ok {
		return chat.PressOutcome{}, sink.Interaction(ctx, press)
	}
	wctx, cancel := context.WithTimeout(ctx, c.pressWait)
	defer cancel()
	return d.DecidePress(wctx, press)
}

// toldPresser is the answer to an invoke that Teams shows the presser as text.
func toldPresser(text string) InvokeAnswer {
	body, _ := json.Marshal(struct {
		StatusCode int    `json:"statusCode"`
		Type       string `json:"type"`
		Value      string `json:"value"`
	}{http.StatusOK, messageAnswer, text}) // marshalling a struct of a number and strings does not fail
	return InvokeAnswer{Status: http.StatusOK, Body: body}
}

// press is the neutral Interaction of inv, a press of an Action.Execute of a card the adapter rendered, and what it is of; false when
// inv has no presser or message, or its action is not an Action.Execute with the adapter's data of one of two forms: a prompt's ID with
// an action chat.Action reads (promptAction), or a message's button with its value (buttonsCard).
//
// The press is of the message that Post returned, which the card's message is for a prompt and for buttons alone. A message's buttons
// under its text are a card of their own, whose data names the text ("of"): the press is of the text, in the card's thread, so that it
// names the message that the outbox recorded, as a press on Slack does. The data comes from the presser's client, so the text is taken
// only with its MAC, which must be the one this Client computes for the press's connection, channel and thread, the text and the value
// (buttonMAC): data that names another text, another value or another thread, or a card posted under another secret, makes no press,
// nor does a text ID that a card cannot name (nameableText). The key of the press is the invoke's, of the card's message.
func (c *Client) press(inv Invoke) (chat.Interaction, pressKind, bool) {
	if inv.User == "" || inv.At.ID == "" || inv.At.Channel == "" {
		return chat.Interaction{}, 0, false
	}
	var v actionValue
	if json.Unmarshal(inv.Value, &v) != nil || v.Action.Type != "Action.Execute" {
		return chat.Interaction{}, 0, false
	}
	var own ownData
	if raw, ok := v.Action.Data[dataKey]; !ok || json.Unmarshal(raw, &own) != nil {
		return chat.Interaction{}, 0, false
	}
	out := chat.Interaction{User: inv.User, At: inv.At, Delivery: pressKey(inv)}
	switch {
	case own.Prompt != nil && own.Action != nil && own.Button == nil && own.Value == nil && own.Of == nil && own.MAC == nil:
		if out.Action.UnmarshalText([]byte(*own.Action)) != nil {
			return chat.Interaction{}, 0, false
		}
		out.PromptID = *own.Prompt
		if out.Action == chat.ActionSubmit {
			out.Answers = answersOf(v.Action.Data)
		}
		return out, pressPrompt, true
	case own.Value != nil && own.Prompt == nil && own.Action == nil:
		out.PromptID = *own.Value
		switch {
		case own.Of != nil:
			of, root := *own.Of, cmp.Or(inv.At.Thread, inv.At.ID) // the card is a reply in the text's thread, whose root this is
			if !nameableText(of) || own.MAC == nil ||
				!hmac.Equal([]byte(*own.MAC), []byte(c.buttonMAC(inv.At.Channel, root, of, *own.Value))) {
				return chat.Interaction{}, 0, false
			}
			out.At.ID, out.At.Thread = of, threadOf(root, of)
		case own.MAC != nil:
			return chat.Interaction{}, 0, false
		}
		return out, pressButton, true
	}
	return chat.Interaction{}, 0, false
}

// answersOf reads a Submit's answers from the inputs Teams merged into the action's data, by the IDs the card gave them (inputID): a
// text's value whole, a choice of one as the index it holds, a choice of several split at its commas (Teams joins the chosen values
// with them), in the order of the questions. A key of no input, a value that is not a string, and an input that holds nothing are left
// out; what the answers mean for the prompt (a question answered twice, an index past its options) is the session's to decide.
func answersOf(data map[string]json.RawMessage) []chat.Answer {
	var out []chat.Answer
	for key, raw := range data {
		m := inputKey.FindStringSubmatch(key)
		var s string
		if m == nil || json.Unmarshal(raw, &s) != nil {
			continue
		}
		q, _ := strconv.Atoi(m[2]) // at most four digits, as inputKey matched
		var values []string
		if m[1] == string(inputMulti) {
			for _, part := range strings.Split(s, ",") {
				if part != "" {
					values = append(values, part)
				}
			}
		} else if s != "" {
			values = []string{s}
		}
		if len(values) > 0 {
			out = append(out, chat.Answer{Question: q, Values: values})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Question < out[j].Question })
	return out
}

// pressKey is the key of a press by which the broker records it once (chat.Interaction.Delivery): the presser, the channel, the message
// and the invoke's activity ID, each prefixed by its length, hashed with SHA-256 and written in hex. A second delivery of the invoke
// has the same activity ID, and another press another; "" when the invoke has no ID.
func pressKey(inv Invoke) string {
	if inv.ID == "" {
		return ""
	}
	h := sha256.New()
	for _, part := range []string{inv.User, inv.At.Channel, inv.At.ID, inv.ID} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}
