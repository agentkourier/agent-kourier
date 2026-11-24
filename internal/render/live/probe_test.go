package live_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestHTTPProbeKnowsTheFooter holds hack/slack/http-probe.sh to the footer that ends a turn's final message: the probe
// takes a reply as the agent's answer only when it ends with the feedback buttons or the follow-up hint, so a change to
// either would fail every release probe with "no footer".
func TestHTTPProbeKnowsTheFooter(t *testing.T) {
	script, err := os.ReadFile("../../../hack/slack/http-probe.sh")
	if err != nil {
		t.Fatal(err)
	}
	value := func(name string) string {
		t.Helper()
		m := regexp.MustCompile(`(?m)^` + name + `='?([^'\n]*)'?$`).FindSubmatch(script)
		if m == nil {
			t.Fatalf("http-probe.sh sets no %s", name)
		}
		return string(m[1])
	}
	if got := value("FOLLOW_UP_HINT"); got != live.FollowUpHint {
		t.Errorf("http-probe.sh FOLLOW_UP_HINT = %q, want live.FollowUpHint %q", got, live.FollowUpHint)
	}
	prefix := value("FEEDBACK_ACTION")
	for _, kind := range []store.FeedbackKind{store.FeedbackInvestigation, store.FeedbackAnswer} {
		var buttons int
		for _, part := range feedback.Parts(kind, "1700000000.000001", "turn") {
			b, ok := part.(chat.Buttons)
			if !ok {
				continue
			}
			for _, button := range b.Buttons {
				buttons++
				if !strings.HasPrefix(button.ID, prefix) {
					t.Errorf("the %s feedback button %q does not start with http-probe.sh FEEDBACK_ACTION %q", kind, button.ID, prefix)
				}
			}
		}
		if buttons == 0 {
			t.Errorf("feedback.Parts(%s) has no buttons", kind)
		}
	}
}
