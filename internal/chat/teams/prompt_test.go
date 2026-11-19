package teams

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	promptrender "github.com/agentkourier/agent-kourier/internal/render"
)

// A trigger's promptTemplate that reads .Channel and .Link renders on Teams: the channel ID and the permalink that the adapter builds
// pass the checks of a prompt's values, whose name shape has no "@".
func TestPermalinkPassesThePromptCheck(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.c.recv.recMu.Lock()
	ct.c.recv.recorded[testChannel] = Conversation{Channel: testChannel, AADGroupID: testGroup}
	ct.c.recv.recMu.Unlock()
	link, ok := ct.c.Permalink(ct.c.RefAt(testChannel, "1700000000001", "1700000000009"))
	if !ok {
		t.Fatal("no permalink")
	}
	p, err := promptrender.NewPrompt("teams", "Investigate {{ .Channel }} at {{ .Link }}")
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Render(v1alpha1.PromptView{Channel: testChannel, Link: link}, promptrender.TriggerMessage{Text: "boom"})
	if err != nil {
		t.Fatalf("Render() = %v; want the channel and the link to pass", err)
	}
	if want := "Investigate " + testChannel + " at " + link; !strings.HasPrefix(got, want) {
		t.Errorf("prompt = %q; want it to start with %q", got, want)
	}
}
