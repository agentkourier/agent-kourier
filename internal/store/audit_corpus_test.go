package store

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
)

// The store is the last line behind the audit writer, and it redacts as identifier text: every field but Kind and
// Email comes back normalized, on one line, with the tokens of the corpus taken out.
func TestAuditEntryRedactedTakesTheSplitSecretCorpus(t *testing.T) {
	for _, c := range redacttest.Identifiers(redacttest.SplitSecretsSample()) {
		e := AuditEntry{Kind: AuditEvent, User: c.Text, Email: c.Text, Identity: c.Text, Binding: c.Text, Subject: c.Text, Outcome: c.Text}.Redacted()
		for _, v := range []string{e.User, e.Identity, e.Binding, e.Subject, e.Outcome} {
			c.CheckKept(t, v)
		}
		if e.Email != c.Text {
			t.Errorf("%s: Email = %q, want it as given: it is an identity field", c.Name, e.Email)
		}
	}
}
