package render

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/agent"
)

// hostileText mentions, links, escapes and hides in every flavour at once.
const hostileText = "<!channel> [x](https://evil.example) & \x07 end"

func hostileEvents() []agent.TextEvent {
	return []agent.TextEvent{{ArtifactID: "a", Text: hostileText}}
}

func TestMarkdownBlockEscapesForMarkdown(t *testing.T) {
	block, ok := MarkdownBlock(hostileEvents())
	want := `&lt;!channel&gt; \[x\](https://evil.example) &amp;  end`
	if !ok || block["type"] != "markdown" || block["text"] != want {
		t.Fatalf("MarkdownBlock = %v, %v; want a markdown block of %q", block, ok, want)
	}
	if got := MarkdownText(hostileEvents()); got != want {
		t.Errorf("MarkdownText = %q; want the same text, %q", got, want)
	}
	if _, ok := MarkdownBlock([]agent.TextEvent{{ArtifactID: "a", Text: " \u200b\n"}}); ok {
		t.Error("a block of nothing but white space and zero-width characters")
	}
	long, _ := MarkdownBlock([]agent.TextEvent{{ArtifactID: "a", Text: strings.Repeat("x", MaxBlock+50)}})
	if text := long["text"].(string); utf16Len(text) > MaxBlock || !strings.HasSuffix(text, Truncated) {
		t.Errorf("a long text gives %d units; want at most %d, marked truncated", utf16Len(text), MaxBlock)
	}
}

func TestMessageEscapesEachPartForItsFlavour(t *testing.T) {
	tool := &agent.ToolCallEvent{Name: "[t](https://evil.example)", Args: map[string]any{"v": "<!channel> & [x](https://evil.example)"}}
	text, blocks := Message(hostileEvents(), tool)
	wantLine := `Calling [t](https://evil.example)(v="&lt;!channel&gt; &amp; [x](https://evil.example)")`
	if want := `&lt;!channel&gt; [x](https://evil.example) &amp;  end`; text != want {
		t.Errorf("text = %q; want %q (text flavour)", text, want)
	}
	if len(blocks) != 2 || blocks[0]["type"] != "markdown" || blocks[0]["text"] != `&lt;!channel&gt; \[x\](https://evil.example) &amp;  end` {
		t.Fatalf("blocks = %v; want the markdown block first", blocks)
	}
	els, _ := blocks[1]["elements"].([]map[string]any)
	if blocks[1]["type"] != "context" || len(els) != 1 || els[0]["type"] != "mrkdwn" || els[0]["text"] != wantLine {
		t.Errorf("tool block = %v; want one mrkdwn element saying %q", blocks[1], wantLine)
	}
}

func TestMessageWithOnlyAToolCallFallsBackToItsStatusLine(t *testing.T) {
	text, blocks := Message(nil, &agent.ToolCallEvent{Name: "[t](u)"})
	if text != "Calling [t](u)()" || len(blocks) != 1 || blocks[0]["type"] != "context" {
		t.Errorf("Message = %q, %v; want the status line as the text, in one context block", text, blocks)
	}
	if text, blocks := Message(nil, nil); text != "" || len(blocks) != 0 {
		t.Errorf("Message of nothing = %q, %v", text, blocks)
	}
}

func TestMessageTextIsCapped(t *testing.T) {
	long, _ := Message([]agent.TextEvent{{ArtifactID: "a", Text: strings.Repeat("x", 3000)}}, nil)
	if utf16Len(long) > MaxFallback || !strings.HasSuffix(long, Truncated) {
		t.Errorf("a long text is %d units; want at most %d, marked truncated", utf16Len(long), MaxFallback)
	}
}

func TestNoOutputBlockIsAMarkdownBlock(t *testing.T) {
	if b := NoOutputBlock(); b["type"] != "markdown" || b["text"] != NoOutput {
		t.Errorf("NoOutputBlock = %v; want a markdown block saying %q", b, NoOutput)
	}
}

func TestNoticeTextEscapesItsDetailOnly(t *testing.T) {
	if got, want := NoticeText("Stopped.", "<!channel> & [x](u)"), "Stopped. &lt;!channel&gt; &amp; [x](u)"; got != want {
		t.Errorf("NoticeText = %q; want %q", got, want)
	}
	if got := NoticeText("Stopped.", ""); got != "Stopped." {
		t.Errorf("NoticeText with no detail = %q", got)
	}
	if got := NoticeText("x", strings.Repeat("d", 500)); utf16Len(got) > 2+maxNoticeDetail || !strings.HasSuffix(got, Truncated) {
		t.Errorf("a long detail gives %d units; want it capped and marked", utf16Len(got))
	}
}
