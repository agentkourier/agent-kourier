package fakeslack

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// StateValues is what a user entered in a message's input elements: block_id,
// then action_id, then the element's fields other than its type, such as
// {"value": "yes"} for a text input or {"selected_option": {"value": "a"}} for
// a select. The type comes from the element in the message, so a "type" given
// here must match it.
type StateValues map[string]map[string]map[string]any

// Click presses a button on a message the bot posted, as user, and delivers a
// block_actions interactive envelope for it. value must be the button's own
// value, since a user cannot change it. It returns the envelope_id.
func (s *Server) Click(user, messageTS, actionID, value string) (string, error) {
	channel, m, blocks, err := s.findMessage(messageTS)
	if err != nil {
		return "", err
	}
	btn, err := blocks.button(actionID)
	if err != nil {
		return "", err
	}
	if v, _ := btn.element["value"].(string); v != value {
		return "", invalid("fakeslack: button %q has value %q, not %q", actionID, v, value)
	}
	return s.blockActions(user, channel, m, btn, blocks, nil)
}

// Submit answers an inline ask-user prompt: it presses the Submit button
// submitActionID on a message the bot posted, as user, with values as the state
// of the message's input elements, and delivers a block_actions envelope. Every
// block_id and action_id in values must be a stateful element of the message,
// not a button, and a select's choices must be among its options. Inputs
// missing from values are sent unanswered, as Slack sends them. It returns the
// envelope_id.
//
// A message is not a modal, so Slack sends its inputs' state with the click on
// the button, in state.values, and never a view_submission, which belongs to
// views.open. That is the only way courier's inline ask-user in a thread (spec
// 8.4) can be answered, so this fake has no view_submission at all.
func (s *Server) Submit(user, messageTS, submitActionID string, values StateValues) (string, error) {
	channel, m, blocks, err := s.findMessage(messageTS)
	if err != nil {
		return "", err
	}
	btn, err := blocks.button(submitActionID)
	if err != nil {
		return "", err
	}
	return s.blockActions(user, channel, m, btn, blocks, values)
}

// blockActions delivers the block_actions payload for a press of btn.
//
// UNCONFIRMED(WP-04): the payload's fields, which are the ones courier needs
// and slack-go parses (no response_url, trigger_id, token or team domain); the
// message in it, which omits metadata; the envelope's accepts_response_payload
// being false; and that Submit answers with block_actions and not
// view_submission.
func (s *Server) blockActions(user, channel string, m Message, btn element, blocks elements, values StateValues) (string, error) {
	state, err := blocks.state(values)
	if err != nil {
		return "", err
	}
	m.Metadata = nil
	s.mu.Lock()
	name, now := s.users[user].Name, s.now()
	s.mu.Unlock()
	if name == "" {
		name = user
	}
	container := map[string]any{"type": "message", "message_ts": m.TS, "channel_id": channel, "is_ephemeral": false}
	if m.ThreadTS != "" {
		container["thread_ts"] = m.ThreadTS
	}
	action := map[string]any{
		"type": "button", "action_id": btn.element["action_id"], "block_id": btn.blockID,
		"text": btn.element["text"], "value": btn.element["value"],
		"action_ts": fmt.Sprintf("%d.%06d", now.Unix(), now.Nanosecond()/1000),
	}
	return s.SendEnvelope("interactive", map[string]any{
		"type": "block_actions", "api_app_id": s.bot.app, "team": map[string]any{"id": s.team.id},
		"user":    map[string]any{"id": user, "name": name, "team_id": s.team.id},
		"channel": map[string]any{"id": channel}, "container": container, "message": m,
		"state": map[string]any{"values": state}, "actions": []any{action},
	})
}

// findMessage returns the message with the given ts, whichever channel it is
// in (ts values are unique across the fake), and its interactive elements. It
// fails if any of them sits in a block with no block_id, which the user's
// state could not be keyed by.
//
// UNCONFIRMED(WP-04): that refusal. Slack generates a block_id for such a
// block; posting one still succeeds here.
func (s *Server) findMessage(ts string) (channel string, m Message, blocks elements, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch, msgs := range s.channels {
		for _, x := range msgs {
			if x.TS != ts {
				continue
			}
			els := findElements(x.Blocks)
			if slices.ContainsFunc(els, func(e element) bool { return e.blockID == "" }) {
				return "", Message{}, nil, invalid("fakeslack: message %s has an interactive block with no block_id; "+
					"Slack would generate one, so set explicit block IDs", ts)
			}
			return ch, x.clone(), els, nil
		}
	}
	return "", Message{}, nil, fmt.Errorf("fakeslack: no message with ts %q: %w", ts, ErrNotFound)
}

// element is an object with an action_id, and the block_id of the block that
// holds it.
type element struct {
	blockID string
	element map[string]any
}

type elements []element

// findElements collects every element with an action_id in a message's blocks:
// the elements of an actions block, a section's accessory, an input's element.
func findElements(blocks json.RawMessage) elements {
	var list []map[string]any
	if json.Unmarshal(blocks, &list) != nil {
		return nil
	}
	var out elements
	var walk func(blockID string, v any)
	walk = func(blockID string, v any) {
		switch t := v.(type) {
		case map[string]any:
			if _, ok := t["action_id"].(string); ok {
				out = append(out, element{blockID, t})
			}
			for _, x := range t {
				walk(blockID, x)
			}
		case []any:
			for _, x := range t {
				walk(blockID, x)
			}
		}
	}
	for _, b := range list {
		id, _ := b["block_id"].(string)
		walk(id, b)
	}
	return out
}

// state builds the state.values of a block_actions payload: every stateful
// element in the message (everything with an action_id but a button) unanswered,
// as {"type": <the element's type>, <its value field>: null}, with values laid
// over it.
//
// UNCONFIRMED(WP-04): the name of each element type's value field (unlisted
// types use "value"); that Slack sends unanswered inputs this way; and that
// multi-answer elements (checkboxes and the multi_* selects) are sent as an
// empty list where the others are null.
func (es elements) state(values StateValues) (StateValues, error) {
	out, defs := StateValues{}, map[[2]string]map[string]any{}
	for _, e := range es {
		typ, _ := e.element["type"].(string)
		if typ == "button" {
			continue
		}
		if out[e.blockID] == nil {
			out[e.blockID] = map[string]map[string]any{}
		}
		actionID := e.element["action_id"].(string)
		field := valueField(typ)
		var unanswered any // null, or an empty list where the field holds several answers
		if strings.HasSuffix(field, "s") {
			unanswered = []any{}
		}
		out[e.blockID][actionID] = map[string]any{"type": typ, field: unanswered}
		defs[[2]string{e.blockID, actionID}] = e.element
	}
	for blockID, actions := range values {
		for actionID, given := range actions {
			cur, ok := out[blockID][actionID]
			if !ok {
				return nil, fmt.Errorf("fakeslack: message has no stateful element %q in block %q (buttons have none): %w", actionID, blockID, ErrNotFound)
			}
			if typ, has := given["type"]; has && typ != cur["type"] {
				return nil, invalid("fakeslack: element %q is a %v, not a %v", actionID, cur["type"], typ)
			}
			if err := optionsOK(defs[[2]string{blockID, actionID}], given); err != nil {
				return nil, fmt.Errorf("fakeslack: element %q: %w", actionID, err)
			}
			maps.Copy(cur, given)
		}
	}
	return out, nil
}

// valueField returns the field that holds an element's answer in state.values.
func valueField(typ string) string {
	switch typ {
	case "static_select", "external_select", "radio_buttons":
		return "selected_option"
	case "multi_static_select", "multi_external_select", "checkboxes":
		return "selected_options"
	case "datepicker":
		return "selected_date"
	case "timepicker":
		return "selected_time"
	case "datetimepicker":
		return "selected_date_time"
	case "users_select":
		return "selected_user"
	case "multi_users_select":
		return "selected_users"
	case "conversations_select":
		return "selected_conversation"
	case "multi_conversations_select":
		return "selected_conversations"
	case "channels_select":
		return "selected_channel"
	case "multi_channels_select":
		return "selected_channels"
	case "rich_text_input":
		return "rich_text_value"
	}
	return "value"
}

// optionsOK checks that what the user picked in a select, checkbox or radio
// element is among the options the element offers. Elements that offer none in
// the message, such as external selects, accept anything.
func optionsOK(el, given map[string]any) error {
	var offered struct {
		Options []struct{ Value string } `json:"options"`
		Groups  []struct {
			Options []struct{ Value string } `json:"options"`
		} `json:"option_groups"`
	}
	var picked struct {
		One  *struct{ Value string }  `json:"selected_option"`
		Many []struct{ Value string } `json:"selected_options"`
	}
	raw, _ := json.Marshal(el)
	gotRaw, _ := json.Marshal(given)
	if json.Unmarshal(raw, &offered) != nil || json.Unmarshal(gotRaw, &picked) != nil {
		return invalid("selected options are not objects with a value")
	}
	allowed := map[string]bool{}
	for _, o := range offered.Options {
		allowed[o.Value] = true
	}
	for _, g := range offered.Groups {
		for _, o := range g.Options {
			allowed[o.Value] = true
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	if picked.One != nil {
		picked.Many = append(picked.Many, *picked.One)
	}
	for _, o := range picked.Many {
		if !allowed[o.Value] {
			return invalid("option %q is not among the options offered", o.Value)
		}
	}
	return nil
}

// button returns the button with the given action_id.
func (es elements) button(actionID string) (element, error) {
	for _, e := range es {
		if e.element["action_id"] == actionID {
			if e.element["type"] != "button" {
				return element{}, invalid("fakeslack: element %q is a %v, not a button", actionID, e.element["type"])
			}
			return e, nil
		}
	}
	return element{}, fmt.Errorf("fakeslack: message has no element with action_id %q: %w", actionID, ErrNotFound)
}
