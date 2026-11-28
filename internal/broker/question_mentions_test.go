package broker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// TestQuestionMentionsWireThroughBrokerToRenderer proves the broker's wiring between live.Renderer, live.Sink,
// interact.Tap and interact.Intake: a pause whose question names declared people notifies them with
// QuestionMentionMessage after the prompt is delivered, shares the turn's cap and deduplication with the reply
// text, and does not post a second mention or audit row if the pause is handed off again.
func TestQuestionMentionsWireThroughBrokerToRenderer(t *testing.T) {
	st := sqlitetest.Open(t)
	log := testutil.DiscardLogger()
	aud, err := audit.New(st, audit.Options{OnFailure: audit.NoFailure, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	fake := chattest.NewFake()
	platforms := chattest.Same[chat.Platform](fake)
	ob, err := outbox.New(outbox.Config{Store: st, Platforms: chat.Narrow[outbox.Platform](platforms), OnAbandon: outbox.NoAbandon, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := live.New(live.Config{Sender: ob, Audit: aud, Platforms: platforms, Store: st, Throttle: time.Millisecond, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = renderer.Wait(context.Background()) })

	people := []v1alpha1.ChatMention{
		{Name: "sam", User: "U0SAM"},
		{Name: "lee", User: "U0LEE"},
		{Name: "kim", User: "U0KIM"},
	}
	snap := snapshot(t, "http://agent.invalid", v1alpha1.DialectKagentV1, time.Hour)
	rb, _ := snap.Binding("sre", "oncall")
	rb.Binding.Spec.Interactions.AskUser = true
	rb.Binding.Spec.Interactions.Timeout = metav1.Duration{Duration: 10 * time.Minute}
	rb.Binding.Spec.Chat.Mentions = people
	rb.Binding.Spec.Chat.MaxMentionsPerTurn = new(2)

	s := &snaps{}
	s.p.Store(snap)
	bindings, err := NewBindings(s, &reader{vals: map[string]string{}}, aud, Options{OnRejected: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bindings.Close)

	metrics, err := NewMetrics(nil)
	if err != nil {
		t.Fatal(err)
	}
	questionMentions := func(ctx context.Context, req interact.QuestionMentions) {
		renderer.NotifyQuestionMentions(ctx, live.MentionTarget{
			Source:      req.Turn,
			Binding:     req.Binding,
			Connection:  req.Key.Connection,
			Channel:     req.Key.Channel,
			Thread:      req.Key.Thread,
			Mentions:    req.Mentions,
			MaxMentions: req.MaxMentions,
		}, req.Questions)
	}
	deps := interactionDeps{
		st: st, ob: ob, platforms: platforms, auditor: aud, bindings: bindings, metrics: metrics, log: log,
		mentions: questionMentions,
	}
	ix, err := deps.build(live.NewSink(renderer, ob, log))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.tap.Close(context.Background()) })

	root, err := fake.Post(t.Context(), chat.Target{Channel: "C1"}, chat.Say(chat.Message{Fallback: "root"}), "root")
	if err != nil {
		t.Fatal(err)
	}
	key := session.Key{Connection: "sre/slack", Channel: "C1", Thread: root.ID}
	ref := session.TurnRef{
		Key:         key,
		Binding:     "sre/oncall",
		MessageID:   "turn-1",
		UserID:      session.SystemUser,
		Output:      v1alpha1.OutputFinal,
		Mentions:    people,
		MaxMentions: 2,
	}

	now := time.Now().UTC()
	task := a2a.Task{
		ID:        "task-1",
		ContextID: "ctx-1",
		Status: a2a.TaskStatus{
			State:     a2a.TaskStateInputRequired,
			Timestamp: &now,
			Message: &a2a.Message{
				ID: "q-1",
				Metadata: map[string]any{
					kagentv1.HITLExtension: map[string]any{
						"type": "ask_user_request",
						"id":   "req-1",
						"questions": []map[string]any{
							{"question": "@sam and @lee and @kim, may I restart the pod?"},
						},
					},
				},
			},
		},
	}

	h := ix.tap.Turn(t.Context(), ref)
	h(agent.TextEvent{ArtifactID: "a1", Text: "Checking with @sam before restarting."})
	h(agent.FinalEvent{Task: &task})

	testutil.Eventually(t, "prompt and two capped mentions queued", func() bool {
		if _, err := st.GetOutbox(t.Context(), outbox.ID(outbox.MentionSource("turn-1", "U0SAM"), store.OutboxNotice)); err != nil {
			return false
		}
		if _, err := st.GetOutbox(t.Context(), outbox.ID(outbox.MentionSource("turn-1", "U0LEE"), store.OutboxNotice)); err != nil {
			return false
		}
		return true
	})
	if err := renderer.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := ix.tap.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	// @kim is past the shared cap of 2 across the reply and question.
	if _, err := st.GetOutbox(t.Context(), outbox.ID(outbox.MentionSource("turn-1", "U0KIM"), store.OutboxNotice)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("kim mention = %v, want ErrNotFound (past shared cap of 2)", err)
	}
	// @lee came only from the question prompt, so its message uses QuestionMentionMessage.
	leeRow, err := st.GetOutbox(t.Context(), outbox.ID(outbox.MentionSource("turn-1", "U0LEE"), store.OutboxNotice))
	if err != nil {
		t.Fatal(err)
	}
	var out chat.Outgoing
	if err := json.Unmarshal([]byte(leeRow.Payload), &out); err != nil || out.Message == nil {
		t.Fatalf("lee payload %q: %v", leeRow.Payload, err)
	}
	if want := render.QuestionMentionMessage(people[1]); !reflect.DeepEqual(*out.Message, want) {
		t.Errorf("lee mention message = %+v, want %+v", *out.Message, want)
	}
	// Exactly two mention audit rows: one for sam, one for lee.
	entries, err := st.ListAudit(t.Context(), time.Time{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var mentionAudits int
	for _, e := range entries {
		if e.Kind == store.AuditEvent && e.Subject == "mention turn=turn-1 channel=C1 thread="+root.ID {
			mentionAudits++
		}
	}
	if mentionAudits != 2 {
		t.Errorf("mention audit rows = %d, want 2 (entries: %+v)", mentionAudits, entries)
	}
}
