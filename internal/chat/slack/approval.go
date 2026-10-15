package slack

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// This file is the Block Kit layout of a tool approval (a Prompt of tools and the Ending of one): what the person
// sees of each tool call, and the buttons that decide it (spec 8.4).
//
// One tool is a header, a section that names it, a rich_text block of its arguments in preformatted text, and an
// Approve and a Deny button, which are approve all and deny all. Several tools are a header, then for each tool
// two blocks: a rich_text block that holds the tool's name, numbered ("N. name"), and then its arguments in
// preformatted text, and an input block whose label is the same "N. name" with one radio button group, Approve or
// Deny; then a Submit button beside Approve all and Deny all. The name travels with the arguments, in the block
// above the radio, so that a reader cannot pair the arguments of one tool with the radio of the next. 20 tools,
// the most a pause carries, take 40 of the 50 blocks a message holds, and with the header, the sub-agent's
// line, the deadline's and the buttons 44. A single tool is not numbered, which would read badly; the number
// counts toward the name's limit, since a place's limit is the whole of what is shown.
//
// The live probe of 2026-10-13 posted and updated this layout in Slack with 20 tools (42 blocks) and 24 tools (50 blocks),
// including the radio_buttons and the rich_text pairing of a name with its arguments. A raw block_actions capture records
// the control payload separately without logging user or message data.
//
// The name is agent text. In the input's label and in a section it is a plain_text object, escaped as every plain_text
// object is; in a rich_text block it is the characters of a text element, which is literal, so it is cleaned and not
// escaped. The arguments are agent text too, and are the characters of a text element of a rich_text_preformatted
// (argsBlock): Block Kit makes a mention, a broadcast, a channel or a link in rich_text an element of its own type,
// so no character of text is one, and there is nothing to escape.
//
// They are not a fenced block of a markdown block, as the code of an agent's answer is (escape.go), for two reasons
// that the conformance suite holds every adapter to. Slack shows code in a markdown block literally (rows 01 and 03
// of tasks/plans/slack-code-rendering.md), so the "&lt;" that keeps "<!" out of code would be shown as "&lt;", and
// the person would be asked to approve arguments that are not the arguments: "&lt;!x" and "<!x" would look alike.
// And a line of them that looks like a fence would end the block there, so that the rest was shown as prose.
const (
	approvalHeader   = "The agent needs your approval"
	approvalFallback = "The agent needs your approval."
	noArguments      = "(no arguments)"

	approveWord, denyWord = "Approve", "Deny"

	// maxApprovalArgs is the conservative per-tool budget in cleaned UTF-16 units. Slack accepted 3001 ASCII units and
	// 3002 UTF-16 units of astral text in the 2026-10-13 probe; keep the documented 3000-character budget for a block.
	maxApprovalArgs = maxSection
	// maxApprovalTotal is the conservative aggregate budget in cleaned UTF-16 units for the generated approval layout.
	// Slack accepted 3,600 ASCII units in the 24-tool, 50-block fixture and refused 4,992 with msg_blocks_too_long;
	// the 20-tool fixture accepted 5,200 and refused 5,360. This is a layout-specific budget, not Slack's absolute limit:
	// tool names and other blocks consume message space too, so the local cap stays below the observed rejection boundary.
	maxApprovalTotal = 3600
	// maxApprovalBlocks is a conservative budget for the complete canonical Block Kit JSON in UTF-16 units. The
	// approval renderer leaves optional HTML escaping to Slack's JSON client, so this measures the JSON syntax and the
	// required JSON string escapes without charging agent text for that second encoding step.
	// The 24-tool fixture was accepted at 16,291 Unicode characters and refused at 17,683; 16,000 leaves room for
	// variation in names, labels and notices without treating the observed boundary as universal.
	maxApprovalBlocks = 16000
)

// The action IDs of the buttons that decide a tool approval, and the block that holds the buttons of one tool.
// The pressed button's action ID is how a press is told apart (Platform.interaction); the Submit of several
// tools is submitActionID, as a question's is. Every button is valued with the prompt's ID.
const (
	approveAllActionID = "approve_all"
	denyAllActionID    = "deny_all"
	decideBlockID      = "decide"
)

func button(actionID, value, label, style string) map[string]any {
	b := map[string]any{"type": "button", "action_id": actionID, "value": value, "text": plainObject(label)}
	if style != "" {
		b["style"] = style
	}
	return b
}

// shownName is the name of tool i of n as the person is told it: cleaned, on one line, and numbered if there are
// several tools ("2. name"); or "Tool N" if there is nothing left of the name, so that a tool can still be told apart
// and decided. It is not escaped. It is one string for the input's label and the rich_text block's text.
func shownName(name string, i, n int) string {
	cleaned := chat.Clean(name, true)
	switch {
	case cleaned == "":
		return "Tool " + strconv.Itoa(i+1)
	case n > 1:
		return strconv.Itoa(i+1) + ". " + cleaned
	}
	return cleaned
}

// noArgs says whether a tool has no arguments to show.
func noArgs(args string) bool { return strings.TrimSpace(args) == "" }

// richSection is a rich_text_section of one text element.
func richSection(s string) map[string]any {
	return map[string]any{"type": "rich_text_section", "elements": []map[string]any{{"type": "text", "text": s}}}
}

// argsElements are the cleaned arguments of a tool as the element of a rich_text block: preformatted text, or a
// section that says there are none.
func argsElements(args string) map[string]any {
	if noArgs(args) {
		return richSection(noArguments)
	}
	return map[string]any{"type": "rich_text_preformatted", "elements": []map[string]any{{"type": "text", "text": args}}}
}

func richText(elements ...map[string]any) map[string]any {
	return map[string]any{"type": "rich_text", "elements": elements}
}

// argsBlock is the cleaned arguments of a tool alone in a rich_text block.
func argsBlock(args string) map[string]any { return richText(argsElements(args)) }

// toolBlock is the name of a tool, as shownName has it, and its cleaned arguments, in one rich_text block.
func toolBlock(name, args string) map[string]any {
	return richText(richSection(name), argsElements(args))
}

// approvalContent encodes the canonical approval JSON used for the local budget. HTML escaping is unnecessary inside
// a JSON string and makes the local budget charge six UTF-16 units for one agent character such as '<'; the Slack
// client performs that optional escaping when it serializes the Web API form.
func approvalContent(fallback string, blocks []map[string]any) (Content, error) {
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(blocks); err != nil {
		return Content{}, fmt.Errorf("slack: encode the approval blocks: %w", err)
	}
	return Content{Text: fallback, Blocks: bytes.TrimSuffix(raw.Bytes(), []byte{'\n'})}, nil
}

// renderToolPrompt is a prompt of tools as a Slack message (see the top of this file). A tool whose name or
// arguments, cleaned, are over the limit of their place is chat.ErrTooLarge and is never shown cut, because a person cannot
// approve what they cannot read; and so are tools whose arguments together are more than a message shows (maxApprovalTotal),
// or that need more than 50 blocks.
func renderToolPrompt(p chat.Prompt) (Content, error) {
	nameLimit, argsLimit := caps.Limit(chat.WhereToolName), caps.Limit(chat.WhereToolArgs)
	names, args := make([]string, len(p.Tools)), make([]string, len(p.Tools))
	total := 0
	for i, t := range p.Tools {
		names[i] = shownName(t.Name, i, len(p.Tools))
		if n := chat.Units(names[i]); n > nameLimit {
			return Content{}, fmt.Errorf("%w: the name of tool %d is %d units, at most %d", chat.ErrTooLarge, i+1, n, nameLimit)
		}
		args[i] = chat.Clean(t.Args, false)
		if n := chat.Units(args[i]); n > argsLimit {
			return Content{}, fmt.Errorf("%w: the arguments of tool %d are %d units, at most %d", chat.ErrTooLarge, i+1, n, argsLimit)
		}
		total += chat.Units(args[i])
	}
	if total > maxApprovalTotal {
		return Content{}, fmt.Errorf("%w: the arguments of the tools are %d units together, a message shows at most %d", chat.ErrTooLarge, total, maxApprovalTotal)
	}
	blocks := []map[string]any{{"type": "header", "text": plainObject(cmp.Or(text(p.Hint, maxHeader, true), approvalHeader))}}
	if len(p.Tools) == 1 { // Approve and Deny decide the only tool: they are approve all and deny all
		blocks = append(blocks, section(text(names[0], maxSection, true)), argsBlock(args[0]))
		blocks = append(blocks, map[string]any{"type": "actions", "block_id": decideBlockID, "elements": []map[string]any{
			button(approveAllActionID, p.ID, approveWord, "primary"),
			button(denyAllActionID, p.ID, denyWord, "danger"),
		}})
	} else {
		for i := range p.Tools {
			// Approve and Deny valued by index, "0" and "1", which are chat.ToolApproveValue and chat.ToolDenyValue.
			choice := chat.Question{Text: names[i], Choices: []string{approveWord, denyWord}}
			blocks = append(blocks, toolBlock(names[i], args[i]), questionBlocks(i, choice)[0])
		}
		blocks = append(blocks, map[string]any{"type": "actions", "block_id": submitBlockID, "elements": []map[string]any{
			button(submitActionID, p.ID, "Submit", "primary"),
			button(approveAllActionID, p.ID, "Approve all", ""),
			button(denyAllActionID, p.ID, "Deny all", "danger"),
		}})
	}
	if len(blocks) > maxBlocks {
		return Content{}, fmt.Errorf("%w: %d blocks", chat.ErrTooLarge, len(blocks))
	}
	blocks, fallback := withNotices(blocks, p, approvalFallback)
	out, err := approvalContent(fallback, blocks)
	if err != nil {
		return Content{}, err
	}
	if n := chat.Units(string(out.Blocks)); n > maxApprovalBlocks {
		return Content{}, fmt.Errorf("%w: approval blocks are %d UTF-16 units, at most %d", chat.ErrTooLarge, n, maxApprovalBlocks)
	}
	return out, nil
}

// toolEndingBlocks are the tools of an ended prompt: for each a section of its name (numbered, as in the prompt, if
// there are several) that holds what was decided of it in a field if something was, and its arguments. An ending is never refused for size, so what is long is cut and marked: a name
// at the limit of its place, the arguments at theirs, and, when the arguments together are more than a message shows,
// each to its share, which is what it needs or an equal part of what is left.
func toolEndingBlocks(e chat.Ending) []map[string]any {
	tools := e.Prompt.Tools
	nameLimit, argsLimit := caps.Limit(chat.WhereToolName), caps.Limit(chat.WhereToolArgs)
	cleaned := make([]string, len(tools))
	needs := make([]int, len(tools))
	for i, t := range tools {
		cleaned[i] = chat.Clean(t.Args, false)
		needs[i] = min(chat.Units(cleaned[i]), argsLimit)
	}
	room := shares(needs, maxApprovalTotal)
	var blocks []map[string]any
	for i, t := range tools {
		name := section(text(chat.Truncate(shownName(t.Name, i, len(tools)), nameLimit), maxSection, true))
		if i < len(e.Verdicts) {
			verdict := map[chat.ToolVerdict]string{chat.ToolApproved: "Approved", chat.ToolDenied: "Denied"}[e.Verdicts[i]]
			name["fields"] = []map[string]any{plainObject(verdict)}
		}
		blocks = append(blocks, name, argsBlock(chat.Truncate(cleaned[i], room[i])))
	}
	return blocks
}

// shares divides budget among the needs: each gets what it needs, or an equal part of what is left once the smaller
// needs are met. The shares add up to at most budget.
func shares(needs []int, budget int) []int {
	order := make([]int, len(needs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(needs[a], needs[b]) })
	out := make([]int, len(needs))
	for n, i := range order {
		out[i] = min(needs[i], budget/(len(order)-n))
		budget -= out[i]
	}
	return out
}
