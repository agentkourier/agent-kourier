package e2e

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	chatslack "github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// BotMessages returns the messages of a thread that the bot posted, in the order the thread has them.
func BotMessages(thread []fakeslack.MessageView) []fakeslack.MessageView {
	var out []fakeslack.MessageView
	for _, m := range thread {
		if m.BotID != "" {
			out = append(out, m)
		}
	}
	return out
}

// Tasks returns the task cards of a streamed message, as fakeslack.Message.Tasks does. A view read from the fake's control
// surface has its chunks on the view and none on the Message it embeds, so the view's own Tasks method finds nothing.
func Tasks(v fakeslack.MessageView) []fakeslack.Task {
	return fakeslack.Message{Chunks: v.Chunks}.Tasks()
}

// HasAction reports whether blocks, a message's blocks as JSON, hold an element with the action_id, at any
// depth: an input's element, a button in an actions block. Blocks that are absent or unreadable hold none.
func HasAction(blocks json.RawMessage, actionID string) bool { return findAction(blocks, actionID, "") }

// HasButton is HasAction for a button, which is what a prompt's Submit is and what a decided prompt no
// longer has.
func HasButton(blocks json.RawMessage, actionID string) bool {
	return findAction(blocks, actionID, "button")
}

// HasBlockText reports whether a decoded Slack text field contains want. JSON decoding turns Slack's
// HTML-safe escapes, such as \u003c and \u003e around a user mention, back into the visible text.
func HasBlockText(blocks json.RawMessage, want string) bool {
	var v any
	if json.Unmarshal(blocks, &v) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			if text, ok := x["text"].(string); ok && strings.Contains(text, want) {
				return true
			}
			for _, child := range x {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range x {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}

// ApprovalPrompt returns the bot's approval prompt in thread, or false when the thread has none. Approval prompts
// always carry the neutral approve-all and deny-all buttons, including a prompt with several tools. A question prompt
// can also have a submit button, so that button alone is deliberately not enough to identify an approval.
func ApprovalPrompt(thread []fakeslack.MessageView) (fakeslack.MessageView, bool) {
	for _, m := range BotMessages(thread) {
		if HasButton(m.Blocks, chat.ActionApproveAll.String()) || HasButton(m.Blocks, chat.ActionDenyAll.String()) {
			return m, true
		}
	}
	return fakeslack.MessageView{}, false
}

// PressApproval presses an approval control described by the neutral chat action. For Submit, verdicts are
// selected by tool order and the helper derives the Slack block and action IDs from the prompt itself. The
// scenario therefore does not depend on the adapter's block layout.
func (s *Slack) PressApproval(ctx context.Context, user string, prompt fakeslack.MessageView, action chat.Action, verdicts ...chat.ToolVerdict) (string, error) {
	actionID, value, ok := approvalButton(prompt.Blocks, action)
	if !ok {
		return "", fmt.Errorf("e2e: approval message %s has no %s control", prompt.TS, action)
	}
	if action != chat.ActionSubmit {
		if len(verdicts) != 0 {
			return "", fmt.Errorf("e2e: %s does not take per-tool verdicts", action)
		}
		return s.Click(ctx, user, prompt.TS, actionID, value)
	}
	values, err := approvalValues(prompt.Blocks, verdicts)
	if err != nil {
		return "", fmt.Errorf("e2e: build %s approval press: %w", action, err)
	}
	return s.Submit(ctx, user, prompt.TS, actionID, values)
}

// PressStaleApproval presses an approval button from the prompt snapshot a
// person saw before the broker edited it. It models a second person's stale
// Slack interaction arriving after the first decision won.
func (s *Slack) PressStaleApproval(ctx context.Context, user string, prompt fakeslack.MessageView, action chat.Action) (string, error) {
	if action == chat.ActionSubmit {
		return "", fmt.Errorf("e2e: stale %s approval presses are buttons only", action)
	}
	actionID, value, ok := approvalButton(prompt.Blocks, action)
	if !ok {
		return "", fmt.Errorf("e2e: approval message %s has no %s control", prompt.TS, action)
	}
	return s.ClickStale(ctx, user, prompt.Message, actionID, value)
}

// approvalButton reads the value of the button for action from an approval prompt. It matches the complete action ID,
// including the adapter's submit button, and ignores controls from a different message part.
func approvalButton(blocks json.RawMessage, action chat.Action) (actionID, value string, ok bool) {
	if action != chat.ActionSubmit && action != chat.ActionApproveAll && action != chat.ActionDenyAll {
		return "", "", false
	}
	var root any
	if json.Unmarshal(blocks, &root) != nil {
		return "", "", false
	}
	want := action.String()
	var walk func(any)
	walk = func(v any) {
		if ok {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			typ, _ := x["type"].(string)
			id, _ := x["action_id"].(string)
			if typ == "button" && id == want {
				value, ok = x["value"].(string)
				if ok {
					actionID = id
				}
				return
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(root)
	return actionID, value, ok
}

// approvalValues builds the fake Slack state for a Submit press. Verdicts are selected by tool order; a shorter slice
// intentionally models a partial (or empty) Submit, which the interaction flow refuses without deciding the prompt.
func approvalValues(blocks json.RawMessage, verdicts []chat.ToolVerdict) (fakeslack.StateValues, error) {
	values := fakeslack.StateValues{}
	for i, verdict := range verdicts {
		blockID, actionID, typ, ok := QuestionControl(blocks, i)
		if !ok {
			return nil, fmt.Errorf("tool %d has no radio control", i+1)
		}
		if typ != "radio_buttons" {
			return nil, fmt.Errorf("tool %d uses %s, not radio_buttons", i+1, typ)
		}
		var selected string
		switch verdict {
		case chat.ToolApproved:
			selected = chat.ToolApproveValue
		case chat.ToolDenied:
			selected = chat.ToolDenyValue
		default:
			return nil, fmt.Errorf("tool %d has unknown verdict %q", i+1, verdict)
		}
		state, err := AnswerState(typ, selected)
		if err != nil {
			return nil, err
		}
		if values[blockID] == nil {
			values[blockID] = map[string]map[string]any{}
		}
		values[blockID][actionID] = state
	}
	return values, nil
}

// findAction reports whether blocks hold an element with the action_id and, if typ is not empty, that type.
func findAction(blocks json.RawMessage, actionID, typ string) bool {
	var v any
	if json.Unmarshal(blocks, &v) != nil {
		return false
	}
	var walk func(v any) bool
	walk = func(v any) bool {
		switch t := v.(type) {
		case map[string]any:
			if t["action_id"] == actionID && (typ == "" || t["type"] == typ) {
				return true
			}
			for _, x := range t {
				if walk(x) {
					return true
				}
			}
		case []any:
			for _, x := range t {
				if walk(x) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}

// OutboxID is the outbox ID Agent Kourier tagged the message with, which the outbox looks a post up by after a
// crash, or "" if the message carries no such tag.
func OutboxID(m fakeslack.MessageView) string {
	if m.Metadata == nil || m.Metadata.EventType != chatslack.MetadataEventType {
		return ""
	}
	id, _ := m.Metadata.EventPayload["outbox_id"].(string)
	return id
}

// AnswerState is the state of an input element of the type typ holding values, for Slack.Submit: the types a
// prompt's questions are rendered as. The options of a choice are valued by the index of the choice. It refuses
// a type it does not know, and a count of values the type cannot take.
func AnswerState(typ string, values ...string) (map[string]any, error) {
	options := make([]map[string]string, len(values))
	for i, v := range values {
		options[i] = map[string]string{"value": v}
	}
	switch typ {
	case "checkboxes", "multi_static_select":
		return map[string]any{"type": typ, "selected_options": options}, nil
	case "radio_buttons", "static_select":
		if len(values) != 1 {
			return nil, fmt.Errorf("e2e: a %s takes one value, not %d", typ, len(values))
		}
		return map[string]any{"type": typ, "selected_option": options[0]}, nil
	case "plain_text_input":
		if len(values) != 1 {
			return nil, fmt.Errorf("e2e: a %s takes one value, not %d", typ, len(values))
		}
		return map[string]any{"type": typ, "value": values[0]}, nil
	}
	return nil, fmt.Errorf("e2e: no answer state for an element of type %q", typ)
}

// ElementType is the type of the element with the action_id in blocks, as a prompt rendered it, or "" if
// there is none. A scenario answers with the type the prompt has, and does not assume it.
func ElementType(blocks json.RawMessage, actionID string) string {
	var v any
	if json.Unmarshal(blocks, &v) != nil {
		return ""
	}
	var found string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if t["action_id"] == actionID {
				found, _ = t["type"].(string)
			}
			for _, x := range t {
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(v)
	return found
}

// CompareTS orders two Slack message ts values, which are seconds and microseconds: as numbers, not as
// strings, in which "9.000001" is after "10.000000". It returns -1, 0 or 1. A value that is not a ts is
// compared as a string.
func CompareTS(a, b string) int {
	sa, ma, oka := splitTS(a)
	sb, mb, okb := splitTS(b)
	if !oka || !okb {
		return strings.Compare(a, b)
	}
	if c := cmp.Compare(sa, sb); c != 0 {
		return c
	}
	return cmp.Compare(ma, mb)
}

// splitTS is the seconds and the microseconds of a ts.
func splitTS(ts string) (sec, micro int64, ok bool) {
	whole, frac, _ := strings.Cut(ts, ".")
	frac = (frac + "000000")[:6]
	sec, err1 := strconv.ParseInt(whole, 10, 64)
	micro, err2 := strconv.ParseInt(frac, 10, 64)
	return sec, micro, err1 == nil && err2 == nil
}

// Consistently checks cond now and then every pollInterval for d, and fails the first time it is false or
// returns an error. It is for the one thing a scenario must show by waiting: that something did not happen.
// It watches for the whole of d, so keep d short, and say in desc what was meant to hold. A cancelled ctx
// ends it with ctx's error.
func Consistently(ctx context.Context, d time.Duration, desc string, cond func(context.Context) (bool, error)) error {
	end := time.NewTimer(d)
	defer end.Stop()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		switch ok, err := cond(ctx); {
		case err != nil:
			return fmt.Errorf("checking that %s: %w", desc, err)
		case !ok:
			return fmt.Errorf("e2e: %s did not hold", desc)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("checking that %s: %w", desc, ctx.Err())
		case <-end.C:
			return nil
		case <-tick.C:
		}
	}
}

// QuestionControl is where the answer to question i of a prompt goes, as the prompt's blocks say it: the input
// block's ID, the ID of the action of its element, and the element's type. A scenario answers with what the prompt
// has and assumes no IDs, so a change to the layout of a prompt cannot silently turn a press into one that answers
// nothing. ok is false if the blocks hold no input for the question.
func QuestionControl(blocks json.RawMessage, i int) (blockID, actionID, typ string, ok bool) {
	var bs []struct {
		Type    string `json:"type"`
		BlockID string `json:"block_id"`
		Element struct {
			Type     string `json:"type"`
			ActionID string `json:"action_id"`
		} `json:"element"`
	}
	if json.Unmarshal(blocks, &bs) != nil {
		return "", "", "", false
	}
	want := "q" + strconv.Itoa(i) // the adapter names the input block of question i so; it is the one ID read here
	for _, b := range bs {
		if b.Type == "input" && b.BlockID == want && b.Element.ActionID != "" {
			return b.BlockID, b.Element.ActionID, b.Element.Type, true
		}
	}
	return "", "", "", false
}
