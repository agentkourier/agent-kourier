//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

const (
	approvalMetric   = "agentkourier_approval_outcomes_total"
	approvalIdentity = "secret:agent-kourier/agent-kourier-agent-token"
)

func waitApprovalMetric(t *testing.T, s *scenario, metric string, labels map[string]string, before float64) {
	t.Helper()
	err := Wait(s.ctx, 30*time.Second, "the approval outcome metric", func(ctx context.Context) (bool, error) {
		value, err := s.leaderCounter(ctx, metric, labels)
		return value == before+1, err
	})
	if err != nil {
		t.Fatalf("%v (it was %v)", err, before)
	}
	if got := s.counter(metric, labels); got != before+1 {
		t.Errorf("%s%v went from %v to %v; want one increment", metric, labels, before, got)
	}
}

func approvalAuditAfter(entries, before []store.AuditEntry, binding string) []store.AuditEntry {
	var sequence int64
	for _, entry := range before {
		sequence = max(sequence, entry.Sequence)
	}
	rows := make([]store.AuditEntry, 0)
	for _, entry := range entries {
		if entry.Sequence > sequence && entry.Kind == store.AuditInteractionDecision && entry.Binding == binding {
			rows = append(rows, entry)
		}
	}
	return rows
}

func waitApprovalAudit(t *testing.T, s *scenario, before []store.AuditEntry, binding string) store.AuditEntry {
	t.Helper()
	var found store.AuditEntry
	err := Wait(s.ctx, 30*time.Second, "the persisted approval audit decision", func(ctx context.Context) (bool, error) {
		entries, err := s.leaderAuditEntries(ctx)
		if err != nil {
			return false, err
		}
		rows := approvalAuditAfter(entries, before, binding)
		if len(rows) == 0 {
			return false, nil
		}
		found = rows[0]
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// assertApprovalAudit checks the fields not already guaranteed by approvalAuditAfter's new-row,
// interaction-decision and Binding selection.
func assertApprovalAudit(t *testing.T, entry store.AuditEntry, user, outcome string) {
	t.Helper()
	if entry.User != user {
		t.Errorf("persisted approval audit actor = %q; want %q", entry.User, user)
	}
	if entry.Identity != approvalIdentity {
		t.Errorf("persisted approval audit identity = %q; want %q", entry.Identity, approvalIdentity)
	}
	if entry.Outcome != outcome {
		t.Errorf("persisted approval audit outcome = %q; want %q", entry.Outcome, outcome)
	}
	subject, ok := strings.CutPrefix(entry.Subject, "interaction=")
	interaction, task, hasTask := strings.Cut(subject, " task=")
	if !ok || !hasTask || interaction == "" || task == "" {
		t.Errorf("persisted approval audit subject = %q; want interaction and task identifiers", entry.Subject)
	}
}

func waitApprovalAudits(t *testing.T, s *scenario, before []store.AuditEntry, binding string, count int) []store.AuditEntry {
	t.Helper()
	var found []store.AuditEntry
	err := Wait(s.ctx, 30*time.Second, "the persisted approval audit decisions", func(ctx context.Context) (bool, error) {
		entries, err := s.leaderAuditEntries(ctx)
		if err != nil {
			return false, err
		}
		found = approvalAuditAfter(entries, before, binding)
		return len(found) == count, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}
