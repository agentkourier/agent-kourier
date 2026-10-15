//go:build liveprobe

package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/secret"
	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// rawBlockAction is intentionally a narrow decoder. It has no fields for tokens, users, message text,
// response URLs or channel IDs, so a log can contain only the controls being probed.
type rawBlockAction struct {
	ActionID string `json:"action_id"`
	BlockID  string `json:"block_id"`
	Type     string `json:"type"`
	Value    string `json:"value"`
}

type rawSelection struct {
	Value string `json:"value"`
}

type rawStateInput struct {
	Type            string         `json:"type"`
	Value           string         `json:"value"`
	SelectedOption  rawSelection   `json:"selected_option"`
	SelectedOptions []rawSelection `json:"selected_options"`
}

type rawBlockActionPayload struct {
	Type      string           `json:"type"`
	Actions   []rawBlockAction `json:"actions"`
	Container struct {
		MessageTS string `json:"message_ts"`
	} `json:"container"`
	State struct {
		Values map[string]map[string]rawStateInput `json:"values"`
	} `json:"state"`
}

type safeRawInput struct {
	BlockID  string
	ActionID string
	Type     string
	Selected string
}

type safeRawBlockAction struct {
	PromptID string
	ActionID string
	BlockID  string
	Type     string
	Inputs   []safeRawInput
}

func safeControlID(value string) string {
	if value == "" || len(value) > 64 {
		return "<unexpected>"
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			if i == 0 && r >= '0' && r <= '9' {
				return "<unexpected>"
			}
			continue
		}
		return "<unexpected>"
	}
	return value
}

func safeSelection(input rawStateInput) string {
	value := input.SelectedOption.Value
	if value == "" && len(input.SelectedOptions) == 1 {
		value = input.SelectedOptions[0].Value
	}
	if value == "" && input.Value == "" {
		return "none"
	}
	if value == "0" || value == "1" {
		return value
	}
	return "other"
}

func captureSafeRawBlockAction(payload []byte, promptID, messageTS string) (safeRawBlockAction, bool) {
	var raw rawBlockActionPayload
	if json.Unmarshal(payload, &raw) != nil || raw.Type != string(slackgo.InteractionTypeBlockActions) || len(raw.Actions) == 0 {
		return safeRawBlockAction{}, false
	}
	action := raw.Actions[0]
	if action.Value != promptID || (messageTS != "" && raw.Container.MessageTS != messageTS) {
		return safeRawBlockAction{}, false
	}
	out := safeRawBlockAction{
		PromptID: promptID,
		ActionID: safeControlID(action.ActionID),
		BlockID:  safeControlID(action.BlockID),
		Type:     safeControlID(action.Type),
	}
	for blockID, actions := range raw.State.Values {
		for actionID, input := range actions {
			out.Inputs = append(out.Inputs, safeRawInput{
				BlockID:  safeControlID(blockID),
				ActionID: safeControlID(actionID),
				Type:     safeControlID(input.Type),
				Selected: safeSelection(input),
			})
		}
	}
	sort.Slice(out.Inputs, func(i, j int) bool {
		if out.Inputs[i].BlockID != out.Inputs[j].BlockID {
			return out.Inputs[i].BlockID < out.Inputs[j].BlockID
		}
		return out.Inputs[i].ActionID < out.Inputs[j].ActionID
	})
	return out, true
}

func TestLiveRawBlockActions(t *testing.T) {
	cfg := readLiveProbeConfig(t)
	client := New(Config{BotToken: secret.New(cfg.Bot), AppToken: secret.New(cfg.App)})
	cfg.Bot, cfg.App = "", ""
	promptID := fmt.Sprintf("phase5-raw-%d", time.Now().Unix())
	prompt := chat.Prompt{
		ID:   promptID,
		Hint: "Raw block_actions probe: leave both radios unset and press Submit",
		Tools: []chat.Tool{
			{Name: "phase5_raw_first", Args: `{"fixture":true}`},
			{Name: "phase5_raw_second", Args: `{"fixture":true}`},
		},
	}
	content, err := renderOutgoing(chat.Ask(prompt))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	socket := socketmode.New(client.api)
	connected := make(chan struct{})
	captured := make(chan safeRawBlockAction, 1)
	runDone := make(chan error, 1)
	go func() { runDone <- socket.RunContext(ctx) }()
	go func() {
		connectedOnce := false
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-socket.Events:
				switch event.Type {
				case socketmode.EventTypeConnected:
					if !connectedOnce {
						connectedOnce = true
						close(connected)
					}
				case socketmode.EventTypeInteractive:
					if event.Request == nil {
						continue
					}
					if safe, ok := captureSafeRawBlockAction(event.Request.Payload, promptID, ""); ok {
						// Ack only the fresh probe action that this test owns. Other envelopes are left for
						// the existing sandbox/broker connections rather than being consumed here.
						_ = socket.AckCtx(ctx, event.Request.EnvelopeID, nil)
						select {
						case captured <- safe:
						default:
						}
					}
				}
			}
		}
	}()
	select {
	case <-connected:
	case err := <-runDone:
		t.Fatalf("raw capture connection failed: %T", err)
	case <-ctx.Done():
		t.Fatal("raw capture connection timed out")
	}

	ts, err := client.Post(ctx, cfg.Channel, "", content, promptID)
	if err != nil {
		t.Fatalf("post raw capture prompt: %T", err)
	}
	t.Logf("posted fresh raw block_actions prompt id=%s channel_code=%s; leave both radios unset and press Submit", promptID, liveChannelCode(cfg.Channel))
	select {
	case safe := <-captured:
		parts := make([]string, len(safe.Inputs))
		for i, input := range safe.Inputs {
			parts[i] = input.BlockID + "/" + input.ActionID + "/" + input.Type + "/" + input.Selected
		}
		t.Logf("raw block_actions action=%s block=%s type=%s state_inputs=%d state=%s", safe.ActionID, safe.BlockID, safe.Type, len(safe.Inputs), strings.Join(parts, ","))
		cleanup, _ := json.Marshal([]map[string]any{{"type": "section", "text": map[string]any{"type": "plain_text", "text": "Raw block_actions probe complete"}}})
		if err := client.Update(ctx, cfg.Channel, ts, Content{Text: "Raw block_actions probe complete", Blocks: cleanup}); err != nil {
			t.Logf("raw block_actions cleanup=%T", err)
		}
	case <-ctx.Done():
		t.Fatal("no matching raw block_actions envelope observed before deadline")
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Log("raw capture connection did not stop within five seconds")
	}
}

// TestLiveRawApprovalButtons is an explicitly tagged manual probe. It posts three fresh two-tool prompts so
// the owner can exercise the two all-tools buttons and a Submit with one approval and one denial. The raw
// observer records only the whitelisted action and radio metadata; it does not consume other app events.
func TestLiveRawApprovalButtons(t *testing.T) {
	cfg := readLiveProbeConfig(t)
	client := New(Config{BotToken: secret.New(cfg.Bot), AppToken: secret.New(cfg.App)})
	cfg.Bot, cfg.App = "", ""
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	stamp := time.Now().UnixNano()
	type target struct {
		id, expected, hint string
	}
	targets := []target{
		{id: fmt.Sprintf("phase5-raw-selected-%d", stamp), expected: "submit", hint: "Raw selected Submit: choose Approve for the first tool, Deny for the second, then Submit"},
		{id: fmt.Sprintf("phase5-raw-approve-%d", stamp), expected: "approve_all", hint: "Raw Approve all: press Approve all"},
		{id: fmt.Sprintf("phase5-raw-deny-%d", stamp), expected: "deny_all", hint: "Raw Deny all: press Deny all"},
	}

	socket := socketmode.New(client.api)
	connected := make(chan struct{})
	captured := make(chan safeRawBlockAction, len(targets))
	runDone := make(chan error, 1)
	go func() { runDone <- socket.RunContext(ctx) }()
	go func() {
		connectedOnce := false
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-socket.Events:
				switch event.Type {
				case socketmode.EventTypeConnected:
					if !connectedOnce {
						connectedOnce = true
						close(connected)
					}
				case socketmode.EventTypeInteractive:
					if event.Request == nil {
						continue
					}
					for _, want := range targets {
						if safe, ok := captureSafeRawBlockAction(event.Request.Payload, want.id, ""); ok {
							// Ack only fresh probe actions. Unrelated envelopes remain available to the existing
							// sandbox/broker connections and never enter this test's logs.
							_ = socket.AckCtx(ctx, event.Request.EnvelopeID, nil)
							select {
							case captured <- safe:
							default:
							}
							break
						}
					}
				}
			}
		}
	}()
	select {
	case <-connected:
	case err := <-runDone:
		t.Fatalf("raw button capture connection failed: %T", err)
	case <-ctx.Done():
		t.Fatal("raw button capture connection timed out")
	}

	refs := make(map[string]string, len(targets))
	for _, want := range targets {
		prompt := chat.Prompt{
			ID:   want.id,
			Hint: want.hint,
			Tools: []chat.Tool{
				{Name: "phase5_raw_first", Args: `{"fixture":true}`},
				{Name: "phase5_raw_second", Args: `{"fixture":true}`},
			},
		}
		content, err := renderOutgoing(chat.Ask(prompt))
		if err != nil {
			t.Fatal(err)
		}
		ts, err := client.Post(ctx, cfg.Channel, "", content, want.id)
		if err != nil {
			t.Fatalf("post raw %s capture prompt: %T", want.expected, err)
		}
		refs[want.id] = ts
		t.Logf("posted fresh raw %s prompt id=%s channel_code=%s", want.expected, want.id, liveChannelCode(cfg.Channel))
	}

	got := make(map[string]safeRawBlockAction, len(targets))
	for len(got) < len(targets) {
		select {
		case safe := <-captured:
			if _, exists := got[safe.PromptID]; !exists {
				got[safe.PromptID] = safe
			}
		case <-ctx.Done():
			t.Fatalf("captured %d/%d raw approval button actions before deadline", len(got), len(targets))
		}
	}
	for _, want := range targets {
		safe := got[want.id]
		parts := make([]string, len(safe.Inputs))
		for i, input := range safe.Inputs {
			parts[i] = input.BlockID + "/" + input.ActionID + "/" + input.Type + "/" + input.Selected
		}
		t.Logf("raw approval action=%s prompt=%s block=%s type=%s state_inputs=%d state=%s", safe.ActionID, safe.PromptID, safe.BlockID, safe.Type, len(safe.Inputs), strings.Join(parts, ","))
		if safe.ActionID != want.expected || safe.BlockID != "submit" || safe.Type != "button" {
			t.Errorf("raw %s prompt action=%s block=%s type=%s, want action=%s block=submit type=button", want.expected, safe.ActionID, safe.BlockID, safe.Type, want.expected)
		}
		cleanup, _ := json.Marshal([]map[string]any{{"type": "section", "text": map[string]any{"type": "plain_text", "text": "Raw approval button probe complete"}}})
		if err := client.Update(ctx, cfg.Channel, refs[want.id], Content{Text: "Raw approval button probe complete", Blocks: cleanup}); err != nil {
			t.Logf("raw %s cleanup=%T", want.expected, err)
		}
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Log("raw button capture connection did not stop within five seconds")
	}
}
