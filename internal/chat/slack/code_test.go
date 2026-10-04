package slack_test

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// The answers of the alert trial of 2026-10-04, streamed in pieces, arrive in the thread as a document whose code can
// be copied: no "\[", no "&lt;", and the fence a model indented in a list item is a block at column 0.
func TestStreamedCodeOfTheTrialAnswersArrivesCopyable(t *testing.T) {
	c, fake, _, root := updateClient(t)
	st, err := slack.NewPlatform(c, "t/slack").Stream(t.Context(), chat.Target{Channel: "C1", Thread: root}, "U1", "1. *Look* at it, then:\n")
	if err != nil {
		t.Fatal(err)
	}
	const patch = `kubectl patch deployment search -n shop --type='json' -p='[{"op":"replace","path":"/spec/template/spec/containers/0/readinessProbe/httpGet/path","value":"/"}]'`
	for _, md := range []string{
		"2. *Update the Deployment* if the code is legitimate:\n   ```ba",
		"sh\n   kubectl edit deployment recommender -n shop\n   ``",
		"`\n3. Patch the probe:\n```\n" + patch + "\n```\nThen `kubectl logs <pod> -n shop 2>&1` and `model = []`.",
	} {
		if err := st.Append(t.Context(), md); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Stop(t.Context(), "", chat.Message{}, "ob-1"); err != nil {
		t.Fatal(err)
	}
	want := "1. *Look* at it, then:\n" +
		"2. *Update the Deployment* if the code is legitimate:\n```\nkubectl edit deployment recommender -n shop\n```\n" +
		"3. Patch the probe:\n```\n" + patch + "\n```\n" +
		"Then `kubectl logs <pod> -n shop 2>&1` and `model = []`."
	th := fake.Thread("C1", root)
	if got := th[len(th)-1].Text; got != want {
		t.Fatalf("the thread shows\n%q\nwant\n%q", got, want)
	}
}
