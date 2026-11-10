package k8ssecrets

import (
	"context"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/redact"
)

// The errors of Value are logged and shown in a Binding's status, which redact the value of anything that reads as a secret name followed
// by a colon. A key named botToken, or a Secret of the type kubernetes.io/service-account-token, is such a name, so no error puts a colon
// after one: the cause that follows it would be shown as [redacted].
func TestTheErrorsOfValueSurviveRedaction(t *testing.T) {
	empty := secret("sre", "empty")
	empty.Data["botToken"] = []byte{}
	account := secret("sre", "account")
	account.Type = "kubernetes.io/service-account-token"
	client := newAPI(t, empty, account)
	r := follow(t, client, sn("sre", "empty"), sn("sre", "account"))

	for _, tc := range []struct{ name, secret, key string }{
		{"a key the Secret lacks", "empty", "appToken"},
		{"a key with no value", "empty", "botToken"},
		{"a Secret of a type that is never read", "account", "token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Value(t.Context(), "sre", tc.secret, tc.key)
			if err == nil {
				t.Fatal("Value succeeded")
			}
			if got := redact.FreeText(err.Error()); got != err.Error() {
				t.Errorf("redaction changes the error\n got %q\nwant %q", got, err.Error())
			}
		})
	}

	t.Run("a Secret whose cache has not synced", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, err := unsynced(t).Value(ctx, "sre", "creds", "botToken")
		if err == nil {
			t.Fatal("Value succeeded")
		}
		if got := redact.FreeText(err.Error()); got != err.Error() {
			t.Errorf("redaction changes the error\n got %q\nwant %q", got, err.Error())
		}
	})
}
