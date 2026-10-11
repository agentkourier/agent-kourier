package interact

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// The stored forms of a press, pinned on literal JSON: a press is a record that outlives the process that wrote it, and the
// version that reads it may not be the one that wrote it.

// TestEncodePressWritesSubmitAsTheOlderFormAndTheOtherButtonsAsAnObject: a Submit is a bare array, as every version has written
// it; the buttons that decide every tool are an object that names the action, with nothing of the controls.
func TestEncodePressWritesSubmitAsTheOlderFormAndTheOtherButtonsAsAnObject(t *testing.T) {
	answers := []chat.Answer{{Question: 0, Values: []string{"1"}}, {Question: 2, Values: []string{"0"}}}
	for _, tc := range []struct {
		name string
		in   chat.Interaction
		want string
	}{
		{"submit", chat.Interaction{Answers: answers}, `[{"Question":0,"Values":["1"]},{"Question":2,"Values":["0"]}]`},
		{"submit of nothing", chat.Interaction{}, `null`},
		{"approve all", chat.Interaction{Action: chat.ActionApproveAll}, `{"action":"approve_all"}`},
		{"deny all", chat.Interaction{Action: chat.ActionDenyAll}, `{"action":"deny_all"}`},
		{"approve all drops the controls", chat.Interaction{Action: chat.ActionApproveAll, Answers: answers}, `{"action":"approve_all"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodePress(tc.in)
			if err != nil || string(got) != tc.want {
				t.Errorf("encodePress = %s, %v; want %s", got, err, tc.want)
			}
		})
	}
	for _, a := range []chat.Action{9, -1} {
		if got, err := encodePress(chat.Interaction{Action: a}); err == nil {
			t.Errorf("encodePress of action %d = %s; want an error: nothing is stored that no version could read", a, got)
		}
	}
}

// TestDecodePressReadsWhatEveryVersionWrote: the bare array of the version on main is a Submit, and so is a press that names no
// answers at all. The object names the action.
func TestDecodePressReadsWhatEveryVersionWrote(t *testing.T) {
	one := []chat.Answer{{Question: 0, Values: []string{"0"}}}
	for _, tc := range []struct {
		name, stored string
		want         pressInputs
	}{
		{"a bare array, as main writes it", `[{"Question":0,"Values":["0"]}]`, pressInputs{Action: chat.ActionSubmit, Answers: one}},
		{"a bare array with white space around it", " \n[{\"Question\":0,\"Values\":[\"0\"]}]\n", pressInputs{Action: chat.ActionSubmit, Answers: one}},
		{"an empty array", `[]`, pressInputs{Action: chat.ActionSubmit, Answers: []chat.Answer{}}},
		{"null, which a Submit of nothing was", `null`, pressInputs{Action: chat.ActionSubmit}},
		{"approve all", `{"action":"approve_all"}`, pressInputs{Action: chat.ActionApproveAll}},
		{"deny all", `{"action":"deny_all"}`, pressInputs{Action: chat.ActionDenyAll}},
		{"submit, named", `{"action":"submit","answers":[{"Question":0,"Values":["0"]}]}`, pressInputs{Action: chat.ActionSubmit, Answers: one}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodePress([]byte(tc.stored))
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("decodePress(%s) = %+v, %v; want %+v", tc.stored, got, err, tc.want)
			}
		})
	}
}

// TestDecodePressRefusesWhatIsNotAPressOfThisVersion: the state of Slack's controls by block and action, which the version
// before neutral answers wrote, is an object too, and must not be taken for one with an action: it is the press of an older
// version and the person is told the question is gone.
func TestDecodePressRefusesWhatIsNotAPressOfThisVersion(t *testing.T) {
	for name, stored := range map[string]string{
		"slack's state by block and action": `{"q0":{"a0":{"Type":"plain_text_input","Values":["x"]}}}`,
		"an empty object":                   `{}`,
		"answers and no action":             `{"answers":[{"Question":0,"Values":["0"]}]}`,
		"an action this version lacks":      `{"action":"abstain"}`,
		"an action of null":                 `{"action":null}`,
		"an action as a number":             `{"action":1}`,
		"an object with another key":        `{"action":"approve_all","note":"x"}`,
		"a string":                          `"approve_all"`,
		"a number":                          `3`,
		"nothing":                           ``,
		"text":                              `not json`,
	} {
		if got, err := decodePress([]byte(stored)); err == nil {
			t.Errorf("%s: decodePress = %+v; want an error", name, got)
		}
	}
}

// TestAPressOfTheNewFormIsUnreadableToTheVersionBefore: the claim in pressInputs' comment, kept honest. The version on main
// decodes Inputs into []chat.Answer, which fails for an object, and its press pipeline then tells the person the question
// is gone: the safe direction. A Submit is the bare array it always was, and decodes there unchanged.
func TestAPressOfTheNewFormIsUnreadableToTheVersionBefore(t *testing.T) {
	for _, stored := range []string{`{"action":"approve_all"}`, `{"action":"deny_all"}`} {
		var older []chat.Answer
		if err := json.Unmarshal([]byte(stored), &older); err == nil {
			t.Errorf("the version before reads %s as %+v; want it unable to, so that it is told the question is gone", stored, older)
		}
	}
	raw, err := encodePress(chat.Interaction{Answers: []chat.Answer{{Question: 1, Values: []string{"0"}}}})
	var older []chat.Answer
	if err != nil || json.Unmarshal(raw, &older) != nil || !reflect.DeepEqual(older, []chat.Answer{{Question: 1, Values: []string{"0"}}}) {
		t.Errorf("a Submit written now, read as the version before reads it: %+v, %v (%s)", older, err, raw)
	}
}
