package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

var (
	t0      = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	key     = store.ConversationKey{Connection: "slack/main", Channel: "C1", Thread: "1.1"}
	errBoom = errors.New("boom")
	timeout = 10 * time.Minute
)

func pausedOn(payload map[string]any) a2a.Task {
	return a2a.Task{ID: "task-1", ContextID: "ctx-1", Status: a2a.TaskStatus{
		State: a2a.TaskStateInputRequired, Timestamp: &t0, Message: &a2a.Message{Metadata: map[string]any{kagentv1.HITLExtension: payload}},
	}}
}

func askUser(id string) a2a.Task {
	return pausedOn(map[string]any{"type": "ask_user_request", "id": id, "questions": []map[string]any{{"question": "Which?", "choices": []string{"a", "b"}}}})
}

// poster is the outbox's platform, keeping posts by outbox ID. fault fails the next post "before" it
// happens, or reports an error "after" it did, as a crash would.
type poster struct {
	posted   map[string]string
	contents map[string]chat.Outgoing // what each post held, by outbox ID
	fault    string
}

func (p *poster) Post(_ context.Context, t chat.Target, c chat.Outgoing, id string) (chat.Ref, error) {
	f := p.fault
	p.fault = ""
	if f == "before" {
		return chat.Ref{}, errBoom
	}
	p.posted[id] = fmt.Sprintf("100.%03d", len(p.posted)+1)
	if p.contents == nil {
		p.contents = map[string]chat.Outgoing{}
	}
	p.contents[id] = c
	if f == "after" {
		return chat.Ref{}, errBoom
	}
	return p.RefAt(t.Channel, t.Thread, p.posted[id]), nil
}

func (p *poster) Find(_ context.Context, t chat.Target, id string, _ time.Time) (chat.Ref, bool, error) {
	ts, ok := p.posted[id]
	return p.RefAt(t.Channel, t.Thread, ts), ok, nil
}

func (*poster) React(context.Context, chat.Ref, string) error {
	return errors.New("this test makes no reactions")
}

func (*poster) Unreact(context.Context, chat.Ref, string) error {
	return errors.New("this test makes no reactions")
}

func (*poster) RefAt(channel, thread, id string) chat.Ref {
	return chat.Ref{Platform: "fake", Channel: channel, Thread: thread, ID: id}
}

func (*poster) Fits(chat.Prompt) error { return nil }

func (*poster) Classify(chat.Op, error) chat.Verdict { return chat.Verdict{Class: chat.Transient} }

// refStore fails the next SetInteractionMessageRef, as a crash would.
type refStore struct {
	*sqlite.Store
	failRef      bool
	beforeSubmit func(id string, answer []byte, decider string, now time.Time) // runs as a submit is made
}

func (s *refStore) SubmitInteraction(ctx context.Context, id string, answer []byte, decider string, now time.Time) error {
	if s.beforeSubmit != nil {
		s.beforeSubmit(id, answer, decider, now)
	}
	return s.Store.SubmitInteraction(ctx, id, answer, decider, now)
}

func (s *refStore) SetInteractionMessageRef(ctx context.Context, id, ref string) error {
	if s.failRef {
		s.failRef = false
		return errBoom
	}
	return s.Store.SetInteractionMessageRef(ctx, id, ref)
}

type lookup struct{ b interact.Binding }

func (l lookup) Binding(string) (interact.Binding, error) { return l.b, nil }

// fitter is the platform that is asked whether a prompt fits: it keeps what it is asked, and says what promptErr says.
type fitter struct{ r *rig }

func (f fitter) Fits(p chat.Prompt) error {
	f.r.asked = append(f.r.asked, p)
	return f.r.promptErr
}

type rig struct {
	in     *interact.Intake
	st     *refStore
	slack  *poster
	asked  []chat.Prompt // what the platform was asked to fit
	sess   *sess
	audit  *auditor
	outbox *outbox.Outbox
	hook   []interact.Outcome // what OnApprovalOutcome was told
	// promptErr, if set, is what the platform answers to Fits.
	promptErr error
}

func newRig(t *testing.T, mod func(*interact.Binding)) *rig {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &rig{st: &refStore{Store: st}, slack: &poster{posted: map[string]string{}}, sess: &sess{}}
	r.audit = &auditor{Writer: audit.New(st, audit.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ob := (outbox.New(outbox.Config{Store: st, Platform: r.slack, Now: func() time.Time { return t0 }, CallTimeout: 30 * time.Second, Logger: quiet}))
	b := interact.Binding{Dialect: kagentv1.New(30 * time.Second), AskUser: true, Timeout: timeout}
	r.outbox = ob
	if mod != nil {
		mod(&b)
	}
	if r.in, err = interact.New(interact.Config{Store: r.st, Lookup: lookup{b}, Platform: fitter{r}, Outbox: ob, Session: r.sess, Audit: r.audit, OnApprovalOutcome: func(_ string, o interact.Outcome) { r.hook = append(r.hook, o) }, Now: func() time.Time { return t0.Add(time.Hour) }}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) paused(t *testing.T, task a2a.Task) error {
	return r.in.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Task: task})
}

// row returns the interaction the pause of task became.
func (r *rig) row(t *testing.T, task a2a.Task) store.Interaction {
	t.Helper()
	in, err := (&kagentv1.Dialect{}).ParseInteraction(task)
	if err != nil || in == nil {
		t.Fatal(in, err)
	}
	row, err := r.st.GetInteraction(t.Context(), interact.InteractionID(*in))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestPausedRecordsOnePendingInteractionAndPostsOnePrompt(t *testing.T) {
	r, task := newRig(t, nil), askUser("req-1")
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, task)
	if row.State != store.InteractionPending || row.Key != key || row.TaskID != "task-1" || row.Kind != store.InteractionAskUser ||
		!row.CreatedAt.Equal(t0) || !row.Deadline.Equal(t0.Add(timeout)) {
		t.Errorf("interaction = %+v; want pending, for the thread and task, due the pause time plus the timeout", row)
	}
	if ts := r.slack.posted[outbox.ID(row.ID, store.OutboxReply)]; len(r.slack.posted) != 1 || ts == "" || row.MessageRef != ts {
		t.Errorf("posts %v, ref %q; want one post, outbox ID from the interaction, recorded as the ref", r.slack.posted, row.MessageRef)
	}
	if len(r.asked) != 1 || r.asked[0].ID != row.ID || len(r.asked[0].Questions) != 1 || r.asked[0].Questions[0].Text != "Which?" || !r.asked[0].Deadline.Equal(row.Deadline) {
		t.Errorf("the platform was asked to fit %+v; want the ID, the question and the deadline", r.asked)
	}
	// What is posted is the prompt in the neutral model, which the outbox renders for Slack: the question, and
	// the button that a press carries the interaction's ID back on.
	if posted := r.slack.contents[outbox.ID(row.ID, store.OutboxReply)]; posted.Prompt == nil || posted.Prompt.ID != row.ID || len(posted.Prompt.Questions) != 1 || posted.Prompt.Questions[0].Text != "Which?" {
		t.Errorf("posted %+v; want the prompt in the neutral model: its ID the interaction's, and its question", posted)
	}
	// Seen again an hour later (the rig's clock), as a reconciliation re-emits it: the prompt is not
	// rendered or sent again, and the deadline is the one from the pause time.
	if err := r.paused(t, task); err != nil || len(r.asked) != 1 || !r.row(t, task).Deadline.Equal(t0.Add(timeout)) {
		t.Errorf("second Paused = %v with the renderer asked %d times; want a pending prompt left alone", err, len(r.asked))
	}
}

// TestCrashAtEachSeam: the first Paused dies at a seam, the second is the pause coming again, and
// between them the thread has exactly one prompt.
func TestCrashAtEachSeamLeavesOneInteractionAndOnePrompt(t *testing.T) {
	for name, crash := range map[string]func(*rig){
		"after the interaction is recorded":                        func(r *rig) { r.slack.fault = "before" },
		"after the prompt is posted, before the outbox records it": func(r *rig) { r.slack.fault = "after" },
		"after the outbox records it, before the interaction does": func(r *rig) { r.st.failRef = true },
	} {
		t.Run(name, func(t *testing.T) {
			r, task := newRig(t, nil), askUser("req-1")
			crash(r)
			if err := r.paused(t, task); err == nil {
				t.Fatal("the first Paused succeeded through a crash")
			}
			if err := r.paused(t, task); err != nil {
				t.Fatal(err)
			}
			if row := r.row(t, task); len(r.slack.posted) != 1 || row.MessageRef == "" || row.MessageRef != r.slack.posted[outbox.ID(row.ID, store.OutboxReply)] {
				t.Errorf("posts %v, ref %q; want one prompt, recorded", r.slack.posted, row.MessageRef)
			}
		})
	}
}

func TestInteractionIDIsDeterministic(t *testing.T) {
	ask := dialect.Interaction{Kind: dialect.AskUser, TaskID: "task-1", RequestID: "req-1"}
	tools := dialect.Interaction{Kind: dialect.ToolApproval, TaskID: "task-1", Tools: []dialect.Tool{{ID: "t1"}, {ID: "t2"}}}
	reordered := tools
	reordered.Tools = []dialect.Tool{{ID: "t2"}, {ID: "t1"}}
	id := interact.InteractionID(ask)
	if id != interact.InteractionID(ask) || interact.InteractionID(tools) != interact.InteractionID(reordered) {
		t.Error("the same pause got two IDs")
	}
	if interact.InteractionID(dialect.Interaction{TaskID: "ab", RequestID: "c"}) == interact.InteractionID(dialect.Interaction{TaskID: "a", RequestID: "bc"}) {
		t.Error("fields that run together hash alike: the length prefixes are not doing their job")
	}
	for _, other := range []dialect.Interaction{{Kind: dialect.AskUser, TaskID: "task-1", RequestID: "req-2"}, {Kind: dialect.AskUser, TaskID: "task-2", RequestID: "req-1"}, tools} {
		if got := interact.InteractionID(other); got == id || len(got) > 40 {
			t.Errorf("%+v: ID %q; want one that is different and short", other, got)
		}
	}
}

// TestPausesThatAreNotPrompts: no prompt is rendered or posted for them. A tool approval is denied; one the
// Binding allows is not handled until Phase 5, and comes back as ErrDisallowed.
func TestPausesThatAreNotPrompts(t *testing.T) {
	approval := pausedOn(map[string]any{"type": "tool_approval_request", "id": "r", "tools": []map[string]any{{"id": "t1", "name": "x"}}})
	for name, tc := range map[string]struct {
		task a2a.Task
		mod  func(*interact.Binding)
		want error
	}{
		"an unusable pause":         {pausedOn(map[string]any{"type": "nonsense"}), nil, nil},
		"a tool approval":           {approval, nil, nil},
		"an approval, allowed":      {approval, func(b *interact.Binding) { b.ToolApprovals = true }, interact.ErrDisallowed},
		"ask-user on askUser false": {askUser("req-1"), func(b *interact.Binding) { b.AskUser = false }, nil},
		"a task that is not paused": {a2a.Task{ID: "task-9", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}, nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, tc.mod)
			if err := r.paused(t, tc.task); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Errorf("Paused = %v; want %v", err, tc.want)
			}
			if len(r.asked) != 0 || len(r.slack.posted) != 0 {
				t.Error("something was posted for a pause that is not a prompt")
			}
		})
	}
}

// TestAReusedRequestIDIsNotTheSamePause: kagent's request ID is the model's tool call ID, and a model
// may repeat it (UNCONFIRMED(WP-02)), so a second question under a seen ID is not the first one
// again. It is refused as unusable, and the first prompt stands.
func TestAReusedRequestIDIsNotTheSamePause(t *testing.T) {
	r := newRig(t, nil)
	if err := r.paused(t, askUser("req-1")); err != nil {
		t.Fatal(err)
	}
	other := pausedOn(map[string]any{"type": "ask_user_request", "id": "req-1", "questions": []map[string]any{{"question": "A different question?"}}})
	if err := r.paused(t, other); err != nil || len(r.slack.posted) != 1 || len(r.sess.cancels) != 1 {
		t.Errorf("Paused = %v with %d posts and %d cancels; want it refused as unusable (cancelled), and the first prompt alone", err, len(r.slack.posted), len(r.sess.cancels))
	}
}

// TestAStoredQuestionWithOtherUntrustedFieldsIsStillTheSame: a row written before a field was added to
// the request, or with another hint or context, is the same pause, and must not be refused and its
// task cancelled across an upgrade. What the answer depends on is compared, and not the bytes.
func TestAStoredQuestionWithOtherUntrustedFieldsIsStillTheSame(t *testing.T) {
	r := newRig(t, nil)
	task := pausedOn(map[string]any{"type": "ask_user_request", "id": "req-1", "hint": "a hint from today",
		"questions": []map[string]any{{"question": "Which?", "choices": []string{"a", "b"}}}})
	in, err := (&kagentv1.Dialect{}).ParseInteraction(task)
	if err != nil || in == nil {
		t.Fatal(in, err)
	}
	stored, _ := json.Marshal(in)
	var fields map[string]any
	_ = json.Unmarshal(stored, &fields)
	fields["added_in_a_later_version"] = []any{"x"}
	fields["hint"] = "a hint from an older version"
	fields["context_id"] = "another-context"
	old, _ := json.Marshal(fields)
	row := store.Interaction{ID: interact.InteractionID(*in), Key: key, TaskID: "task-1", Kind: store.InteractionAskUser, Request: old, CreatedAt: t0, Deadline: t0.Add(timeout)}
	if _, err := r.st.CreateInteraction(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := r.paused(t, task); err != nil || len(r.slack.posted) != 1 || len(r.sess.cancels) != 0 {
		t.Errorf("Paused = %v with %d posts and %d cancels; want the same pause, prompted and not cancelled", err, len(r.slack.posted), len(r.sess.cancels))
	}
}

// TestTheDeadlineNeverRunsPastCouriersClock: a task timestamp ahead of courier's clock (kagent's
// clock is UNCONFIRMED(WP-02)) cannot push the deadline out.
func TestTheDeadlineNeverRunsPastCouriersClock(t *testing.T) {
	r, task := newRig(t, nil), askUser("req-1")
	future := t0.Add(3 * time.Hour) // the rig's clock is t0 plus one hour
	task.Status.Timestamp = &future
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	if row := r.row(t, task); !row.CreatedAt.Equal(t0.Add(time.Hour)) || !row.Deadline.Equal(t0.Add(time.Hour+timeout)) {
		t.Errorf("created %v, due %v; want courier's own time plus the timeout", row.CreatedAt, row.Deadline)
	}
}
