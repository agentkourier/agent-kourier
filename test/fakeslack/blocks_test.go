package fakeslack_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// One of each block courier can plausibly render must be accepted; a validator
// that rejects valid content would break the pilot with the E2E tier green.
func TestBlocksCourierRendersAreAccepted(t *testing.T) {
	fake, api := newFake(t)
	blocks := []slack.Block{
		slack.NewMarkdownBlock("", "**answer** with `code`"),
		slack.NewSectionBlock(mrkdwn("section"), []*slack.TextBlockObject{mrkdwn("field")}, nil),
		slack.NewHeaderBlock(plain("header")),
		slack.NewRichTextBlock("", slack.NewRichTextSection(slack.NewRichTextSectionTextElement("rich", nil))),
		slack.NewContextBlock("", mrkdwn("context")),
		slack.NewActionBlock("actions", slack.NewButtonBlockElement("approve", "yes", plain("Approve"))),
		slack.NewImageBlock("https://example.com/a.png", "alt", "", nil),
		slack.NewDividerBlock(),
	}
	post(t, api, slack.MsgOptionBlocks(blocks...))
	if got := fake.Messages("C1")[0]; len(got.Blocks) == 0 {
		t.Fatalf("stored message has no blocks: %+v", got)
	}
}

// WP-33 renders interactions with buttons, so the limits on them are tested at
// their boundaries, on both methods that write blocks.
func TestBlockRulesMatchSlack(t *testing.T) {
	blocks := func(b ...slack.Block) []slack.MsgOption { return []slack.MsgOption{slack.MsgOptionBlocks(b...)} }
	button := func(id, value, label string) *slack.ButtonBlockElement {
		return slack.NewButtonBlockElement(id, value, plain(label))
	}
	buttons := func(n int) []slack.BlockElement {
		out := make([]slack.BlockElement, n)
		for i := range out {
			out[i] = button(fmt.Sprint("a", i), "v", "Go")
		}
		return out
	}
	contexts := func(n int) []slack.MixedElement {
		out := make([]slack.MixedElement, n)
		for i := range out {
			out[i] = mrkdwn("c")
		}
		return out
	}
	fields := func(n int) []*slack.TextBlockObject {
		out := make([]*slack.TextBlockObject, n)
		for i := range out {
			out[i] = mrkdwn("f")
		}
		return out
	}
	md := slack.NewMarkdownBlock
	// A button as a section's accessory takes the same rules as one in an actions block.
	accessory := func(id, value, label string) []slack.MsgOption {
		return blocks(slack.NewSectionBlock(mrkdwn("s"), nil, slack.NewAccessory(button(id, value, label))))
	}

	for name, tc := range map[string]struct {
		opts []slack.MsgOption
		want string
	}{
		"markdown at limit":                    {blocks(md("", chars(12000))), ""},
		"markdown over limit":                  {blocks(md("", chars(12001))), "invalid_blocks"},
		"markdown cumulative":                  {blocks(md("", chars(6000)), md("", chars(6001))), "invalid_blocks"},
		"section text empty":                   {blocks(slack.NewSectionBlock(mrkdwn(""), nil, nil)), "invalid_blocks"},
		"section field empty":                  {blocks(slack.NewSectionBlock(nil, []*slack.TextBlockObject{mrkdwn("")}, nil)), "invalid_blocks"},
		"10 section fields":                    {blocks(slack.NewSectionBlock(nil, fields(10), nil)), ""},
		"11 section fields":                    {blocks(slack.NewSectionBlock(nil, fields(11), nil)), "invalid_blocks"},
		"25 actions elements":                  {blocks(slack.NewActionBlock("a", buttons(25)...)), ""},
		"26 actions elements":                  {blocks(slack.NewActionBlock("a", buttons(26)...)), "invalid_blocks"},
		"button text at limit":                 {blocks(slack.NewActionBlock("a", button("x", "v", chars(75)))), ""},
		"button text over limit":               {blocks(slack.NewActionBlock("a", button("x", "v", chars(76)))), "invalid_blocks"},
		"button value at limit":                {blocks(slack.NewActionBlock("a", button("x", chars(2000), "Go"))), ""},
		"button value over limit":              {blocks(slack.NewActionBlock("a", button("x", chars(2001), "Go"))), "invalid_blocks"},
		"action_id at limit":                   {blocks(slack.NewActionBlock("a", button(chars(255), "v", "Go"))), ""},
		"action_id over limit":                 {blocks(slack.NewActionBlock("a", button(chars(256), "v", "Go"))), "invalid_blocks"},
		"block_id at limit":                    {blocks(slack.NewActionBlock(chars(255), button("x", "v", "Go"))), ""},
		"block_id over limit":                  {blocks(slack.NewActionBlock(chars(256), button("x", "v", "Go"))), "invalid_blocks"},
		"10 context elements":                  {blocks(slack.NewContextBlock("", contexts(10)...)), ""},
		"11 context elements":                  {blocks(slack.NewContextBlock("", contexts(11)...)), "invalid_blocks"},
		"button text empty":                    {blocks(slack.NewActionBlock("a", button("x", "v", ""))), "invalid_blocks"},
		"button text one character":            {blocks(slack.NewActionBlock("a", button("x", "v", "G"))), ""},
		"accessory button at limits":           {accessory(chars(255), chars(2000), chars(75)), ""},
		"accessory button text empty":          {accessory("x", "v", ""), "invalid_blocks"},
		"accessory button text over limit":     {accessory("x", "v", chars(76)), "invalid_blocks"},
		"accessory button value over limit":    {accessory("x", chars(2001), "Go"), "invalid_blocks"},
		"accessory action_id over limit":       {accessory(chars(256), "v", "Go"), "invalid_blocks"},
		"section with neither text nor fields": {blocks(slack.NewSectionBlock(nil, nil, nil)), "invalid_blocks"},
		"section with only an accessory":       {blocks(slack.NewSectionBlock(nil, nil, slack.NewAccessory(button("x", "v", "Go")))), "invalid_blocks"},
		"section with only fields":             {blocks(slack.NewSectionBlock(nil, fields(1), nil)), ""},
		"duplicate block_id":                   {blocks(slack.NewActionBlock("same", button("x", "v", "Go")), slack.NewActionBlock("same", button("y", "v", "Go"))), "invalid_blocks"},
		"distinct block_ids":                   {blocks(slack.NewActionBlock("one", button("x", "v", "Go")), slack.NewActionBlock("two", button("y", "v", "Go"))), ""},
		"blocks without block_ids":             {blocks(slack.NewActionBlock("", button("x", "v", "Go")), slack.NewActionBlock("", button("y", "v", "Go"))), ""},
		"duplicate action_id in a block":       {blocks(slack.NewActionBlock("a", button("same", "v", "Go"), button("same", "w", "Go"))), "invalid_blocks"},
		"action_id reused across blocks":       {blocks(slack.NewActionBlock("a", button("same", "v", "Go")), slack.NewActionBlock("b", button("same", "v", "Go"))), ""},
		"alert block":                          {blocks(slack.NewAlertBlock(mrkdwn("x"))), "invalid_blocks"},
		"card block":                           {blocks(slack.NewCardBlock()), "invalid_blocks"},
		"carousel block":                       {blocks(slack.NewCarouselBlock()), "invalid_blocks"},
	} {
		t.Run(name, func(t *testing.T) { expectContent(t, tc.opts, tc.want) })
	}
}

func TestStreamContentLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		opt  slack.MsgOption
		want string
	}{
		"markdown_text at limit":   {slack.MsgOptionMarkdownText(chars(12000)), ""},
		"markdown_text over limit": {slack.MsgOptionMarkdownText(chars(12001)), "msg_too_long"},
		"chunk text at limit":      {slack.MsgOptionChunks(slack.NewMarkdownTextChunk(chars(12000))), ""},
		"chunk text over limit":    {slack.MsgOptionChunks(slack.NewMarkdownTextChunk(chars(12001))), "msg_too_long"},
		"50 blocks in a chunk":     {slack.MsgOptionChunks(slack.NewBlocksChunk(nDividers(50)...)), ""},
		"51 blocks in a chunk":     {slack.MsgOptionChunks(slack.NewBlocksChunk(nDividers(51)...)), "invalid_blocks"},
		"card in a chunk":          {slack.MsgOptionChunks(slack.NewBlocksChunk(slack.NewCardBlock())), ""},
		"duplicate block_id in a chunk": {slack.MsgOptionChunks(slack.NewBlocksChunk(
			slack.NewActionBlock("same", slack.NewButtonBlockElement("x", "v", plain("Go"))),
			slack.NewActionBlock("same", slack.NewButtonBlockElement("y", "v", plain("Go"))))), "invalid_blocks"},
		"empty section in a chunk": {slack.MsgOptionChunks(slack.NewBlocksChunk(slack.NewSectionBlock(mrkdwn(""), nil, nil))), "invalid_blocks"},
	} {
		t.Run(name, func(t *testing.T) {
			_, api, ts := newStream(t)
			_, _, err := api.AppendStream("C1", ts, tc.opt)
			if (err == nil) != (tc.want == "") || (err != nil && err.Error() != tc.want) {
				t.Fatalf("AppendStream = %v; want %q", err, tc.want)
			}
		})
	}
}

func nDividers(n int) []slack.Block {
	out := make([]slack.Block, n)
	for i := range out {
		out[i] = slack.NewDividerBlock()
	}
	return out
}

// The message as a whole is bounded even when every call is within its limit.
func TestStreamTextAccumulationIsBounded(t *testing.T) {
	// A start is bounded too, or its stream could never be stopped.
	_, startAPI := newFake(t)
	parent := post(t, startAPI, text("q"))
	startWith := func(chunks ...slack.StreamChunk) (string, error) {
		return startIn(startAPI, "C1", parent, append(recipients(), slack.MsgOptionChunks(chunks...))...)
	}
	md := func(n int) slack.StreamChunk { return slack.NewMarkdownTextChunk(chars(n)) }
	if _, err := startWith(md(12000), md(12000), md(12000), md(12000)); err == nil || err.Error() != "msg_too_long" {
		t.Fatalf("start with 48000 characters = %v; want msg_too_long", err)
	}
	full, err := startWith(md(12000), md(12000), md(12000), md(4000))
	if err != nil {
		t.Fatalf("start with exactly 40000: %v", err)
	}
	if _, _, err := startAPI.StopStream("C1", full); err != nil {
		t.Fatalf("stop after a full start: %v", err)
	}

	fake, api, ts := newStream(t)
	for i := range 3 {
		if _, _, err := api.AppendStream("C1", ts, slack.MsgOptionMarkdownText(chars(12000))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if _, _, err := api.AppendStream("C1", ts, slack.MsgOptionMarkdownText(chars(4000))); err != nil {
		t.Fatalf("append to exactly 40000: %v", err)
	}
	if _, _, err := api.AppendStream("C1", ts, slack.MsgOptionMarkdownText("x")); err == nil || err.Error() != "msg_too_long" {
		t.Fatalf("append past 40000 = %v; want msg_too_long", err)
	}
	if m := fake.Messages("C1"); len(m[len(m)-1].Text) != 40000 {
		t.Fatalf("a rejected append changed the text: %d characters", len(m[len(m)-1].Text))
	}
}

// A block or element type Slack does not have is refused, wherever it sits: a misspelling in courier
// must fail here as it does there, not be stored. These go over the wire as raw JSON because slack-go
// has no builder for a type it does not know.
func TestUnknownBlockAndElementTypesAreRefused(t *testing.T) {
	fake, _ := newFake(t)
	text := `{"type":"mrkdwn","text":"x"}`
	btn := `{"type":"button","action_id":"a","text":{"type":"plain_text","text":"Go"},"value":"v"}`
	var inputs []string
	for i, typ := range []string{"plain_text_input", "static_select", "multi_static_select", "radio_buttons", "checkboxes"} {
		inputs = append(inputs, fmt.Sprintf(`{"type":"input","block_id":"b%d","label":{"type":"plain_text","text":"l"},"element":{"type":%q,"action_id":"a"}}`, i, typ))
	}
	for name, tc := range map[string]struct{ blocks, want string }{
		"an unknown block type":                  {`[{"type":"markdownx","text":"x"}]`, "invalid_blocks"},
		"an empty block type":                    {`[{"type":""}]`, "invalid_blocks"},
		"an unknown element in an actions block": {`[{"type":"actions","block_id":"b","elements":[{"type":"buttonx","action_id":"a"}]}]`, "invalid_blocks"},
		"an unknown element in a context block":  {`[{"type":"context","elements":[{"type":"markdown","text":"x"}]}]`, "invalid_blocks"},
		"an unknown section accessory":           {`[{"type":"section","text":` + text + `,"accessory":{"type":"spinner"}}]`, "invalid_blocks"},
		"an unknown input element":               {`[{"type":"input","block_id":"b","label":{"type":"plain_text","text":"l"},"element":{"type":"plain_text_inputx","action_id":"a"}}]`, "invalid_blocks"},
		"a markdown block":                       {`[{"type":"markdown","text":"x"}]`, ""},
		"a context block of text and image":      {`[{"type":"context","elements":[` + text + `,{"type":"plain_text","text":"p"},{"type":"image","image_url":"https://example.com/a.png","alt_text":"a"}]}]`, ""},
		"a section with a button accessory":      {`[{"type":"section","text":` + text + `,"accessory":` + btn + `}]`, ""},
		"an input block of each courier control": {"[" + strings.Join(inputs, ",") + "]", ""},
	} {
		t.Run(name, func(t *testing.T) {
			body := rawBody(t, fake, "chat.postMessage", url.Values{"token": {testToken}, "channel": {"C1"}, "text": {"x"}, "blocks": {tc.blocks}})
			if got := strings.Contains(body, `"ok":true`); got != (tc.want == "") || (tc.want != "" && !strings.Contains(body, tc.want)) {
				t.Errorf("response %s; want error %q", body, tc.want)
			}
		})
	}
}
