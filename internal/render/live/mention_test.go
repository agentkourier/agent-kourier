package live_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

var declaredPeople = []v1alpha1.ChatMention{{Name: "sam", User: "U0SAM"}, {Name: "lee", User: "U0LEE"}, {Name: "kim", User: "U0KIM"}}

// mentionTurn is a turn of a Binding that declares the people.
func mentionTurn(mode v1alpha1.OutputMode, recipient string) live.Turn {
	return live.Turn{Mode: mode, Recipient: recipient, Binding: "ns/b", Mentions: declaredPeople, MaxMentions: 2}
}

// mentionRow is the row of turn-1's mentions, and whether there is one.
func (e *env) mentionRow(t *testing.T) (store.OutboxEntry, chat.Message, bool) {
	t.Helper()
	row, err := e.store.GetOutbox(t.Context(), outbox.ID(outbox.MentionSource("turn-1"), store.OutboxNotice))
	if errors.Is(err, store.ErrNotFound) {
		return row, chat.Message{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var out chat.Outgoing
	if err := json.Unmarshal([]byte(row.Payload), &out); err != nil || out.Message == nil {
		t.Fatalf("the row's payload %q: %v", row.Payload, err)
	}
	return row, *out.Message, true
}

// A reply that names a declared person makes one message that mentions the person, on every path a reply can take, and the
// reply itself says what the agent wrote: the name stays text, for the adapter to make inert.
func TestAReplyThatNamesAPersonQueuesAMention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      v1alpha1.OutputMode
		recipient string
	}{
		{"a stream", v1alpha1.OutputLive, "U1"},
		{"a message that is updated", v1alpha1.OutputLive, ""},
		{"a post when the turn ends", v1alpha1.OutputFinal, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			run, _ := e.startFor(t, mentionTurn(tc.mode, tc.recipient))
			run.Handle(text("a", "ask @sam about the rollout"))
			run.Handle(agent.FinalEvent{})
			e.finish(t, run)
			row, m, ok := e.mentionRow(t)
			if !ok {
				t.Fatal("no row for the mention")
			}
			if row.Role != store.OutboxNotice || row.Channel != "C1" || row.Thread != e.root {
				t.Errorf("row = %+v; want a notice in the thread", row)
			}
			if want := render.MentionMessage(declaredPeople[:1]); !reflect.DeepEqual(m, want) {
				t.Errorf("message = %+v, want %+v", m, want)
			}
			if got := e.reply().Text; !strings.Contains(got, "ask @sam about the rollout") {
				t.Errorf("the reply says %q; want the agent's text as it was", got)
			}
		})
	}
}

// A pause shows its text, a question that may be for the person it names, and so notifies as a turn's end does.
func TestAPauseThatNamesAPersonQueuesAMention(t *testing.T) {
	e := setup(t)
	run, _ := e.startFor(t, mentionTurn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "@sam, may I restart the pod?"))
	run.Handle(agent.FinalEvent{Task: &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}})
	e.finish(t, run)
	_, m, ok := e.mentionRow(t)
	if !ok {
		t.Fatal("no row for the mention")
	}
	if want := render.MentionMessage(declaredPeople[:1]); !reflect.DeepEqual(m, want) {
		t.Errorf("message = %+v, want %+v", m, want)
	}
}

// A turn that failed, was stopped or was rejected shows what it had, and notifies nobody. One that completed does.
func TestATurnThatEndedBadlyMakesNoMention(t *testing.T) {
	for _, tc := range []struct {
		state a2a.TaskState
		want  bool
	}{
		{a2a.TaskStateCompleted, true},
		{a2a.TaskStateFailed, false},
		{a2a.TaskStateCanceled, false},
		{a2a.TaskStateRejected, false},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			e := setup(t)
			run, _ := e.startFor(t, mentionTurn(v1alpha1.OutputLive, ""))
			run.Handle(text("a", "ask @sam"))
			run.Handle(agent.FinalEvent{Task: &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: tc.state}}})
			e.finish(t, run)
			if _, _, ok := e.mentionRow(t); ok != tc.want {
				t.Errorf("a row = %v, want %v", ok, tc.want)
			}
		})
	}
}

// A reply that never reached the thread notifies nobody of it.
func TestAReplyThatWasNotShownMakesNoMention(t *testing.T) {
	e := setup(t)
	send := &failingSender{err: fmt.Errorf("deliver: %w", outbox.ErrAbandoned)}
	cfg := e.cfg
	cfg.Sender = send
	e.live = newRenderer(t, cfg)
	run, _ := e.startFor(t, mentionTurn(v1alpha1.OutputFinal, ""))
	run.Handle(text("a", "ask @sam"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if send.calls != 1 {
		t.Errorf("%d calls, want the reply's one and no mention", send.calls)
	}
}

// cancelsOnPost is a Sender that ends the turn's context as the reply is posted, as a turn's end can.
type cancelsOnPost struct {
	live.Sender
	cancel context.CancelFunc
}

func (s cancelsOnPost) Send(ctx context.Context, p outbox.Post) (chat.Ref, error) {
	defer s.cancel()
	return s.Sender.Send(context.WithoutCancel(ctx), p)
}

// The context of a turn may end as the reply goes out. The mention is still queued.
func TestAContextThatEndsWithTheReplyLeavesTheMention(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg := e.cfg
	cfg.Sender = cancelsOnPost{Sender: e.ob, cancel: cancel}
	e.live = newRenderer(t, cfg)
	turn := mentionTurn(v1alpha1.OutputFinal, "")
	turn.Source, turn.Channel, turn.Thread = "turn-1", "C1", e.root
	run := e.live.Start(ctx, turn)
	run.Handle(text("a", "ask @sam"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if _, _, ok := e.mentionRow(t); !ok {
		t.Error("no row for the mention")
	}
}

// noEnqueue is a Sender that posts, and cannot queue.
type noEnqueue struct{ live.Sender }

func (noEnqueue) Enqueue(context.Context, outbox.Post) (string, error) {
	return "", errors.New("the store is down")
}

// A mention that could not be queued is logged, and is not recorded as made.
func TestAMentionThatWasNotQueuedIsNotRecorded(t *testing.T) {
	e := setup(t)
	reg := prometheus.NewRegistry()
	provider := sdktrace.NewTracerProvider()
	defer func() { _ = provider.Shutdown(context.Background()) }()
	obs, err := telemetry.New(reg, provider)
	if err != nil {
		t.Fatal(err)
	}
	aud := &mentionAudit{}
	cfg := e.cfg
	cfg.Sender, cfg.Audit = noEnqueue{e.ob}, aud
	e.live = newRenderer(t, cfg)
	turn := mentionTurn(v1alpha1.OutputLive, "")
	turn.Source, turn.Channel, turn.Thread = "turn-1", "C1", e.root
	run := e.live.Start(telemetry.WithObserver(t.Context(), obs), turn)
	run.Handle(text("a", "ask @sam"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if n := mentionCount(t, reg, "made"); n != 0 {
		t.Errorf("made = %v, want 0", n)
	}
	if len(aud.rows) != 0 {
		t.Errorf("audit rows = %+v, want none", aud.rows)
	}
	if !strings.Contains(e.logs.String(), "were not queued") {
		t.Errorf("logs = %q", e.logs.String())
	}
}

// What must not notify anyone makes no row, and the reply goes out as ever.
func TestAReplyMakesNoMention(t *testing.T) {
	for _, tc := range []struct{ name, say string }{
		{"no name", "the pods are crash looping"},
		{"a name that is not declared", "ask @nobody"},
		{"a broadcast", "@here @channel @everyone <!here> <!channel>"},
		{"a raw user ID", "ask U0SAM or <@U0SAM>"},
		{"a user group", "<!subteam^S1> @S1"},
		{"a name in code", "run `notify @sam`\n```\n@lee\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			run, _ := e.startFor(t, mentionTurn(v1alpha1.OutputLive, ""))
			run.Handle(text("a", tc.say))
			run.Handle(agent.FinalEvent{})
			e.finish(t, run)
			if _, m, ok := e.mentionRow(t); ok {
				t.Errorf("a row for %q: %+v", tc.say, m)
			}
		})
	}
}

// A Binding with no mentions behaves as it did: no row, whatever the reply says.
func TestABindingWithNoMentionsMakesNone(t *testing.T) {
	e := setup(t)
	run, _ := e.start(t, v1alpha1.OutputLive, false)
	run.Handle(text("a", "ask @sam"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if _, m, ok := e.mentionRow(t); ok {
		t.Errorf("a row: %+v", m)
	}
}

// The cap holds, and the first named are the ones mentioned.
func TestTheMentionsOfATurnStopAtTheCap(t *testing.T) {
	e := setup(t)
	run, _ := e.startFor(t, mentionTurn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "@kim @sam @lee"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	_, m, ok := e.mentionRow(t)
	if want := render.MentionMessage([]v1alpha1.ChatMention{declaredPeople[2], declaredPeople[0]}); !ok || !reflect.DeepEqual(m, want) {
		t.Errorf("message = %+v, want %+v", m, want)
	}
}

// Only the final text counts: a name the agent wrote and then took back notifies nobody.
func TestANameTheFinalTextDropsNotifiesNobody(t *testing.T) {
	e := setup(t)
	run, _ := e.startFor(t, mentionTurn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "ask @sam"))
	run.Handle(text("a", "ask the team"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if _, m, ok := e.mentionRow(t); ok {
		t.Errorf("a row: %+v", m)
	}
}

// A turn cut short never reached its final text, so it notifies nobody of what it showed so far.
func TestATurnCutShortMakesNoMention(t *testing.T) {
	e := setup(t)
	run, cancel := e.startCause(t, mentionTurn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "ask @sam"))
	eventually(t, "the first post", func() bool { return len(e.rec.sent("/chat.postMessage")) == 1 })
	cancel(errors.New("the send failed"))
	e.finish(t, run)
	if _, m, ok := e.mentionRow(t); ok {
		t.Errorf("a row: %+v", m)
	}
}

// mentionAudit records what the renderer audits.
type mentionAudit struct {
	mu   sync.Mutex
	rows []audit.Mention
	who  []audit.Actor
	err  error
}

func (a *mentionAudit) Mention(_ context.Context, who audit.Actor, m audit.Mention) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows, a.who = append(a.rows, m), append(a.who, who)
	return a.err
}

// Each person mentioned is an audit row, and a turn recovered after a restart and rendered again makes no second row.
func TestAMentionIsAuditedOnceHoweverOftenItsTurnIsRendered(t *testing.T) {
	e := setup(t)
	aud := &mentionAudit{}
	cfg := e.cfg
	cfg.Audit = aud
	e.live = newRenderer(t, cfg)
	for i := range 2 {
		turn := mentionTurn(v1alpha1.OutputLive, "")
		turn.Recovered = i == 1
		run, _ := e.startFor(t, turn)
		run.Handle(text("a", "ask @sam and @lee"))
		run.Handle(agent.FinalEvent{})
		e.finish(t, run)
	}
	want := []audit.Mention{
		{Turn: "turn-1", Channel: "C1", Thread: e.root, Name: "sam", User: "U0SAM"},
		{Turn: "turn-1", Channel: "C1", Thread: e.root, Name: "lee", User: "U0LEE"},
	}
	if !reflect.DeepEqual(aud.rows, want) {
		t.Errorf("audit rows = %+v, want %+v", aud.rows, want)
	}
	if len(aud.who) != 2 || aud.who[0].Binding != "ns/b" {
		t.Errorf("actors = %+v, want the Binding", aud.who)
	}
}

// A failed audit write is logged, and the mention stands.
func TestAFailedAuditLeavesTheMention(t *testing.T) {
	e := setup(t)
	cfg := e.cfg
	cfg.Audit = &mentionAudit{err: errors.New("the log is down")}
	e.live = newRenderer(t, cfg)
	run, _ := e.startFor(t, mentionTurn(v1alpha1.OutputLive, ""))
	run.Handle(text("a", "ask @sam"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if _, _, ok := e.mentionRow(t); !ok {
		t.Error("no row for the mention")
	}
	if !strings.Contains(e.logs.String(), "a mention was not audited") {
		t.Errorf("logs = %q", e.logs.String())
	}
}

// The metric says what the names came to, by Binding and outcome.
func TestTheMentionsOfATurnAreCounted(t *testing.T) {
	e := setup(t)
	reg := prometheus.NewRegistry()
	provider := sdktrace.NewTracerProvider()
	defer func() { _ = provider.Shutdown(context.Background()) }()
	obs, err := telemetry.New(reg, provider)
	if err != nil {
		t.Fatal(err)
	}
	turn := mentionTurn(v1alpha1.OutputLive, "")
	turn.Source, turn.Channel, turn.Thread = "turn-1", "C1", e.root
	run := e.live.Start(telemetry.WithObserver(t.Context(), obs), turn)
	run.Handle(text("a", "@sam @lee @kim @nobody @here"))
	run.Handle(agent.FinalEvent{})
	e.finish(t, run)
	if n := mentionCount(t, reg, "made"); n != 2 {
		t.Errorf("made = %v, want 2", n)
	}
	if n := mentionCount(t, reg, "undeclared"); n != 2 {
		t.Errorf("undeclared = %v, want 2", n)
	}
	if n := mentionCount(t, reg, "capped"); n != 1 {
		t.Errorf("capped = %v, want 1", n)
	}
}

// mentionCount reads agentkourier_mentions_total of the Binding ns/b and the outcome from reg.
func mentionCount(t *testing.T, reg *prometheus.Registry, outcome string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "agentkourier_mentions_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["binding"] == "ns/b" && labels["outcome"] == outcome {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// The sink tells a turn the Binding's people, as the session manager gives them.
func TestTheSinkPassesTheBindingsPeopleOn(t *testing.T) {
	e := setup(t)
	s := live.NewSink(e.live, e.ob, nil)
	h := s.Turn(t.Context(), session.TurnRef{Key: session.Key{Channel: "C1", Thread: e.root}, Binding: "ns/b", MessageID: "turn-1", UserID: session.SystemUser, Mentions: declaredPeople, MaxMentions: 2})
	h(text("a", "ask @sam"))
	h(agent.FinalEvent{})
	eventually(t, "the mention", func() bool {
		if e.clk.waiting() > 0 {
			e.clk.advance(throttle)
		}
		_, _, ok := e.mentionRow(t)
		return ok
	})
}
