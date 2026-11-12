package slack

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
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
	// Only the types Agent Kourier renders (text input, selects, radio buttons, checkboxes) have
	// values, each from its own field; any other type reads as unanswered.
	Values []string
}

// decodeInteraction returns the Interaction a callback carries, and whether it is one:
// the press of a button on a message by a person. A press by Agent Kourier itself, by no one,
// of another kind (a modal's submission or close), or with no action is dropped.
// A live probe on 2026-10-13 captured a real Submit block_actions payload with both rendered
// radio buttons unanswered. Slack sent the input entries with no selected value; the decoder
// consequently produces no answers for them. Other callback fields are decoded only as needed
// by this adapter and are not inferred from that probe.
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

// inputValues returns the non-empty answers of an input's state, from the one field its type
// answers in: a mix of fields, or a type Agent Kourier does not render, is not read, so that an
// answer is never one the person did not give.
func inputValues(a slackgo.BlockAction) []string {
	var answers, out []string
	switch a.Type {
	case "plain_text_input":
		answers = []string{a.Value}
	case "static_select", "radio_buttons":
		answers = []string{a.SelectedOption.Value}
	case "multi_static_select", "checkboxes":
		answers = optionValues(a.SelectedOptions)
	}
	for _, v := range answers {
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

// interaction is a press as the neutral model has it. The pressed button's action ID, in the block that holds it, says
// which button it was. The adapter renders two blocks of buttons for a prompt: decide, for one tool, holds Approve and
// Deny, and submit holds Submit and, for several tools, Approve all and Deny all. Approve and Approve all are
// ActionApproveAll, Deny and Deny all are ActionDenyAll, and they ignore the controls, so a press of them carries no
// answers. Submit is the one button whose controls are read: the answers are read from the state of the controls, by
// the block and action IDs that renderPrompt made, and each only as the control it was asked in. The radio of a tool
// is read like a question's, and its value is the index of the option, "0" for Approve and "1" for Deny
// (chat.ToolApproveValue, chat.ToolDenyValue).
//
// A button the adapter does not render in that block makes no action and carries no answers, so that it can never be
// read as a decision. In a block the adapter renders for a prompt (submit, decide) it names no prompt (PromptID empty),
// which interact.Pipeline.Press and feedback.Handler.Press both refuse as not theirs, and the broker's handler drops,
// acknowledged: Submit in decide is such a button. In any other block, such as the block of a message part, it is any
// button, even when its action ID is one of the prompt's: its value is its own and is passed on for its handler to read
// (a feedback button is routed by it).
func (p *Platform) interaction(i Interaction) chat.Interaction {
	out := chat.Interaction{User: i.User, At: p.ref(i.Channel, i.ThreadTS, i.MessageTS), PromptID: i.Value}
	if i.BlockID != submitBlockID && i.BlockID != decideBlockID {
		return out
	}
	switch {
	case i.ActionID == approveAllActionID:
		out.Action = chat.ActionApproveAll
	case i.ActionID == denyAllActionID:
		out.Action = chat.ActionDenyAll
	case i.ActionID == submitActionID && i.BlockID == submitBlockID:
		out.Answers = answers(i.Inputs)
	default:
		out.PromptID = ""
	}
	return out
}

var blockIndex = regexp.MustCompile(`^q(\d{1,4})$`)

// answers reads the answer of each question from the inputs. An element of another type than its action ID
// says, or an input that holds nothing, is an unanswered question: an answer is never one the person did not
// give.
func answers(inputs map[string]map[string]Input) []chat.Answer {
	var out []chat.Answer
	for block, actions := range inputs {
		m := blockIndex.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		q, _ := strconv.Atoi(m[1])
		for id, in := range actions {
			control, ok := controlOf(id, q)
			if !ok || controlElement[control] != in.Type || len(in.Values) == 0 {
				continue
			}
			values := slices.Clone(in.Values)
			if control == controlNumbers {
				values = positions(in.Values)
			}
			out = append(out, chat.Answer{Question: q, Values: values})
		}
	}
	slices.SortFunc(out, func(a, b chat.Answer) int { return cmp.Compare(a.Question, b.Question) })
	return out
}

// controlOf is the control of the action ID id of question q, or ok false if it is not one of that question.
func controlOf(id string, q int) (byte, bool) {
	if len(id) < 2 || id[1:] != strconv.Itoa(q) {
		return 0, false
	}
	_, ok := controlElement[id[0]]
	return id[0], ok
}

// positions turns the numbers a person typed beside a numbered list (1-based, separated by commas or
// spaces) into the indices the neutral model answers choices with. A token that is not a number stays as
// it was typed, for the caller to find that it is no choice.
func positions(typed []string) []string {
	var out []string
	for _, v := range typed {
		for _, tok := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
			if n, err := strconv.Atoi(tok); err == nil && n >= 1 {
				out = append(out, strconv.Itoa(n-1))
			} else {
				out = append(out, tok)
			}
		}
	}
	return out
}
