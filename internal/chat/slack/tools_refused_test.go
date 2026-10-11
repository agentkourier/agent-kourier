package slack_test

import (
	"errors"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// Until the layout of a tool approval exists, the adapter refuses a prompt of tools and its ending as invalid,
// and posts nothing, rather than showing a prompt with no content.
func TestAPromptOfToolsIsRefusedUntilItHasALayout(t *testing.T) {
	w := newWorld(t)
	prompt := chat.Prompt{ID: "ix-1", Tools: []chat.Tool{{Name: "kubectl", Args: "get pods"}}}
	ending := chat.Ending{Outcome: chat.Answered, Prompt: prompt, Verdicts: []chat.ToolVerdict{chat.ToolApproved}}
	if err := w.p.Fits(prompt); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("Fits of a prompt of tools = %v, want ErrInvalid", err)
	}
	before := len(w.fake.Thread("C1", w.target.Thread))
	for name, o := range map[string]chat.Outgoing{"prompt": chat.Ask(prompt), "ending": chat.End(ending)} {
		if _, err := w.p.Post(t.Context(), w.target, o, "tag-"+name); !errors.Is(err, chat.ErrInvalid) {
			t.Errorf("Post of a %s of tools = %v, want ErrInvalid", name, err)
		}
	}
	if after := len(w.fake.Thread("C1", w.target.Thread)); after != before {
		t.Errorf("the thread has %d messages, want the %d it had: nothing is posted", after, before)
	}
}
