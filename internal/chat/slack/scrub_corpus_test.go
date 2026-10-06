package slack_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
)

// The server's own words in a failed call are normalized and have the tokens of the corpus taken out, however an
// invisible character splits them, before the session manager logs the error. (A body is JSON: invalid UTF-8 cannot
// travel in it, so those cases of the corpus are left out.)
func TestSlackErrorTextTakesTheSplitSecretCorpus(t *testing.T) {
	cl, calls, _ := userClient(t, nil)
	calls.status = http.StatusOK
	for _, c := range redacttest.Identifiers(redacttest.SplitSecretsSample()) {
		if !utf8.ValidString(c.Text) {
			continue
		}
		body, err := json.Marshal(map[string]any{"ok": false, "error": c.Text})
		if err != nil {
			t.Fatal(err)
		}
		calls.body = string(body)
		_, got := cl.ResolveUser(t.Context(), "U1")
		if got == nil {
			t.Fatalf("%s: ResolveUser succeeded, want the server's error", c.Name)
		}
		c.CheckKept(t, got.Error())
	}
}
