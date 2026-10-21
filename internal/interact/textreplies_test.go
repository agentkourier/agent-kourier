package interact_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

func TestTextRepliesRequireTheirCollaborators(t *testing.T) {
	r := newPressRig(t)
	ready := make(chan struct{})
	for _, tc := range []struct {
		name     string
		pipeline *interact.Pipeline
		pauses   interact.PauseHandler
		ready    <-chan struct{}
	}{
		{"Pipeline", nil, noPause{}, ready},
		{"Pauses", r.pl, nil, ready},
		{"Ready", r.pl, noPause{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if h, err := interact.NewTextReplies(tc.pipeline, tc.pauses, tc.ready); err == nil || h != nil {
				t.Fatalf("NewTextReplies = %v, %v", h, err)
			}
		})
	}
}

func TestTextRepliesWaitForStartupWithoutDeciding(t *testing.T) {
	r := newPressRig(t)
	task, reply, id := textQuestion(t, r)
	h, err := interact.NewTextReplies(r.pl, noPause{}, make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	outcome, err := h.Decide(ctx, key, reply, task)
	if outcome != session.TextReplyRetry || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("before startup = %v, %v", outcome, err)
	}
	row, err := r.st.GetInteraction(t.Context(), id)
	if err != nil || row.State != store.InteractionPending || len(r.sess.answers()) != 0 || len(r.aud.got) != 0 {
		t.Fatalf("startup decided the question: %+v, %v", row, err)
	}
}

type failingPause struct{ calls int }

func (p *failingPause) Paused(context.Context, interact.Pause) error { p.calls++; return errBoom }

func TestTextRepliesPrepareOnlyUnpinnedReplies(t *testing.T) {
	r := newPressRig(t)
	task, reply, id := textQuestion(t, r)
	ready := make(chan struct{})
	close(ready)
	pauses := &failingPause{}
	h, err := interact.NewTextReplies(r.pl, pauses, ready)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := h.Decide(t.Context(), key, reply, task)
	if outcome != session.TextReplyRetry || !errors.Is(err, errBoom) || pauses.calls != 1 || len(r.sess.answers()) != 0 {
		t.Fatalf("failed take-up = %v, %v; calls %d", outcome, err, pauses.calls)
	}
	reply.InteractionID = id
	outcome, err = h.Decide(t.Context(), key, reply, a2a.Task{})
	if outcome != session.TextReplyConsumed || err != nil || pauses.calls != 1 || len(r.sess.answers()) != 1 {
		t.Fatalf("pinned decision = %v, %v; calls %d", outcome, err, pauses.calls)
	}
}

func TestTextRepliesPostMissingPromptThenDecideOnce(t *testing.T) {
	r := newPressRig(t)
	task, reply, id := textQuestionWithPrompt(t, r, false)
	platform := &poster{posted: map[string]string{}}
	ob := newOutbox(t, outbox.Config{Store: r.st, Platform: platform, Now: func() time.Time { return r.now }})
	pauses, err := interact.New(intakeCfg(interact.Config{
		Store: r.st, AuditReader: r.st, Lookup: lookup{interact.Binding{Dialect: generic.New(time.Second), AskUser: true, Timeout: 10 * time.Minute}},
		Platform: platform, Outbox: ob, Session: r.sess, Audit: newWriter(t, r.st, audit.Options{}), Now: func() time.Time { return r.now },
	}))
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	h, err := interact.NewTextReplies(r.pl, pauses, ready)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := h.Decide(t.Context(), key, reply, task)
	if outcome != session.TextReplyConsumed || err != nil {
		t.Fatalf("decide = %v, %v", outcome, err)
	}
	row, err := r.st.GetInteraction(t.Context(), id)
	if err != nil || row.MessageRef == "" || row.State != store.InteractionAnswered || len(platform.posted) != 1 || len(r.sess.answers()) != 1 {
		t.Fatalf("prepared answer = %+v, %v; posts %d answers %d", row, err, len(platform.posted), len(r.sess.answers()))
	}
	claimed, err := r.st.NextReply(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = h.Decide(t.Context(), key, claimed, a2a.Task{})
	if outcome != session.TextReplyConsumed || err != nil || len(platform.posted) != 1 || len(r.sess.answers()) != 1 || len(r.aud.got) != 1 {
		t.Fatalf("replayed decision = %v, %v; posts %d answers %d audit %d", outcome, err, len(platform.posted), len(r.sess.answers()), len(r.aud.got))
	}
}

// Preparation owns refusing an unusable pause. Its reply stays queued as a turn, rather than
// entering an error-level text-answer retry loop after the task has already been cancelled.
func TestTextRepliesRefuseUnusablePauseBeforeLeavingTheReplyQueued(t *testing.T) {
	r := newPressRig(t)
	task, reply, _ := textQuestion(t, r)
	task.Status.Message = nil
	platform := &poster{posted: map[string]string{}}
	ob := newOutbox(t, outbox.Config{Store: r.st, Platform: platform, Now: func() time.Time { return r.now }})
	cancelled := &sess{}
	pauses, err := interact.New(intakeCfg(interact.Config{
		Store: r.st, AuditReader: r.st, Lookup: lookup{interact.Binding{Dialect: generic.New(time.Second), AskUser: true, Timeout: 10 * time.Minute}},
		Platform: platform, Outbox: ob, Session: cancelled, Audit: newWriter(t, r.st, audit.Options{}), Now: func() time.Time { return r.now },
	}))
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	h, err := interact.NewTextReplies(r.pl, pauses, ready)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := h.Decide(t.Context(), key, reply, task)
	if outcome != session.TextReplyNotAnswer || err != nil {
		t.Fatalf("unusable pause = %v, %v", outcome, err)
	}
	queued, err := r.st.NextReply(t.Context(), key)
	if err != nil || queued.InteractionID != "" || len(cancelled.cancels) != 1 || cancelled.cancels[0] != task.ID || len(r.sess.answers()) != 0 {
		t.Fatalf("queued reply = %+v, %v; cancellations %v", queued, err, cancelled.cancels)
	}
	rows, err := r.st.ListAudit(t.Context(), time.Time{}, 0, 100)
	if err != nil || len(rows) != 2 {
		t.Fatalf("audit rows = %d, %v; want refusal and cancellation", len(rows), err)
	}
	notices, err := r.st.ListUnposted(t.Context())
	if err != nil || len(notices) != 1 || notices[0].Role != store.OutboxNotice {
		t.Fatalf("notices = %+v, %v", notices, err)
	}
}
