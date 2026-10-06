package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/redact/redacttest"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// allRows is every row of s, paged by sequence (a page holds at most 1000).
func allRows(t *testing.T, s *sqlite.Store) []store.AuditEntry {
	t.Helper()
	var all []store.AuditEntry
	for after := int64(0); ; {
		page, err := s.ListAudit(context.Background(), time.Time{}, after, 1000)
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		if len(page) == 0 {
			return all
		}
		all = append(all, page...)
		after = page[len(page)-1].Sequence
	}
}

// An identifier field, whoever wrote it, is normalized and has the token layer of the corpus taken out: a secret an
// invisible character splits does not reach a row.
func TestAuditIdentifiersTakeTheSplitSecretCorpus(t *testing.T) {
	w, s := newWriter(t)
	cases := redacttest.Identifiers(redacttest.SplitSecretsSample())
	for _, c := range cases {
		a := Actor{User: c.Text, Email: "u1@example.com", Identity: c.Text, Binding: c.Text}
		if err := w.Event(context.Background(), a, SourceChat, c.Text, EventAccepted); err != nil {
			t.Fatalf("%s: Event: %v", c.Name, err)
		}
	}
	got := allRows(t, s)
	if len(got) != len(cases) {
		t.Fatalf("got %d rows, want %d", len(got), len(cases))
	}
	for i, e := range got {
		for _, v := range []string{e.User, e.Identity, e.Binding, e.Subject} {
			cases[i].Check(t, v)
		}
	}
}

// Free text, an unusable pause's reason, is normalized and redacted with every rule of free text, and put on one line.
// A flag, a user or a URL's password that a line break cuts from its value is taken too: the text is read joined.
func TestAuditFreeTextTakesTheSplitSecretCorpus(t *testing.T) {
	w, s := newWriter(t)
	cases := append(redacttest.SplitSecretsSample(), redacttest.LineSplits()...)
	for _, b := range redacttest.LineBreaks {
		cases = append(cases, redacttest.Breaks(b))
	}
	for _, c := range cases {
		cause := fmt.Errorf("%w: %s", dialect.ErrUnusablePause, c.Text)
		if err := w.UnusablePause(context.Background(), who, "t1", "c1", cause); err != nil {
			t.Fatalf("%s: UnusablePause: %v", c.Name, err)
		}
	}
	got := allRows(t, s)
	if len(got) != len(cases) {
		t.Fatalf("got %d rows, want %d", len(got), len(cases))
	}
	for i, e := range got {
		cases[i].Check(t, e.Outcome)
	}
	for i, b := range redacttest.LineBreaks {
		e := got[len(got)-len(redacttest.LineBreaks)+i]
		if want := "unusable_pause: " + dialect.ErrUnusablePause.Error() + ": first second token=[redacted]"; e.Outcome != want {
			t.Errorf("%s: outcome = %q, want %q", b.Name, e.Outcome, want)
		}
	}
}

// What the writer returns and logs when the store fails is free text too: the store's own words are quoted.
func TestAuditFailureTextTakesTheSplitSecretCorpus(t *testing.T) {
	for _, c := range redacttest.SplitSecretsSample() {
		var logs bytes.Buffer
		w := New(failing{err: errors.New("disk full " + c.Text)}, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
		err := w.Event(context.Background(), who, SourceAlert, "evt-1", EventAccepted)
		if err == nil {
			t.Fatalf("%s: Event = nil, want the failure", c.Name)
		}
		c.Check(t, err.Error())
		c.Check(t, logs.String())
	}
}

// What the audit path took before redaction learned to read lines (it joined the text, then redacted it) it takes now: a
// no-break space around an equals sign, a flag or a user whose value is on the next line, a URL's password in two lines.
func TestCleanFreeTakesWhatItTookBeforeItJoinedLines(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"password\u00a0=\u00a0hunter2", "password = [redacted]"},
		{"password\u202f:\u202fhunter2 tail", "password : [redacted] tail"},
		{"--password\nhunter2", "--password [redacted]"},
		{"-u\nadmin:hunter2", "-u admin:[redacted]"},
		{"https://user:pa\nss@host", "https://[redacted]@host"},
		{"deploy --password\r\nhunter2 now", "deploy --password [redacted] now"},
	} {
		if got := cleanFree(tc.in, 500); got != tc.want {
			t.Errorf("cleanFree(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
