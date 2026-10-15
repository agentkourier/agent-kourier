//go:build liveprobe

package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/secret"
	slackgo "github.com/slack-go/slack"
)

func liveSlackErrorCode(err error) string {
	if err == nil {
		return "ok"
	}
	var response slackgo.SlackErrorResponse
	if errors.As(err, &response) && response.Err != "" {
		return response.Err
	}
	return fmt.Sprintf("%T", err)
}

// liveProbeSink acknowledges only the probe's interactions and records no free text or credentials.
type liveProbeSink struct {
	mu      sync.Mutex
	presses []chat.Interaction
}

func (*liveProbeSink) Message(context.Context, chat.Inbound) error { return nil }
func (s *liveProbeSink) Interaction(_ context.Context, i chat.Interaction) error {
	if strings.HasPrefix(i.PromptID, "ix-phase5-probe-") {
		s.mu.Lock()
		s.presses = append(s.presses, i)
		s.mu.Unlock()
	}
	return nil
}

// TestLiveApprovalControls is an explicitly tagged manual probe. It posts no tool calls to any agent.
// Run only with owner authorization for the sandbox app and channel supplied by LIVE_SLACK_ENV.
func TestLiveApprovalControls(t *testing.T) {
	cfg := readLiveProbeConfig(t)
	channel := cfg.Channel
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	sink := &liveProbeSink{}
	ready := make(chan struct{}, 1)
	// Post and update go through the same Client as production. Socket Mode is used only to capture neutral control state.
	c := New(Config{BotToken: secret.New(cfg.Bot), AppToken: secret.New(cfg.App), OnConnection: func(up bool) {
		if up {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	}})
	cfg.Bot, cfg.App = "", ""
	p := NewPlatform(c, "probe/slack")
	stopped := make(chan error, 1)
	go func() { stopped <- p.Run(ctx, sink) }()
	drained := false
	defer func() {
		cancel()
		if !drained {
			<-stopped
		}
	}()
	select {
	case <-ready:
	case err := <-stopped:
		drained = true
		t.Fatalf("probe connection failed: %v", err)
	case <-ctx.Done():
		t.Fatal("probe connection timed out")
	}
	root, found, err := p.Find(ctx, chat.Target{Channel: channel}, "phase5-control-probe-root", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		root, err = p.Post(ctx, chat.Target{Channel: channel}, chat.Say(chat.Message{Fallback: "Agent Kourier approval-control check. These prompts do not run tools. Please verify literal text from a second account, then exercise Submit, Approve and Deny."}), "phase5-control-probe-root")
		if err != nil {
			t.Fatal(err)
		}
	}
	hostile := `{"literal":"<!channel> <@U000FAKE> <#C000FAKE> <https://example.invalid|link> ` + "```" + ` & < >","token":"[redacted]"}`
	prompts := []chat.Prompt{
		{ID: "ix-phase5-probe-question", Hint: "Ask-user Submit", Questions: []chat.Question{{Text: "Choose a test option", Choices: []string{"A", "B"}}}},
		{ID: "ix-phase5-probe-approve", Hint: "Single-tool Approve", Tools: []chat.Tool{{Name: "probe_<&>", Args: hostile}}},
		{ID: "ix-phase5-probe-deny", Hint: "Single-tool Deny", Tools: []chat.Tool{{Name: "probe_literal", Args: hostile}}},
		{ID: "ix-phase5-probe-submit", Hint: "Choose Approve/Deny for each tool, then Submit", Tools: []chat.Tool{{Name: "probe_first", Args: hostile}, {Name: "probe_second", Args: "(no arguments)"}}},
		{ID: "ix-phase5-probe-all-approve", Hint: "Approve all", Tools: []chat.Tool{{Name: "probe_first", Args: "{}"}, {Name: "probe_second", Args: "{}"}}},
		{ID: "ix-phase5-probe-all-deny", Hint: "Deny all", Tools: []chat.Tool{{Name: "probe_first", Args: "{}"}, {Name: "probe_second", Args: "{}"}}},
	}
	capPrompt := chat.Prompt{ID: "ix-phase5-probe-cap-deny", Hint: "20-tool limit: press Deny all"}
	for n := 1; n <= 20; n++ {
		capPrompt.Tools = append(capPrompt.Tools, chat.Tool{Name: fmt.Sprintf("probe_read_%02d", n), Args: `{"fixture":true}`})
	}
	prompts = append(prompts, capPrompt)
	refs := map[string]chat.Ref{}
	byID := map[string]chat.Prompt{}
	for _, pr := range prompts {
		pr.Deadline = time.Now().Add(9 * time.Minute)
		ref, err := p.Post(ctx, chat.Target{Channel: channel, Thread: root.ID}, chat.Ask(pr), pr.ID)
		if err != nil {
			t.Fatalf("post %s: %v", pr.ID, err)
		}
		refs[pr.ID], byID[pr.ID] = ref, pr
	}
	t.Logf("posted %d control prompts; inspect the authorized channel thread", len(prompts))
	seen := map[string]bool{}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for len(seen) < len(prompts) {
		select {
		case <-ctx.Done():
			t.Fatalf("observed %d/%d prompts before deadline", len(seen), len(prompts))
		case <-tick.C:
		}
		sink.mu.Lock()
		presses := sink.presses
		sink.presses = nil
		sink.mu.Unlock()
		for _, i := range presses {
			pr, ok := byID[i.PromptID]
			if !ok || seen[i.PromptID] || i.At.ID != refs[i.PromptID].ID {
				continue
			}
			verdicts := make([]chat.ToolVerdict, len(pr.Tools))
			if len(pr.Tools) > 0 {
				switch i.Action {
				case chat.ActionApproveAll:
					for n := range verdicts {
						verdicts[n] = chat.ToolApproved
					}
				case chat.ActionDenyAll:
					for n := range verdicts {
						verdicts[n] = chat.ToolDenied
					}
				case chat.ActionSubmit:
					for _, a := range i.Answers {
						if a.Question >= 0 && a.Question < len(verdicts) {
							verdicts[a.Question], _ = a.ToolVerdict()
						}
					}
				}
			}
			complete := len(pr.Tools) == 0 && i.Action == chat.ActionSubmit && len(i.Answers) == 1 && len(i.Answers[0].Values) == 1
			if len(pr.Tools) > 0 {
				complete = true
				for _, v := range verdicts {
					if v == "" {
						complete = false
					}
				}
			}
			t.Logf("%s: action=%s answered_controls=%d complete=%t", i.PromptID, i.Action, len(i.Answers), complete)
			expected := chat.ActionSubmit
			if strings.Contains(pr.ID, "approve") {
				expected = chat.ActionApproveAll
			}
			if strings.Contains(pr.ID, "deny") {
				expected = chat.ActionDenyAll
			}
			if i.Action != expected {
				complete = false
			}
			if !complete {
				continue
			}
			e := chat.Ending{Outcome: chat.Answered, By: i.User, At: time.Now(), Prompt: pr, Verdicts: verdicts}
			if len(pr.Questions) > 0 {
				if i.Answers[0].Values[0] == "0" {
					e.Answers = [][]string{{"A"}}
				} else if i.Answers[0].Values[0] == "1" {
					e.Answers = [][]string{{"B"}}
				} else {
					continue
				}
			}
			if err := p.Update(ctx, refs[pr.ID], chat.End(e)); err != nil {
				t.Fatal(err)
			}
			seen[pr.ID] = true
		}
	}
}

func liveTextObject(text string) map[string]any {
	return map[string]any{"type": "plain_text", "text": text}
}

func liveToolRichText(index int, args string) map[string]any {
	name := fmt.Sprintf("%d. phase5_size_%02d", index+1, index+1)
	return map[string]any{
		"type": "rich_text",
		"elements": []map[string]any{
			{"type": "rich_text_section", "elements": []map[string]any{{"type": "text", "text": name}}},
			{"type": "rich_text_preformatted", "elements": []map[string]any{{"type": "text", "text": args}}},
		},
	}
}

func liveRadioBlock(index int) map[string]any {
	name := fmt.Sprintf("%d. phase5_size_%02d", index+1, index+1)
	return map[string]any{
		"type":            "input",
		"block_id":        fmt.Sprintf("q%d", index),
		"dispatch_action": false,
		"optional":        false,
		"label":           liveTextObject(name),
		"element": map[string]any{
			"type":      "radio_buttons",
			"action_id": fmt.Sprintf("r%d", index),
			"options": []map[string]any{
				{"text": liveTextObject("Approve"), "value": "0"},
				{"text": liveTextObject("Deny"), "value": "1"},
			},
		},
	}
}

// liveApprovalContent is a valid approval layout built for the size probe. It intentionally bypasses
// renderToolPrompt's local caps so that Slack's own per-block and per-message decisions are observed through
// Client.Post and Client.Update. It contains no agent text other than the generated probe names and arguments.
func liveApprovalContent(promptID string, tools int, args string) (Content, int, error) {
	blocks := []map[string]any{{"type": "header", "text": liveTextObject("Phase 5 size probe")}}
	for i := 0; i < tools; i++ {
		blocks = append(blocks, liveToolRichText(i, args), liveRadioBlock(i))
	}
	blocks = append(blocks, map[string]any{
		"type":     "actions",
		"block_id": "submit",
		"elements": []map[string]any{
			{"type": "button", "action_id": "submit", "value": promptID, "style": "primary", "text": liveTextObject("Submit")},
			{"type": "button", "action_id": "approve_all", "value": promptID, "text": liveTextObject("Approve all")},
			{"type": "button", "action_id": "deny_all", "value": promptID, "style": "danger", "text": liveTextObject("Deny all")},
		},
	})
	raw, err := json.Marshal(blocks)
	if err != nil {
		return Content{}, 0, err
	}
	return Content{Text: "Phase 5 size probe " + promptID, Blocks: raw}, len(blocks), nil
}

func liveProbeCleanupContent() Content {
	raw, _ := json.Marshal([]map[string]any{{"type": "section", "text": liveTextObject("Phase 5 size probe complete")}})
	return Content{Text: "Phase 5 size probe complete", Blocks: raw}
}

func TestLiveSlackSizeProbe(t *testing.T) {
	cfg := readLiveProbeConfig(t)
	c := New(Config{BotToken: secret.New(cfg.Bot), AppToken: secret.New(cfg.App)})
	cfg.Bot, cfg.App = "", ""
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	cases := []struct {
		name  string
		tools int
		args  string
	}{
		{name: "ascii-20-tools", tools: 20, args: "x"},
		{name: "astral-20-tools", tools: 20, args: "😀"},
		{name: "ascii-24-tools", tools: 24, args: "x"},
		{name: "astral-24-tools", tools: 24, args: "😀"},
		{name: "ascii-per-block-3000", tools: 1, args: strings.Repeat("A", 3000)},
		{name: "ascii-per-block-3001", tools: 1, args: strings.Repeat("A", 3001)},
		{name: "astral-per-block-3000", tools: 1, args: strings.Repeat("😀", 1500)},
		{name: "astral-per-block-3002", tools: 1, args: strings.Repeat("😀", 1501)},
		{name: "ascii-total-20-4000-chars", tools: 20, args: strings.Repeat("A", 200)},
		{name: "ascii-total-20-4500-chars", tools: 20, args: strings.Repeat("A", 225)},
		{name: "ascii-total-20-5000-chars", tools: 20, args: strings.Repeat("A", 250)},
		{name: "ascii-total-20-5200-chars", tools: 20, args: strings.Repeat("A", 260)},
		{name: "ascii-total-20-5360-chars", tools: 20, args: strings.Repeat("A", 268)},
		{name: "astral-total-20-4000-chars", tools: 20, args: strings.Repeat("😀", 200)},
		{name: "astral-total-20-4500-chars", tools: 20, args: strings.Repeat("😀", 225)},
		{name: "astral-total-20-5000-chars", tools: 20, args: strings.Repeat("😀", 250)},
		{name: "astral-total-20-5200-chars", tools: 20, args: strings.Repeat("😀", 260)},
		{name: "astral-total-20-5360-chars", tools: 20, args: strings.Repeat("😀", 268)},
		{name: "ascii-total-24-2400-chars", tools: 24, args: strings.Repeat("A", 100)},
		{name: "ascii-total-24-2880-chars", tools: 24, args: strings.Repeat("A", 120)},
		{name: "ascii-total-24-3000-chars", tools: 24, args: strings.Repeat("A", 125)},
		{name: "ascii-total-24-3120-chars", tools: 24, args: strings.Repeat("A", 130)},
		{name: "ascii-total-24-3480-chars", tools: 24, args: strings.Repeat("A", 145)},
		{name: "ascii-total-24-3600-chars", tools: 24, args: strings.Repeat("A", 150)},
		{name: "ascii-total-24-4992-chars", tools: 24, args: strings.Repeat("A", 208)},
		{name: "astral-total-24-2400-chars", tools: 24, args: strings.Repeat("😀", 100)},
		{name: "astral-total-24-2880-chars", tools: 24, args: strings.Repeat("😀", 120)},
		{name: "astral-total-24-3000-chars", tools: 24, args: strings.Repeat("😀", 125)},
		{name: "astral-total-24-3120-chars", tools: 24, args: strings.Repeat("😀", 130)},
		{name: "astral-total-24-3480-chars", tools: 24, args: strings.Repeat("😀", 145)},
		{name: "astral-total-24-3600-chars", tools: 24, args: strings.Repeat("😀", 150)},
		{name: "astral-total-24-4992-chars", tools: 24, args: strings.Repeat("😀", 208)},
	}
	cleanup := liveProbeCleanupContent()
	for i, tc := range cases {
		promptID := fmt.Sprintf("phase5-size-%d-%02d", time.Now().Unix(), i)
		content, blocks, err := liveApprovalContent(promptID, tc.tools, tc.args)
		if err != nil {
			t.Fatalf("%s: build content: %v", tc.name, err)
		}
		units := chat.Units(tc.args)
		postStatus, updateStatus, cleanupStatus := "not-posted", "not-updated", "not-cleaned"
		ts, err := c.Post(ctx, cfg.Channel, "", content, promptID)
		postStatus = liveSlackErrorCode(err)
		if err == nil {
			updateErr := c.Update(ctx, cfg.Channel, ts, content)
			updateStatus = liveSlackErrorCode(updateErr)
			cleanupErr := c.Update(ctx, cfg.Channel, ts, cleanup)
			cleanupStatus = liveSlackErrorCode(cleanupErr)
		}
		t.Logf("size case=%s channel_code=%s tools=%d blocks=%d block_bytes=%d block_chars=%d arg_units_per_tool=%d total_arg_units=%d post=%s update=%s cleanup=%s", tc.name, liveChannelCode(cfg.Channel), tc.tools, blocks, len(content.Blocks), utf8.RuneCount(content.Blocks), units, tc.tools*units, postStatus, updateStatus, cleanupStatus)
	}
}
