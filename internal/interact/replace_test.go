package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// edits records the edits made to prompts, whole.
type edits struct {
	mu  sync.Mutex
	got []edit
	err error
}

type edit struct {
	channel, ts string
	content     slack.Content
}

func (e *edits) Update(_ context.Context, channel, ts string, c slack.Content) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, edit{channel, ts, c})
	return e.err
}

func (e *edits) list() []edit {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]edit(nil), e.got...)
}

type replaceRig struct {
	st *sqlite.Store
	up *edits
	r  *interact.Replacer
}

func newReplaceRig(t *testing.T) *replaceRig { return newReplaceRigWith(t, nil) }

// newReplaceRigWith is newReplaceRig with the Replacer's configuration changed by mod.
func newReplaceRigWith(t *testing.T, mod func(*interact.ReplacerConfig)) *replaceRig {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rig := &replaceRig{st: st, up: &edits{}}
	cfg := interact.ReplacerConfig{Store: st, Updater: rig.up, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mod != nil {
		mod(&cfg)
	}
	rig.r, err = interact.NewReplacer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rig
}

// decided adds the interaction id, with a posted prompt, decided the way finish says.
func (r *replaceRig) decided(t *testing.T, id string, in dialect.Interaction, finish func(id string) error) {
	t.Helper()
	req, _ := json.Marshal(in)
	row := store.Interaction{ID: id, Key: key, TaskID: "task-1", Kind: store.InteractionAskUser, Request: req, CreatedAt: d0, Deadline: deadline}
	if _, err := r.st.CreateInteraction(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetInteractionMessageRef(t.Context(), id, "100.001"); err != nil {
		t.Fatal(err)
	}
	if finish != nil {
		if err := finish(id); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *replaceRig) answer(by string, answers [][]string) func(string) error {
	return func(id string) error {
		a, _ := json.Marshal(dialect.Answer{RequestID: "req-1", Answers: answers})
		return r.st.SubmitInteraction(context.Background(), id, a, by, d0.Add(time.Minute))
	}
}

func (r *replaceRig) resolve(state store.InteractionState) func(string) error {
	return func(id string) error { return r.st.ResolveInteraction(context.Background(), id, state, deadline) }
}

var asked = dialect.Interaction{Kind: dialect.AskUser, TaskID: "task-1", RequestID: "req-1", Hint: "Pick a database", Questions: []dialect.Question{
	{Text: "Which?", Choices: []string{"alpha", "beta"}}, {Text: "Anything else?"}}}

// shown is everything an edit shows: its text and its blocks.
func shown(e edit) string { return e.content.Text + "\n" + string(e.content.Blocks) }

func TestAnAnsweredPromptIsReplacedWithTheAnswer(t *testing.T) {
	r := newReplaceRig(t)
	r.decided(t, "ix-1", asked, r.answer("UALICE1", [][]string{{"beta"}, {"hunter2-the-secret"}}))
	if err := r.r.Replace(t.Context(), "ix-1"); err != nil {
		t.Fatal(err)
	}
	got := r.up.list()
	if len(got) != 1 || got[0].channel != key.Channel || got[0].ts != "100.001" {
		t.Fatalf("edits %+v; want the posted prompt edited once", got)
	}
	out := shown(got[0])
	for _, want := range []string{"Answered", "@UALICE1", "Which?", "beta", "Pick a database"} {
		if !strings.Contains(out, want) {
			t.Errorf("the replacement lacks %q: %s", want, out)
		}
	}
	for _, gone := range []string{`"type":"input"`, `"type":"actions"`, `"type":"button"`, "alpha"} { // no control; a choice not chosen
		if strings.Contains(out, gone) {
			t.Errorf("the replacement still has %q: %s", gone, out)
		}
	}
	if strings.Contains(out, "hunter2-the-secret") || !strings.Contains(out, "(answered)") {
		t.Errorf("the replacement shows a free-text answer, or not that there is one: %s", out)
	}
}

func TestATimedOutOrCancelledPromptIsReplaced(t *testing.T) {
	for state, want := range map[store.InteractionState]string{store.InteractionTimedOut: "Timed out", store.InteractionCancelled: "Cancelled"} {
		r := newReplaceRig(t)
		r.decided(t, "ix-1", asked, nil)
		if err := r.resolve(state)("ix-1"); err != nil {
			t.Fatal(err)
		}
		if err := r.r.Replace(t.Context(), "ix-1"); err != nil {
			t.Fatal(err)
		}
		got := r.up.list()
		if len(got) != 1 || !strings.Contains(shown(got[0]), want) || strings.Contains(shown(got[0]), "@U") {
			t.Errorf("%s: edits %+v; want it said to be %q, with no one named", state, got, want)
		}
	}
}

// TestNothingToReplaceIsLeftAlone: a pending interaction still asks, one with no prompt posted has
// nothing to edit, and an unknown one is gone.
func TestNothingToReplaceIsLeftAlone(t *testing.T) {
	r := newReplaceRig(t)
	r.decided(t, "pending", asked, nil)
	req, _ := json.Marshal(asked)
	if _, err := r.st.CreateInteraction(t.Context(), store.Interaction{ID: "unposted", Key: key, TaskID: "task-1", Kind: store.InteractionAskUser, Request: req, CreatedAt: d0, Deadline: deadline}); err != nil {
		t.Fatal(err)
	}
	if err := r.resolve(store.InteractionTimedOut)("unposted"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pending", "unposted", "unknown"} {
		if err := r.r.Replace(t.Context(), id); err != nil {
			t.Errorf("Replace(%s) = %v; want nothing to do", id, err)
		}
	}
	if got := r.up.list(); len(got) != 0 {
		t.Errorf("edits %+v; want none", got)
	}
}

// TestAReplacementIsTheSameEveryTime: it is built from the row, so a repeat, as after a crash, is the
// same edit, and a failed one is reported for the caller to try again.
func TestAReplacementIsTheSameEveryTime(t *testing.T) {
	r := newReplaceRig(t)
	r.decided(t, "ix-1", asked, r.answer("UALICE1", [][]string{{"alpha"}, {"x"}}))
	r.up.err = errBoom
	if err := r.r.Replace(t.Context(), "ix-1"); !errors.Is(err, errBoom) {
		t.Fatalf("Replace = %v; want the edit's error", err)
	}
	r.up.err = nil
	for range 2 {
		if err := r.r.Replace(t.Context(), "ix-1"); err != nil {
			t.Fatal(err)
		}
	}
	got := r.up.list()
	if len(got) != 3 || shown(got[0]) != shown(got[1]) || shown(got[1]) != shown(got[2]) {
		t.Errorf("%d edits; want the failed one and two that are the same", len(got))
	}
}

// hangs is an Updater whose edit never answers: it waits for its context to end, as a Slack call that has
// gone quiet does, and counts how many it was given.
type hangs struct {
	mu sync.Mutex
	n  int
}

func (h *hangs) Update(ctx context.Context, _, _ string, _ slack.Content) error {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (h *hangs) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// withinASecond runs f and fails the test if it takes longer, so that a missing timeout fails and does not hang.
func withinASecond(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s did not return within a second: the edit is not bounded", what)
	}
}

// TestAHungEditIsCutOffAndRetried: an edit that never answers ends at the edit timeout, with its error, and
// the prompt is not counted as replaced, so that it is tried again.
func TestAHungEditIsCutOffAndRetried(t *testing.T) {
	h := &hangs{}
	r := newRepairRig(t, func(c *interact.ReplacerConfig) { c.Updater, c.EditTimeout = h, 20*time.Millisecond })
	r.decided(t, "ix-1", asked, r.resolve(store.InteractionTimedOut))
	var err error
	withinASecond(t, "Replace", func() { err = r.r.Replace(t.Context(), "ix-1") })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Replace = %v; want the timeout's error", err)
	}
	withinASecond(t, "Repair", func() { _ = r.r.Repair(t.Context()) })
	if got := h.count(); got != 2 {
		t.Errorf("%d edits; want the cut-off one tried again by the repair", got)
	}
}

func TestNewReplacerNeedsItsParts(t *testing.T) {
	if _, err := interact.NewReplacer(interact.ReplacerConfig{}); err == nil {
		t.Error("NewReplacer accepted no Store or Updater")
	}
}
