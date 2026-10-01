package slack

import (
	slackgo "github.com/slack-go/slack"
)

// Interaction is a person pressing a button on a message in a channel: the Submit of an
// ask-user prompt, say. A message is not a modal, so the inputs the person filled in come
// with the press, in state.values, and no view_submission is involved (spec 8.4).
type Interaction struct {
	User      string // who pressed
	Channel   string
	MessageTS string // the message the button is on
	ThreadTS  string // its thread; empty if it is not in one
	// ActionID, BlockID and Value are the pressed button's own, which the renderer set and
	// which tell the session manager what was pressed.
	ActionID, BlockID, Value string
	// Inputs is the state of the message's input elements: block_id, then action_id.
	Inputs map[string]map[string]Input
}

// Input is one input element's answer.
type Input struct {
	Type string // the element type, such as "plain_text_input" or "static_select"
	// Values is what was entered or chosen: one value for a text input or a select, several
	// for a multi-select. An unanswered element, which Slack sends as null or empty, has none.
	// Only the types courier renders (text input, selects, radio buttons, checkboxes) have
	// values; any other type reads as unanswered.
	Values []string
}

// decodeInteraction returns the Interaction a callback carries, and whether it is one:
// the press of a button on a message by a person. A press by courier itself, by no one,
// of another kind (a modal's submission or close), or with no action is dropped.
// UNCONFIRMED(WP-04): the payload's fields, and that state.values holds null or empty for
// an unanswered input.
func decodeInteraction(botUser string, cb slackgo.InteractionCallback) (Interaction, bool) {
	if cb.Type != slackgo.InteractionTypeBlockActions || len(cb.ActionCallback.BlockActions) == 0 {
		return Interaction{}, false
	}
	act := cb.ActionCallback.BlockActions[0]
	i := Interaction{
		User: cb.User.ID, Channel: cb.Container.ChannelID, MessageTS: cb.Container.MessageTs, ThreadTS: cb.Container.ThreadTs,
		ActionID: act.ActionID, BlockID: act.BlockID, Value: act.Value,
	}
	if i.User == "" || i.User == botUser || i.Channel == "" || i.MessageTS == "" {
		return Interaction{}, false
	}
	if cb.BlockActionState != nil {
		i.Inputs = map[string]map[string]Input{}
		for block, actions := range cb.BlockActionState.Values {
			i.Inputs[block] = map[string]Input{}
			for id, a := range actions {
				i.Inputs[block][id] = Input{Type: string(a.Type), Values: inputValues(a)}
			}
		}
	}
	return i, true
}

// inputValues returns the non-empty answers of an input's state.
func inputValues(a slackgo.BlockAction) []string {
	var out []string
	for _, v := range append([]string{a.Value, a.SelectedOption.Value}, optionValues(a.SelectedOptions)...) {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func optionValues(opts []slackgo.OptionBlockObject) []string {
	var out []string
	for _, o := range opts {
		out = append(out, o.Value)
	}
	return out
}
