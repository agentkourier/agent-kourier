package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

var (
	t0    = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	who   = Actor{User: "U1", Email: "u1@example.com", Identity: "sa/investigator", Binding: "ns/b"}
	fakes = []string{"xoxb-canary-0000-fake", "Bearer canary.fake.token", "eyJhbGciOiJub25lIn0.eyJjYW5hcnkiOjF9.sig"}
)

func newWriter(t *testing.T) (*Writer, *sqlite.Store) {
	t.Helper()
	s, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s, Options{Now: func() time.Time { return t0 }}), s
}

func rows(t *testing.T, s *sqlite.Store) []store.AuditEntry {
	t.Helper()
	got, err := s.ListAudit(context.Background(), time.Time{}, 0, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	return got
}

func only(t *testing.T, s *sqlite.Store) store.AuditEntry {
	t.Helper()
	got := rows(t, s)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got), got)
	}
	return got[0]
}

func TestEvent(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		src     Source
		outcome EventOutcome
		want    string
	}{
		{SourceAlert, EventAccepted, "alert: accepted"},
		{SourceAlert, EventRateLimited, "alert: rate_limited"},
		{SourceSlack, EventDenied, "slack_message: denied"},
	} {
		w, s := newWriter(t)
		if err := w.Event(ctx, who, c.src, "evt-1", c.outcome); err != nil {
			t.Fatalf("Event: %v", err)
		}
		want := store.AuditEntry{Sequence: 1, At: t0, Kind: store.AuditEvent, User: "U1", Email: "u1@example.com",
			Identity: "sa/investigator", Binding: "ns/b", Subject: "evt-1", Outcome: c.want}
		if got := only(t, s); got != want {
			t.Errorf("%s: row = %+v, want %+v", c.want, got, want)
		}
	}
	w, s := newWriter(t)
	noBinding := Actor{User: "U1"}
	if err := w.Event(ctx, noBinding, SourceAlert, "evt-1", EventAccepted); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("Event without a Binding = %v, want ErrInvalid", err)
	}
	if err := w.Event(ctx, who, SourceAlert, "evt-1", "bogus"); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("Event with an unknown outcome = %v, want ErrInvalid", err)
	}
	if got := rows(t, s); len(got) != 0 {
		t.Errorf("invalid events left rows: %+v", got)
	}
}

func TestAgentCall(t *testing.T) {
	notSent401 := fmt.Errorf("send: %w", errors.Join(agent.ErrNotSent, agent.ErrCredentialsRejected, &dialect.HTTPStatusError{Code: 401}))
	lost := &agent.StreamLostError{TaskID: "t-lost", ContextID: "c-lost", Err: errors.New("dropped")}
	for _, c := range []struct {
		name        string
		call        Call
		subject     string
		wantOutcome string
	}{
		{"ok", Call{Op: OpSend, TaskID: "t1", ContextID: "c1", MessageID: "m1"}, "task=t1 context=c1 message=m1", "send: ok"},
		{"credentials rejected (D34)", Call{Op: OpSend, MessageID: "m1", Err: notSent401}, "message=m1",
			"send: failed credentials_rejected,not_sent,http_401"},
		{"outcome unknown", Call{Op: OpSend, MessageID: "m1", Err: fmt.Errorf("x: %w", agent.ErrOutcomeUnknown)}, "message=m1", "send: failed outcome_unknown"},
		{"stream lost names its task", Call{Op: OpSend, MessageID: "m1", Err: fmt.Errorf("x: %w", lost)}, "task=t-lost context=c-lost message=m1", "send: failed stream_lost"},
		{"answer rejected", Call{Op: OpAnswer, TaskID: "t1", Err: agent.ErrAnswerRejected}, "task=t1", "answer: failed answer_rejected"},
		{"cancel busy", Call{Op: OpCancel, TaskID: "t1", Err: agent.ErrBusy}, "task=t1", "cancel: failed busy"},
		{"other error", Call{Op: OpCancel, TaskID: "t1", Err: errors.New("boom")}, "task=t1", "cancel: failed error"},
		{"find task absent", Call{Op: OpFindTask, ContextID: "c1", MessageID: "m1"}, "context=c1 message=m1", "find_task: absent"},
		{"token unreadable (D34, the turn fails)", Call{Op: OpSend, MessageID: "m1", Err: fmt.Errorf("%w: boom", errors.Join(agent.ErrNotSent, agent.ErrTokenUnreadable))}, "message=m1", "send: failed not_sent,token_unreadable"},
		{"header unreadable", Call{Op: OpSend, MessageID: "m1", Err: fmt.Errorf("x: %w", agent.ErrHeaderUnreadable)}, "message=m1", "send: failed header_unreadable"},
		{"token required (D34, never downgrade)", Call{Op: OpSend, MessageID: "m1", Err: fmt.Errorf("binding ns/b: %w", agent.ErrTokenRequired)}, "message=m1", "send: failed token_required"},
		{"pool closed", Call{Op: OpSend, MessageID: "m1", Err: agent.ErrPoolClosed}, "message=m1", "send: failed pool_closed"},
		{"streaming unsupported", Call{Op: OpSend, MessageID: "m1", Err: fmt.Errorf("agent ns/a: %w", agent.ErrStreamingUnsupported)}, "message=m1", "send: failed streaming_unsupported"},
		{"cancel not cancelable", Call{Op: OpCancel, TaskID: "t1", Err: a2a.ErrTaskNotCancelable}, "task=t1", "cancel: failed task_not_cancelable"},
		{"cancel task not found", Call{Op: OpCancel, TaskID: "t1", Err: a2a.ErrTaskNotFound}, "task=t1", "cancel: failed task_not_found"},
		{"answer rejected and its cancel failed (R17)", Call{Op: OpAnswer, TaskID: "t1", Err: errors.Join(agent.ErrAnswerRejected, a2a.ErrTaskNotCancelable)}, "task=t1",
			"answer: failed answer_rejected,task_not_cancelable"},
		{"answer rejected and its auto-cancel failed (ErrCancelFailed)", Call{Op: OpAnswer, TaskID: "t1", Err: errors.Join(agent.ErrAnswerRejected, fmt.Errorf("%w: down", agent.ErrCancelFailed))}, "task=t1",
			"answer: failed answer_rejected,cancel_failed"},
		{"find task found", Call{Op: OpFindTask, ContextID: "c1", MessageID: "m1", Holders: []a2a.TaskID{"ta"}}, "task=ta context=c1 message=m1", "find_task: found"},
		{"find task duplicate run", Call{Op: OpFindTask, ContextID: "c1", MessageID: "m1", Holders: []a2a.TaskID{"ta", "tb"}},
			"task=ta context=c1 message=m1", "find_task: duplicate_run holders=2 ids=ta,tb"},
		{"find task many short holders", Call{Op: OpFindTask, ContextID: "c1", MessageID: "m1", Holders: []a2a.TaskID{"t1", "t2", "t3", "t4", "t5", "t6", "t7"}},
			"task=t1 context=c1 message=m1", "find_task: duplicate_run holders=7 ids=t1,t2,t3,t4,t5,t6,t7"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, s := newWriter(t)
			if err := w.AgentCall(context.Background(), who, c.call); err != nil {
				t.Fatalf("AgentCall: %v", err)
			}
			got := only(t, s)
			if got.Kind != store.AuditAgentCall || got.Subject != c.subject || got.Outcome != c.wantOutcome ||
				got.User != "U1" || got.Email != "u1@example.com" || got.Identity != "sa/investigator" || got.Binding != "ns/b" {
				t.Errorf("row = %+v, want subject %q outcome %q with the actor", got, c.subject, c.wantOutcome)
			}
		})
	}
	w, _ := newWriter(t)
	if err := w.AgentCall(context.Background(), who, Call{Op: OpSend}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("a call naming no task, context or message = %v, want ErrInvalid", err)
	}
	if err := w.AgentCall(context.Background(), who, Call{Op: "send: ok Bearer SECRETA1", TaskID: "t1"}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("a call with a free-text Op = %v, want ErrInvalid", err)
	}
}

func TestDuplicateRunListsOnlyWholeIDsThatFit(t *testing.T) {
	w, s := newWriter(t)
	var holders []a2a.TaskID
	for i := range 5 {
		holders = append(holders, a2a.TaskID(fmt.Sprintf("%08d-aaaa-bbbb-cccc-dddddddddddd", i)))
	}
	if err := w.AgentCall(context.Background(), who, Call{Op: OpFindTask, ContextID: "c1", MessageID: "m1", Holders: holders}); err != nil {
		t.Fatalf("AgentCall: %v", err)
	}
	got := only(t, s).Outcome
	const prefix = "find_task: duplicate_run holders=5 ids="
	ids := strings.Split(strings.TrimPrefix(got, prefix), ",")
	if !strings.HasPrefix(got, prefix) || len(got) > store.MaxAuditOutcome || len(ids) != 4 || len(ids[3]) != 36 {
		t.Errorf("outcome (%d bytes) = %q, want holders=5 with the 4 whole UUIDs that fit and none cut", len(got), got)
	}
}

func TestDecision(t *testing.T) {
	decided := t0.Add(-time.Minute)
	for _, c := range []struct {
		name   string
		d      Decision
		wantAt time.Time
		want   string
	}{
		{"ask user", Decision{InteractionID: "i1", TaskID: "t1", Result: DecisionAnswered, At: decided,
			Answer: &dialect.Answer{Answers: [][]string{{"a"}, {"free text " + fakes[0]}}}}, decided, "answered questions=2"},
		{"tool approval", Decision{InteractionID: "i1", TaskID: "t1", Result: DecisionAnswered,
			Answer: &dialect.Answer{Decisions: []dialect.Decision{{ToolID: "x", Approved: true}, {ToolID: "y", Reason: "my reason " + fakes[1]}}}}, t0, "answered approved=1 denied=1"},
		{"incomplete", Decision{InteractionID: "i1", TaskID: "t1", Result: DecisionIncomplete}, t0, "incomplete"},
		{"timed out", Decision{InteractionID: "i1", TaskID: "t1", Result: DecisionTimedOut, At: decided}, decided, "timed_out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w, s := newWriter(t)
			if err := w.Decision(context.Background(), who, c.d); err != nil {
				t.Fatalf("Decision: %v", err)
			}
			got := only(t, s)
			if got.Kind != store.AuditInteractionDecision || got.Subject != "interaction=i1 task=t1" || got.Outcome != c.want ||
				!got.At.Equal(c.wantAt) || got.User != "U1" || got.Binding != "ns/b" {
				t.Errorf("row = %+v, want outcome %q at %v", got, c.want, c.wantAt)
			}
			if strings.Contains(got.Outcome, "free text") || strings.Contains(got.Outcome, "my reason") {
				t.Errorf("outcome %q holds the person's text", got.Outcome)
			}
		})
	}
	w, _ := newWriter(t)
	if err := w.Decision(context.Background(), who, Decision{InteractionID: "i1", Result: "bogus"}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("Decision with an unknown result = %v, want ErrInvalid", err)
	}
}

func TestUnusablePause(t *testing.T) {
	w, s := newWriter(t)
	ctx := context.Background()
	cause := fmt.Errorf("parse: %w: no request id", dialect.ErrUnusablePause)
	if err := w.UnusablePause(ctx, who, "t1", "c1", cause); err != nil {
		t.Fatalf("UnusablePause: %v", err)
	}
	got := only(t, s)
	if got.Kind != store.AuditAgentCall || got.Subject != "task=t1 context=c1" || got.User != "U1" || got.Binding != "ns/b" ||
		got.Outcome != "unusable_pause: parse: unusable pause: no request id" {
		t.Errorf("row = %+v, want the pause with its reason", got)
	}
	for _, c := range []struct {
		err  error
		want string
	}{{nil, "unusable_pause_cancel: ok"}, {errors.New("boom"), "unusable_pause_cancel: failed error"},
		{fmt.Errorf("x: %w", errors.Join(agent.ErrNotSent, agent.ErrCredentialsRejected)), "unusable_pause_cancel: failed credentials_rejected,not_sent"}} {
		if err := w.UnusablePauseCancel(ctx, who, "t1", "c1", c.err); err != nil {
			t.Fatalf("UnusablePauseCancel: %v", err)
		}
		last := rows(t, s)
		if last := last[len(last)-1]; last.Outcome != c.want || last.Subject != "task=t1 context=c1" || last.Kind != store.AuditAgentCall {
			t.Errorf("last row = %+v, want %q on the pause's subject", last, c.want)
		}
	}
	if err := w.UnusablePause(ctx, who, "", "", cause); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("UnusablePause naming no task or context = %v, want ErrInvalid", err)
	}
}

// bearerVariants hide a token behind a separator that a naive strip would glue shut or
// that Go's ASCII-only \s would not match.
var bearerVariants = []string{
	"Bearer\tSECRETA1", "Authorization: Bearer\nSECRETA2", "Bearer\u200bSECRETA3", "Bearer\u00a0SECRETA4", "Bearer\u2003SECRETA5",
	"Bearer: SECRETA8", "Bearer:SECRETA9", "Bearer\u200b:\u00a0SECRETA0", "Bearer\u00a0 SECRETA6", "Bearer\u200b SECRETA7", // each needs its own layer: the space map, the format-character delete
}

func TestNoSecretsInStoredRows(t *testing.T) {
	ctx := context.Background()
	for _, canary := range append([]string{strings.Join(fakes, " ")}, bearerVariants...) {
		w, s := newWriter(t)
		leaky := Actor{User: canary, Email: canary, Identity: canary, Binding: canary}
		_ = w.Event(ctx, leaky, SourceSlack, canary, EventAccepted)
		_ = w.AgentCall(ctx, leaky, Call{Op: OpSend, TaskID: a2a.TaskID(canary), ContextID: canary, MessageID: canary,
			Err: errors.Join(agent.ErrNotSent, errors.New("Authorization: canary-plain-header-value")), Holders: []a2a.TaskID{a2a.TaskID(canary), "b"}})
		_ = w.Decision(ctx, leaky, Decision{InteractionID: canary, TaskID: a2a.TaskID(canary), Result: DecisionAnswered,
			Answer: &dialect.Answer{Answers: [][]string{{canary}}, Decisions: []dialect.Decision{{Reason: canary}}}})
		_ = w.UnusablePause(ctx, leaky, a2a.TaskID(canary), canary, fmt.Errorf("%w: %s", dialect.ErrUnusablePause, canary))
		_ = w.UnusablePauseCancel(ctx, leaky, a2a.TaskID(canary), canary, errors.New(canary))
		got := rows(t, s)
		if len(got) != 5 {
			t.Fatalf("%q: got %d rows, want 5", canary, len(got))
		}
		for _, e := range got {
			all := fmt.Sprintf("%+v", e)
			for _, secret := range append(fakes, "canary.fake.token", "canary-plain-header-value", "SECRETA") {
				if strings.Contains(all, secret) {
					t.Errorf("%q: %s row holds %q: %+v", canary, e.Kind, secret, e)
				}
			}
		}
	}
}

func TestAgentTextIsCapped(t *testing.T) {
	w, s := newWriter(t)
	long := strings.Repeat("é", 500)
	cause := fmt.Errorf("%w: line one\nline\x00 two %s", dialect.ErrUnusablePause, long)
	if err := w.UnusablePause(context.Background(), who, a2a.TaskID(long), "c1", cause); err != nil {
		t.Fatalf("UnusablePause: %v", err)
	}
	got := only(t, s)
	if len(got.Outcome) > store.MaxAuditOutcome || len(got.Subject) > 2*maxField || !strings.HasSuffix(got.Outcome, "...") ||
		strings.ContainsAny(got.Outcome, "\n\x00") {
		t.Errorf("outcome (%d bytes) = %q, want it capped, single-line and marked truncated", len(got.Outcome), got.Outcome)
	}
}

type failing struct {
	store.Audit
	err error
}

func (f failing) AppendAudit(context.Context, store.AuditEntry) error { return f.err }

func TestWriteFailureIsReturnedLoggedAndMetered(t *testing.T) {
	for _, secret := range append([]string{fakes[0]}, bearerVariants...) {
		var logs bytes.Buffer
		var metered []store.AuditKind
		w := New(failing{err: errors.New("disk full " + secret)}, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil)),
			OnFailure: func(k store.AuditKind) { metered = append(metered, k) }})
		err := w.Event(context.Background(), who, SourceAlert, "evt-1", EventAccepted)
		if err == nil || !strings.Contains(err.Error(), "disk full") || strings.Contains(err.Error(), "SECRETA") || strings.Contains(err.Error(), fakes[0]) {
			t.Fatalf("%q: Event = %v, want the store's error wrapped, with no secret", secret, err)
		}
		if len(metered) != 1 || metered[0] != store.AuditEvent {
			t.Errorf("metered = %v, want one event failure", metered)
		}
		out := logs.String()
		if !strings.Contains(out, "audit write failed") || strings.Contains(out, fakes[0]) || strings.Contains(out, "SECRETA") || strings.Contains(out, "evt-1") {
			t.Errorf("%q: log = %q, want a failure line with neither the entry nor a secret", secret, out)
		}
	}
}

func TestReturnedErrorsAreCleanAndStillUnwrap(t *testing.T) {
	w, _ := newWriter(t)
	ctx := context.Background()
	// %q would turn an invisible rune into an ASCII escape before clean could delete it.
	for _, leaky := range append([]string{"Bearer SECRETA1", "xo\u200bxb-1234-SECRETA2", "eyJhbGci\u200bOiJub25lIn0.eyJjYW5hcnkiOjF9.SECRETA3", "Bearer\u00a0 SECRETA4"}, bearerVariants...) {
		for name, err := range map[string]error{
			"event outcome":   w.Event(ctx, who, SourceAlert, leaky, "bogus"),
			"event binding":   w.Event(ctx, Actor{}, SourceAlert, leaky, EventAccepted),
			"pause subject":   w.UnusablePause(ctx, who, "", "", errors.New(leaky)),
			"pause cause nil": w.UnusablePause(ctx, who, "t1", "", nil),
			"call op":         w.AgentCall(ctx, who, Call{Op: Op(leaky), TaskID: "t1"}),
			"call subject":    w.AgentCall(ctx, who, Call{Op: OpSend, Err: errors.New(leaky)}),
			"decision result": w.Decision(ctx, who, Decision{InteractionID: leaky, Result: DecisionResult(leaky)}),
		} {
			if !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%q, %s: err = %v, want ErrInvalid", leaky, name, err)
			}
			for _, secret := range []string{"SECRETA", "xoxb-", "eyJ"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%q, %s: err = %q holds %q", leaky, name, err, secret)
				}
			}
		}
	}
}

func TestIdentifiersNamingBearerAreKept(t *testing.T) {
	w, s := newWriter(t)
	a := Actor{User: "U1", Identity: "sa/bearerauth", Binding: "ns/bearer-gateway"}
	if err := w.Event(context.Background(), a, SourceAlert, "bearer-alert", EventAccepted); err != nil {
		t.Fatalf("Event: %v", err)
	}
	if got := only(t, s); got.Identity != a.Identity || got.Binding != a.Binding || got.Subject != "bearer-alert" {
		t.Errorf("row = %+v, want identifiers that merely start with bearer left alone", got)
	}
}

func TestWriteSurvivesCancelledContext(t *testing.T) {
	w, s := newWriter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a request cancelled mid-flight still needs its record
	if err := w.Event(ctx, who, SourceAlert, "evt-1", EventAccepted); err != nil {
		t.Fatalf("Event on a cancelled context: %v", err)
	}
	if got := only(t, s); got.Outcome != "alert: accepted" {
		t.Errorf("row = %+v", got)
	}
}
