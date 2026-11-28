package interact_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

var testMentions = []v1alpha1.ChatMention{
	{Name: "sam", User: "U0SAM"},
	{Name: "lee", User: "U0LEE"},
}

type mentionRig struct {
	*rig
	notified []interact.QuestionMentions
	// postedAtNotify records how many prompts were already delivered when NotifyQuestionMentions ran.
	postedAtNotify []int
}

func newMentionRig(t *testing.T, mod func(*interact.Binding)) *mentionRig {
	t.Helper()
	r := newRig(t, nil)
	m := &mentionRig{rig: r}
	b := interact.Binding{
		Dialect:     kagentv1.New(30 * time.Second),
		AskUser:     true,
		Timeout:     timeout,
		Mentions:    testMentions,
		MaxMentions: 2,
	}
	if mod != nil {
		mod(&b)
	}
	var err error
	r.in, err = interact.New(intakeCfg(interact.Config{
		Store:             r.st,
		AuditReader:       r.st,
		Lookup:            lookup{b},
		Platforms:         chattest.Same[interact.Fitter](fitter{r}),
		Outbox:            r.outbox,
		Session:           r.sess,
		Audit:             r.audit,
		OnApprovalOutcome: func(_ string, o interact.Outcome) { r.hook = append(r.hook, o) },
		NotifyQuestionMentions: func(_ context.Context, req interact.QuestionMentions) {
			m.notified = append(m.notified, req)
			m.postedAtNotify = append(m.postedAtNotify, len(r.slack.posted))
		},
		Now: func() time.Time { return t0.Add(time.Hour) },
	}))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDeliveredQuestionPromptNotifiesMentionsAfterPost(t *testing.T) {
	t.Run("kagent multi-question prompt", func(t *testing.T) {
		m := newMentionRig(t, nil)
		task := pausedOn(map[string]any{
			"type": "ask_user_request",
			"id":   "req-q1",
			"questions": []map[string]any{
				{"question": "@sam, may I restart the pod?"},
				{"question": "@lee, which cluster?", "choices": []string{"us-east", "eu-west"}},
			},
		})
		if err := m.in.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Turn: "turn-1", Task: task}); err != nil {
			t.Fatal(err)
		}
		if len(m.notified) != 1 {
			t.Fatalf("notified %d times, want 1", len(m.notified))
		}
		if m.postedAtNotify[0] != 1 {
			t.Errorf("postedAtNotify = %d, want 1 (prompt delivered before notifying)", m.postedAtNotify[0])
		}
		want := interact.QuestionMentions{
			Turn:        "turn-1",
			Binding:     "ns/b",
			Key:         key,
			Mentions:    testMentions,
			MaxMentions: 2,
			Questions:   []string{"@sam, may I restart the pod?", "@lee, which cluster?"},
		}
		if !reflect.DeepEqual(m.notified[0], want) {
			t.Errorf("notified = %+v, want %+v", m.notified[0], want)
		}
		// Handing the same pause off again after a restart does not notify again.
		if err := m.in.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Turn: "turn-1", Task: task, Recovered: true}); err != nil {
			t.Fatal(err)
		}
		if len(m.notified) != 1 {
			t.Errorf("notified %d times after recovery, want 1", len(m.notified))
		}
	})

	t.Run("generic a2a text question", func(t *testing.T) {
		m := newMentionRig(t, func(b *interact.Binding) {
			b.Dialect = generic.New(30 * time.Second)
		})
		msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("@sam, may I restart the pod?"))
		msg.ID = "msg-1"
		task := a2a.Task{ID: "task-1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Timestamp: &t0, Message: msg}}
		if err := m.in.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Turn: "turn-2", Task: task}); err != nil {
			t.Fatal(err)
		}
		if len(m.notified) != 1 || !reflect.DeepEqual(m.notified[0].Questions, []string{"@sam, may I restart the pod?"}) || m.notified[0].Turn != "turn-2" {
			t.Errorf("notified = %+v; want one generic question for turn-2", m.notified)
		}
	})
}

func TestRefusedAndNonQuestionPausesNotifyNobody(t *testing.T) {
	question := pausedOn(map[string]any{
		"type":      "ask_user_request",
		"id":        "req-1",
		"questions": []map[string]any{{"question": "@sam, confirm?"}},
	})
	approval := pausedOn(map[string]any{
		"type":  "tool_approval_request",
		"id":    "req-2",
		"tools": []map[string]any{{"id": "t1", "name": "restart", "args": map[string]any{"note": "@sam"}}},
	})
	for name, tc := range map[string]struct {
		task      a2a.Task
		turn      string
		mod       func(*interact.Binding)
		promptErr error
		fault     string
		wantErr   bool
	}{
		"askUser false":          {task: question, turn: "turn-1", mod: func(b *interact.Binding) { b.AskUser = false }},
		"prompt too large":       {task: question, turn: "turn-1", promptErr: chat.ErrTooLarge},
		"prompt invalid":         {task: question, turn: "turn-1", promptErr: chat.ErrInvalid},
		"outbox send fails":      {task: question, turn: "turn-1", fault: "before", wantErr: true},
		"unusable pause":         {task: pausedOn(map[string]any{"type": "nonsense"}), turn: "turn-1"},
		"tool approval":          {task: approval, turn: "turn-1", mod: func(b *interact.Binding) { b.ToolApprovals = true }},
		"no turn source":         {task: question, turn: ""},
		"no mentions on Binding": {task: question, turn: "turn-1", mod: func(b *interact.Binding) { b.Mentions = nil }},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMentionRig(t, tc.mod)
			m.promptErr = tc.promptErr
			m.slack.fault = tc.fault
			err := m.in.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Turn: tc.turn, Task: tc.task})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Paused = %v, wantErr = %v", err, tc.wantErr)
			}
			if len(m.notified) != 0 {
				t.Errorf("notified = %+v, want none", m.notified)
			}
		})
	}
}

func TestQuestionMentionsRunOnceAfterSetRefRetry(t *testing.T) {
	m := newMentionRig(t, nil)
	task := pausedOn(map[string]any{
		"type":      "ask_user_request",
		"id":        "req-retry",
		"questions": []map[string]any{{"question": "@sam, confirm?"}},
	})
	m.st.failRef = true
	if err := m.in.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Turn: "turn-1", Task: task}); err == nil {
		t.Fatal("first Paused succeeded despite SetInteractionMessageRef failure")
	}
	if len(m.notified) != 0 {
		t.Fatalf("notified before SetInteractionMessageRef succeeded: %+v", m.notified)
	}
	if err := m.in.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Turn: "turn-1", Task: task}); err != nil {
		t.Fatal(err)
	}
	if len(m.notified) != 1 || m.notified[0].Turn != "turn-1" {
		t.Errorf("notified = %+v, want once on retry", m.notified)
	}
	if len(m.slack.posted) != 1 {
		t.Errorf("posted = %v, want 1 prompt", m.slack.posted)
	}
	if _, err := m.st.GetOutbox(t.Context(), outbox.ID(m.row(t, task).ID, store.OutboxReply)); err != nil {
		t.Errorf("prompt outbox row: %v", err)
	}
}

func TestTapPassesTurnSourceToPause(t *testing.T) {
	r := newTap(t, nil)
	h := r.tap.Turn(t.Context(), tapRef)
	h(agent.FinalEvent{Task: &a2a.Task{ID: "task-1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Timestamp: &d0, Message: &a2a.Message{ID: "q1"}}}})
	testutil.Eventually(t, "pause handed off", func() bool { return r.pauses.calls() == 1 })
	r.pauses.mu.Lock()
	got := r.pauses.got[0]
	r.pauses.mu.Unlock()
	if got.Turn != tapRef.MessageID {
		t.Errorf("Pause.Turn = %q, want %q", got.Turn, tapRef.MessageID)
	}
}
