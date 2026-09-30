package fakeslack

import (
	"encoding/json"
	"fmt"
)

// StateValues is the state of a message's input elements, as a
// block_actions payload reports it: block_id, then action_id, then the
// element's state, such as {"type": "plain_text_input", "value": "yes"}.
type StateValues map[string]map[string]any

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
		return "", fmt.Errorf("fakeslack: button %q has value %q, not %q", actionID, v, value)
	}
	return s.blockActions(user, channel, m, btn, nil)
}

// Submit answers an inline ask-user prompt: it presses the Submit button
// submitActionID on a message the bot posted, as user, with values as the state
// of the message's input elements, and delivers a block_actions envelope. Every
// block_id and action_id in values must exist in the message. It returns the
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
	for blockID, actions := range values {
		for actionID := range actions {
			if !blocks.has(blockID, actionID) {
				return "", fmt.Errorf("fakeslack: message %s has no element %q in block %q: %w", messageTS, actionID, blockID, ErrNotFound)
			}
		}
	}
	return s.blockActions(user, channel, m, btn, values)
}

// blockActions delivers the block_actions payload for a press of btn.
//
// UNCONFIRMED(WP-04): the payload's fields, which are the ones courier needs
// and slack-go parses (no response_url, trigger_id, token or team domain); the
// message in it, which omits metadata; that state.values holds only the
// elements the caller gave, where Slack sends every input in the message,
// unset ones as null; a block_id of "" for a block sent without one, where
// Slack generates one; and that Submit answers with block_actions and not
// view_submission.
func (s *Server) blockActions(user, channel string, m Message, btn element, state StateValues) (string, error) {
	m.Metadata = nil
	if state == nil {
		state = StateValues{}
	}
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
		"type": "block_actions", "api_app_id": s.bot.app, "team": map[string]any{"id": teamID},
		"user":    map[string]any{"id": user, "name": name, "team_id": teamID},
		"channel": map[string]any{"id": channel}, "container": container, "message": m,
		"state": map[string]any{"values": state}, "actions": []any{action},
	})
}

// findMessage returns the message with the given ts, whichever channel it is
// in (ts values are unique across the fake), and its interactive elements.
func (s *Server) findMessage(ts string) (channel string, m Message, blocks elements, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch, msgs := range s.channels {
		for _, x := range msgs {
			if x.TS == ts {
				return ch, x.clone(), findElements(x.Blocks), nil
			}
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

func (es elements) has(blockID, actionID string) bool {
	for _, e := range es {
		if e.blockID == blockID && e.element["action_id"] == actionID {
			return true
		}
	}
	return false
}

// button returns the button with the given action_id.
func (es elements) button(actionID string) (element, error) {
	for _, e := range es {
		if e.element["action_id"] == actionID {
			if e.element["type"] != "button" {
				return element{}, fmt.Errorf("fakeslack: element %q is a %v, not a button", actionID, e.element["type"])
			}
			return e, nil
		}
	}
	return element{}, fmt.Errorf("fakeslack: message has no element with action_id %q: %w", actionID, ErrNotFound)
}
