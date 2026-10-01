package interact_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

var (
	t0      = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	key     = store.ConversationKey{Connection: "slack/main", Channel: "C1", ThreadTS: "1.1"}
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

// poster is the outbox's Slack, keeping posts by outbox ID. fault fails the next post "before" it
// happens, or reports an error "after" it did, as a crash would.
type poster struct {
	posted map[string]string
	fault  string
}

func (p *poster) Post(_ context.Context, _, _ string, _ slack.Content, id string) (string, error) {
	f := p.fault
	p.fault = ""
	if f == "before" {
		return "", errBoom
	}
	p.posted[id] = fmt.Sprintf("100.%03d", len(p.posted)+1)
	if f == "after" {
		return "", errBoom
	}
	return p.posted[id], nil
}

func (p *poster) Find(_ context.Context, _, _, id string, _ time.Time) (string, bool, error) {
	ts, ok := p.posted[id]
	return ts, ok, nil
}

// refStore fails the next SetInteractionMessageRef, as a crash would.
type refStore struct {
	*sqlite.Store
	failRef bool
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

type promptFn func(interact.Prompt) (slack.Content, error)

func (f promptFn) Prompt(p interact.Prompt) (slack.Content, error) { return f(p) }

type rig struct {
	in    *interact.Intake
	st    *refStore
	slack *poster
	asked []interact.Prompt // what the renderer was asked for
	sess  *sess
	audit *auditor
	hook  []interact.Outcome // what OnApprovalOutcome was told
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
	ob := (outbox.New(outbox.Config{Store: st, Poster: r.slack, Now: func() time.Time { return t0 }, CallTimeout: time.Second, Logger: quiet}))
	b := interact.Binding{Dialect: kagentv1.New(time.Second), AskUser: true, Timeout: timeout}
	if mod != nil {
		mod(&b)
	}
	prompts := promptFn(func(p interact.Prompt) (slack.Content, error) {
		r.asked = append(r.asked, p)
		return slack.Content{Text: "prompt " + p.ID}, nil
	})
	if r.in, err = interact.New(interact.Config{Store: r.st, Lookup: lookup{b}, Prompts: prompts, Outbox: ob, Session: r.sess, Audit: r.audit, OnApprovalOutcome: func(_ string, o interact.Outcome) { r.hook = append(r.hook, o) }, Now: func() time.Time { return t0.Add(time.Hour) }}); err != nil {
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
	if len(r.asked) != 1 || r.asked[0].ID != row.ID || r.asked[0].Interaction.RequestID != "req-1" || !r.asked[0].Deadline.Equal(row.Deadline) {
		t.Errorf("the renderer was asked %+v; want the ID, the pause and the deadline", r.asked)
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

// TestPausesThatAreNotPrompts: no prompt is rendered or posted for them. A tool approval comes back
// as ErrDisallowed until WP-33c2b.
func TestPausesThatAreNotPrompts(t *testing.T) {
	approval := pausedOn(map[string]any{"type": "tool_approval_request", "id": "r", "tools": []map[string]any{{"id": "t1", "name": "x"}}})
	for name, tc := range map[string]struct {
		task a2a.Task
		mod  func(*interact.Binding)
		want error
	}{
		"an unusable pause":         {pausedOn(map[string]any{"type": "nonsense"}), nil, nil},
		"a tool approval":           {approval, nil, interact.ErrDisallowed},
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
