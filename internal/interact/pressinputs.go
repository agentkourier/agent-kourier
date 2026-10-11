package interact

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// pressInputs is what a press holds in store.Press.Inputs, which the store keeps as it is and does not read: the
// button that was pressed, and the controls' state for ActionSubmit.
//
// There are two stored forms, and a press is written in the older one whenever that says all of it:
//
//   - A bare JSON array of chat.Answer, for ActionSubmit. It is the one form that every version has written, so a press
//     recorded before actions existed is read as a Submit, and a Submit recorded now is still read by the older
//     binary (a question is answered as it always was, and an approval is told "no longer available": the older
//     binary has no way to decide one).
//   - A JSON object {"action": "approve_all"} or {"action": "deny_all"} for the buttons that decide every tool at
//     once. The controls' state is not kept: those buttons ignore it. An older binary cannot decode an object into its
//     array, so it takes the press for one recorded by an even older version and tells the person the question is
//     no longer available, deleting the press. That is the safe direction: a decision that the older binary cannot
//     read is never read wrongly, as a Submit.
//
// The action is stored by name (chat.Action.MarshalText), so renumbering the constants never changes a press.
type pressInputs struct {
	Action  chat.Action   `json:"action"`
	Answers []chat.Answer `json:"answers,omitempty"`
}

// encodePress is the stored form of i's press. An action Agent Kourier does not define is an error: nothing is
// stored that no version could read.
func encodePress(i chat.Interaction) ([]byte, error) {
	if i.Action == chat.ActionSubmit {
		return json.Marshal(i.Answers)
	}
	return json.Marshal(pressInputs{Action: i.Action})
}

// decodePress reads either stored form. A text that is neither is an error, for the caller to treat as a press it
// cannot read. The object form is strict: it must name an action and hold no key but the two above, so that the
// state of Slack's controls by block and action, which the version before neutral answers recorded as an object too,
// is never taken for it.
func decodePress(raw []byte) (pressInputs, error) {
	if b := bytes.TrimSpace(raw); len(b) > 0 && b[0] == '{' {
		var in struct {
			Action  *chat.Action  `json:"action"`
			Answers []chat.Answer `json:"answers"`
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return pressInputs{}, err
		}
		if in.Action == nil {
			return pressInputs{}, errors.New("interact: a stored press names no action")
		}
		return pressInputs{Action: *in.Action, Answers: in.Answers}, nil
	}
	var answers []chat.Answer
	if err := json.Unmarshal(raw, &answers); err != nil {
		return pressInputs{}, err
	}
	return pressInputs{Action: chat.ActionSubmit, Answers: answers}, nil
}
