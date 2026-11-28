package interact_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

type fakeStopper struct {
	mu      sync.Mutex
	calls   []stopCall
	stopped bool
	err     error
}

type stopCall struct {
	key    store.ConversationKey
	taskID a2a.TaskID
	by     string
}

func (f *fakeStopper) Stop(_ context.Context, key store.ConversationKey, taskID a2a.TaskID, by string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, stopCall{key: key, taskID: taskID, by: by})
	return f.stopped, f.err
}

func (f *fakeStopper) recorded() []stopCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]stopCall(nil), f.calls...)
}

type stopAuthz struct {
	members map[string]bool
	bots    map[string]bool
	err     error
}

func (a *stopAuthz) IsMember(_ context.Context, _, user string) (bool, error) {
	if a.err != nil {
		return false, a.err
	}
	return a.members[user], nil
}

func (a *stopAuthz) IsBot(_ context.Context, user string) (bool, error) {
	if a.err != nil {
		return true, a.err
	}
	return a.bots[user], nil
}

func (*stopAuthz) IsInAnyGroup(context.Context, []string, string) (bool, error) {
	return false, errors.New("stop does not check approver groups")
}

type stopEphemeral struct {
	mu   sync.Mutex
	sent []string
}

func (e *stopEphemeral) Ephemeral(_ context.Context, _ chat.Target, _ string, m chat.Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, m.Fallback)
	return nil
}

func (e *stopEphemeral) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.sent...)
}

type stopFixture struct {
	st       *sqlite.Store
	authz    *stopAuthz
	eph      *stopEphemeral
	stopper  *fakeStopper
	outcomes *interact.Outcomes
	notified chan store.Press
	handler  *interact.StopHandler
	key      store.ConversationKey
	now      time.Time
}

func newStopFixture(t *testing.T, limits store.PressLimits) *stopFixture {
	t.Helper()
	f := &stopFixture{
		st:       sqlitetest.Open(t),
		authz:    &stopAuthz{members: map[string]bool{"U1": true}, bots: map[string]bool{"B1": true}},
		eph:      &stopEphemeral{},
		stopper:  &fakeStopper{stopped: true},
		outcomes: interact.NewOutcomes(8),
		notified: make(chan store.Press, 16),
		key:      store.ConversationKey{Connection: "ns/slack", Channel: "C1", Thread: "100.1"},
		now:      time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}
	h, err := interact.NewStopHandler(interact.StopConfig{
		Store:       f.st,
		Sessions:    f.st,
		Authorizers: chattest.Same[interact.Authorizer](f.authz),
		Ephemerals:  chattest.Same[interact.Ephemeral](f.eph),
		Stopper:     f.stopper,
		Now:         func() time.Time { return f.now },
		Logger:      testutil.DiscardLogger(),
		Limits:      limits,
		Notify:      func(p store.Press) { f.notified <- p },
		Outcomes:    f.outcomes,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.handler = h
	return f
}

func (f *stopFixture) seedSession(t *testing.T, taskID string) {
	t.Helper()
	sess := store.Session{
		Key:          f.key,
		Binding:      "ns/sre",
		ContextID:    "ctx-1",
		TaskID:       taskID,
		Origin:       store.Origin{Source: "mention"},
		LastActivity: f.now,
	}
	if err := f.st.UpsertSession(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
}

func (f *stopFixture) stopPress(user, taskID, delivery string) chat.Interaction {
	return chat.Interaction{
		User:     user,
		At:       chat.Ref{Conn: f.key.Connection, Channel: f.key.Channel, Thread: f.key.Thread, ID: "100.2"},
		PromptID: render.StopValue{Thread: f.key.Thread, TaskID: taskID}.String(),
		Delivery: delivery,
	}
}

func TestStopHandlerRejectsNonStopButtons(t *testing.T) {
	f := newStopFixture(t, store.PressLimits{})
	for _, id := range []string{"ix-123", "fb1|investigation|useful|100.1|turn-1", "other"} {
		err := f.handler.Press(t.Context(), chat.Interaction{
			User: "U1", At: chat.Ref{Conn: "ns/slack", Channel: "C1", ID: "1.1"}, PromptID: id,
		})
		if !errors.Is(err, interact.ErrNotStop) {
			t.Errorf("Press(%q) = %v; want ErrNotStop", id, err)
		}
	}
}

func TestStopHandlerMemberStopsActiveTask(t *testing.T) {
	f := newStopFixture(t, store.PressLimits{})
	f.seedSession(t, "t1")

	if err := f.handler.Press(t.Context(), f.stopPress("U1", "t1", "d1")); err != nil {
		t.Fatal(err)
	}
	p := <-f.notified
	if !interact.IsStopPress(p) {
		t.Fatalf("IsStopPress(%q) = false; want true", p.InteractionID)
	}
	res, err := f.handler.Process(t.Context(), p)
	if err != nil || res != interact.Done {
		t.Fatalf("Process = %v, %v; want Done, nil", res, err)
	}
	calls := f.stopper.recorded()
	if len(calls) != 1 || calls[0] != (stopCall{key: f.key, taskID: "t1", by: "U1"}) {
		t.Errorf("Stop calls = %+v; want 1 call for t1 by U1", calls)
	}
	if got := f.eph.list(); len(got) != 1 || got[0] != "Stopping the task." {
		t.Errorf("ephemerals = %v; want [Stopping the task.]", got)
	}
	// Redelivery of d1 after processing is dropped and does not call Stop again.
	if err := f.handler.Press(t.Context(), f.stopPress("U1", "t1", "d1")); err != nil {
		t.Fatal(err)
	}
	select {
	case extra := <-f.notified:
		t.Fatalf("redelivered press notified: %+v", extra)
	default:
	}
}

func TestStopHandlerStaleOrEndedTaskSaysNoLongerRunning(t *testing.T) {
	for _, tc := range []struct {
		name        string
		seed        bool
		sessTask    string
		stopperDone bool
	}{
		{"no session", false, "", false},
		{"different task recorded", true, "t2", false},
		{"stopper reports already ended", true, "t1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStopFixture(t, store.PressLimits{})
			f.stopper.stopped = tc.stopperDone
			if tc.seed {
				f.seedSession(t, tc.sessTask)
			}
			if err := f.handler.Press(t.Context(), f.stopPress("U1", "t1", "d1")); err != nil {
				t.Fatal(err)
			}
			p := <-f.notified
			res, err := f.handler.Process(t.Context(), p)
			if err != nil || res != interact.Done {
				t.Fatalf("Process = %v, %v; want Done, nil", res, err)
			}
			if tc.sessTask != "t1" && len(f.stopper.recorded()) != 0 {
				t.Errorf("Stop called for stale task: %+v", f.stopper.recorded())
			}
			if got := f.eph.list(); len(got) != 1 || got[0] != "This task is no longer running." {
				t.Errorf("ephemerals = %v; want [This task is no longer running.]", got)
			}
		})
	}
}

func TestStopHandlerNonMemberAndBotDenied(t *testing.T) {
	t.Run("non-member", func(t *testing.T) {
		f := newStopFixture(t, store.PressLimits{})
		f.seedSession(t, "t1")
		done := make(chan chat.PressOutcome, 1)
		go func() {
			out, err := f.handler.DecidePress(t.Context(), f.stopPress("U_OUTSIDE", "t1", "d1"))
			if err != nil {
				t.Errorf("DecidePress = %v", err)
			}
			done <- out
		}()
		p := <-f.notified
		res, err := f.handler.Process(t.Context(), p)
		if err != nil || res != interact.Done {
			t.Fatalf("Process = %v, %v; want Done, nil", res, err)
		}
		out := <-done
		if !out.Decided || out.Text != "Only members of this channel can stop this task." {
			t.Errorf("inline outcome = %+v; want Decided with non-member text", out)
		}
		if len(f.stopper.recorded()) != 0 {
			t.Errorf("Stop called for non-member")
		}
		if got := f.eph.list(); len(got) != 0 {
			t.Errorf("ephemerals = %v; want none (Slack rejects ephemerals to non-members)", got)
		}
	})

	t.Run("bot", func(t *testing.T) {
		f := newStopFixture(t, store.PressLimits{})
		f.authz.members["B1"] = true
		f.seedSession(t, "t1")
		if err := f.handler.Press(t.Context(), f.stopPress("B1", "t1", "d2")); err != nil {
			t.Fatal(err)
		}
		p := <-f.notified
		res, err := f.handler.Process(t.Context(), p)
		if err != nil || res != interact.Done {
			t.Fatalf("Process = %v, %v; want Done, nil", res, err)
		}
		if len(f.stopper.recorded()) != 0 {
			t.Errorf("Stop called for bot")
		}
		if got := f.eph.list(); len(got) != 0 {
			t.Errorf("ephemerals = %v; want none for bot", got)
		}
	})
}

func TestStopHandlerPermanentErrorsFinishWithMessage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantText string
	}{
		{"unknown binding", fmt.Errorf("lookup: %w", session.ErrUnknownBinding), "This thread's agent is no longer set up, so the task cannot be stopped."},
		{"card too large", fmt.Errorf("cancel: %w", dialect.ErrCardTooLarge), "The agent's card is still too large to read, so the task could not be stopped. Fix the card and press Stop again."},
		{"credentials rejected", fmt.Errorf("cancel: %w", agent.ErrCredentialsRejected), "The agent refused the request to stop this task."},
		{"forbidden", fmt.Errorf("cancel: %w", agent.ErrForbidden), "The agent refused the request to stop this task."},
		{"agent forbidden", fmt.Errorf("cancel: %w", agent.ErrAgentForbidden), "The agent refused the request to stop this task."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStopFixture(t, store.PressLimits{})
			f.seedSession(t, "t1")
			f.stopper.stopped = false
			f.stopper.err = tc.err
			if err := f.handler.Press(t.Context(), f.stopPress("U1", "t1", "d1")); err != nil {
				t.Fatal(err)
			}
			p := <-f.notified
			res, err := f.handler.Process(t.Context(), p)
			if err != nil || res != interact.Done {
				t.Fatalf("Process = %v, %v; want Done, nil", res, err)
			}
			if got := f.eph.list(); len(got) != 1 || got[0] != tc.wantText {
				t.Errorf("ephemerals = %v; want [%q]", got, tc.wantText)
			}
		})
	}
}

func TestStopHandlerTransientErrorRetriesAndGiveUpTellsUser(t *testing.T) {
	f := newStopFixture(t, store.PressLimits{})
	f.seedSession(t, "t1")
	f.stopper.stopped = false
	f.stopper.err = errors.New("connection reset")

	if err := f.handler.Press(t.Context(), f.stopPress("U1", "t1", "d1")); err != nil {
		t.Fatal(err)
	}
	p := <-f.notified
	res, err := f.handler.Process(t.Context(), p)
	if err == nil || res != interact.Retry {
		t.Fatalf("Process = %v, %v; want Retry, non-nil error", res, err)
	}
	if len(f.eph.list()) != 0 {
		t.Errorf("transient retry should not send ephemeral yet: %v", f.eph.list())
	}
	if err := f.handler.GiveUp(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if got := f.eph.list(); len(got) != 1 || got[0] != "Couldn't stop the task. Please press Stop again." {
		t.Errorf("ephemerals after GiveUp = %v; want retry message", got)
	}
}

func TestStopHandlerEnforcesPressLimitsAndDecidePressInline(t *testing.T) {
	f := newStopFixture(t, store.PressLimits{Total: 10, PerUser: 1})
	f.seedSession(t, "t1")

	if err := f.handler.Press(t.Context(), f.stopPress("U1", "t1", "d1")); err != nil {
		t.Fatal(err)
	}
	if err := f.handler.Press(t.Context(), f.stopPress("U1", "t1", "d2")); !errors.Is(err, store.ErrLimit) {
		t.Fatalf("second press = %v; want store.ErrLimit", err)
	}
	p := <-f.notified
	if _, err := f.handler.Process(t.Context(), p); err != nil {
		t.Fatal(err)
	}

	// Inline DecidePress waits for worker outcome.
	done := make(chan chat.PressOutcome, 1)
	go func() {
		out, err := f.handler.DecidePress(t.Context(), f.stopPress("U1", "t1", "d3"))
		if err != nil {
			t.Errorf("DecidePress = %v", err)
		}
		done <- out
	}()
	p2 := <-f.notified
	if _, err := f.handler.Process(t.Context(), p2); err != nil {
		t.Fatal(err)
	}
	if out := <-done; !out.Decided || out.Text != "Stopping the task." {
		t.Errorf("DecidePress outcome = %+v; want Decided with Stopping the task.", out)
	}
}
