package interact

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

// failsOn is an Editor that fails for the messages in bad and succeeds for the rest.
type failsOn map[string]bool

func (f failsOn) Update(_ context.Context, r chat.Ref, _ chat.Outgoing) error {
	if f[r.ID] {
		return errors.New("slack chat.update: message_not_found")
	}
	return nil
}

func (failsOn) RefAt(channel, thread, id string) chat.Ref {
	return chat.Ref{Platform: "fake", Channel: channel, Thread: thread, ID: id}
}

// Classify says what the Slack adapter says, which is what the Replacer is run with.
func (failsOn) Classify(op chat.Op, err error) chat.Verdict {
	return slack.NewPlatform(nil, "ns/c").Classify(op, err)
}

// TestWhatLeftTheHorizonIsForgotten: the repair keeps what it has replaced, and what has failed, only while
// the interaction can still be listed; otherwise both would grow by one entry per interaction for as long
// as the process runs.
func TestWhatLeftTheHorizonIsForgotten(t *testing.T) {
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func(id, ref string, decidedAt time.Time) {
		t.Helper()
		row := store.Interaction{ID: id, Key: store.ConversationKey{Connection: "slack/main", Channel: "C1", Thread: "1.1"}, TaskID: "task-1",
			Kind: store.InteractionAskUser, Request: []byte(`{}`), CreatedAt: base, Deadline: base.Add(time.Minute)}
		if _, err := st.CreateInteraction(t.Context(), row); err != nil {
			t.Fatal(err)
		}
		if err := st.SetInteractionMessageRef(t.Context(), id, ref); err != nil {
			t.Fatal(err)
		}
		if err := st.ResolveInteraction(t.Context(), id, store.InteractionCancelled, decidedAt); err != nil {
			t.Fatal(err)
		}
	}
	add("old-done", "1.1", base)
	add("old-failed", "bad", base)
	add("recent-done", "2.2", base.Add(20*time.Hour))
	add("recent-failed", "bad", base.Add(20*time.Hour))
	now := base.Add(21 * time.Hour)
	r, err := NewReplacer(ReplacerConfig{Store: st, Updater: failsOn{"bad": true}, Now: func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Repair(t.Context())
	if len(r.done) != 2 || len(r.failures) != 2 {
		t.Fatalf("done %v, failures %v; want two of each before anything leaves the horizon", r.done, r.failures)
	}
	now = now.Add(4 * time.Hour) // the old ones are now 25 hours old; the recent ones 5
	_ = r.Repair(t.Context())
	if _, ok := r.done["recent-done"]; !ok || len(r.done) != 1 {
		t.Errorf("done %v; want only recent-done kept", r.done)
	}
	if n := r.failures["recent-failed"]; n != 2 || len(r.failures) != 1 {
		t.Errorf("failures %v; want only recent-failed kept, and still counted (it was tried again)", r.failures)
	}
}

// TestAnEditHasHalfAMinuteByDefault: an edit is bounded without being configured.
func TestAnEditHasHalfAMinuteByDefault(t *testing.T) {
	r, err := NewReplacer(ReplacerConfig{Store: struct{ store.Interactions }{}, Updater: failsOn{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.cfg.EditTimeout; got != 30*time.Second {
		t.Errorf("EditTimeout = %v; want 30s", got)
	}
}
