package broker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// askedPlatforms is a Resolver of the connections that run, which counts how often each connection was asked for.
type askedPlatforms struct {
	running chat.PlatformSet[chat.Platform]
	mu      sync.Mutex
	asked   map[string]int
	// stopped says why a connection that does not run is down, as the runtime table does for one whose credentials were rejected.
	stopped map[string]error
}

func (a *askedPlatforms) Platform(conn string) (chat.Platform, error) {
	a.mu.Lock()
	a.asked[conn]++
	why := a.stopped[conn]
	a.mu.Unlock()
	if why != nil {
		return nil, why
	}
	return a.running.Platform(conn)
}

func (a *askedPlatforms) askedFor(conn string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked[conn]
}

const (
	downOwn  = "ns/own"
	downTeam = "ns/team"
)

// downParts is the interactions and the session manager as the leader builds them, over a store and the connections that run: downOwn
// does, and downTeam is down (nothing runs it) until a test says otherwise.
func downParts(t *testing.T, bindings *Bindings) (interactionDeps, *interactionRuntime, *session.Manager, *askedPlatforms) {
	t.Helper()
	st := sqlitetest.Open(t)
	log := testutil.DiscardLogger()
	aud, err := audit.New(st, audit.Options{OnFailure: audit.NoFailure, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	platforms := &askedPlatforms{running: chat.PlatformSet[chat.Platform]{downOwn: builtPlatform{Platform: chattest.NewFake()}}, asked: map[string]int{}}
	ob, err := outbox.New(outbox.Config{Store: st, Platforms: chat.Narrow[outbox.Platform](platforms), OnAbandon: outbox.NoAbandon, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := NewMetrics(nil)
	if err != nil {
		t.Fatal(err)
	}
	d := interactionDeps{st: st, ob: ob, platforms: platforms, auditor: aud, bindings: bindings, metrics: metrics, log: log}
	ix, err := d.build(readinessSink{})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.New(session.Config{Context: t.Context(), Store: st, Questions: st, Bindings: bindings, Users: users{platforms}, Sink: ix.tap, Audit: aud, TextReply: ix.replies.Decide, ThreadContext: session.NoThreadContext, OnTurnEnd: metrics.TurnOutcome, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ix.late.bind(mgr)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ix.tap.Close(ctx)
		_ = mgr.Close(ctx)
	})
	return d, ix, mgr, platforms
}

// pendingQuestion records a session and a pending question of the thread, due at deadline, and returns the thread. The prompt was
// posted as the message ref says; none is as when the connection was down when it was asked.
func pendingQuestion(t *testing.T, st store.Store, conn, id string, deadline time.Time, ref string) store.ConversationKey {
	t.Helper()
	k := store.ConversationKey{Connection: conn, Channel: "C0123456789", Thread: "1.1"}
	if err := st.UpsertSession(t.Context(), store.Session{Key: k, Binding: "sre/oncall", LastActivity: time.Now()}); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(dialect.Interaction{Kind: dialect.AskUser, TaskID: a2a.TaskID("task-" + id), RequestID: "req-" + id, Questions: []dialect.Question{{Text: "Which?", Choices: []string{"a", "b"}}}})
	row := store.Interaction{ID: id, Key: k, TaskID: "task-" + id, Kind: store.InteractionAskUser, Request: req, CreatedAt: time.Now().Add(-2 * time.Hour), Deadline: deadline}
	if _, err := st.CreateInteraction(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if ref != "" {
		if err := st.SetInteractionMessageRef(t.Context(), id, ref); err != nil {
			t.Fatal(err)
		}
	}
	return k
}

func startInteractions(t *testing.T, d interactionDeps, ix *interactionRuntime, mgr *session.Manager) {
	t.Helper()
	var stops stopList
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stops.run(ctx)
	})
	if _, err := d.start(t.Context(), mgr, ix, &stops); err != nil {
		t.Fatal(err)
	}
}

// TestTheSweepLeavesTheTimeoutOfATeamConnectionThatIsDownPending: the leader's sweeper, over the table of the connections that run,
// times out the interaction of a connection that runs and not the one of a connection that is down: ending its pause would resume the
// turn into a thread that nothing reaches, and the notice would be queued for it. The pending interaction is timed out by a sweep
// once the connection runs (TestSweepLeavesAnInteractionOfAConnectionThatIsDownUntilItRuns).
func TestTheSweepLeavesTheTimeoutOfATeamConnectionThatIsDownPending(t *testing.T) {
	d, ix, mgr, _ := downParts(t, newFixture(t).b)
	due := time.Now().Add(-time.Hour)
	pendingQuestion(t, d.st, downOwn, "ix-own", due, "")
	pendingQuestion(t, d.st, downTeam, "ix-team", due, "")
	startInteractions(t, d, ix, mgr)

	testutil.Eventually(t, "the interaction of the connection that runs to be timed out", func() bool {
		row, err := d.st.GetInteraction(t.Context(), "ix-own")
		return err == nil && row.State == store.InteractionTimedOut
	}, testutil.Within(15*time.Second), testutil.Every(10*time.Millisecond))
	row, err := d.st.GetInteraction(t.Context(), "ix-team")
	if err != nil || row.State != store.InteractionPending {
		t.Errorf("the interaction of the connection that is down = %+v, %v; want it pending", row.State, err)
	}
	if _, err := d.st.GetOutbox(t.Context(), outbox.ID("ix-team", store.OutboxNotice)); err == nil {
		t.Error("a notice was queued for the connection that is down; want none until its timeout is recorded")
	}
}

// TestAReplayedPressOfATeamConnectionThatIsDownIsKeptUntilItRuns: the press worker's replay at a takeover reaches the connection's
// platform first, to ask whether the person may answer, and nothing that resumes the paused turn comes before that. With the
// connection down the press stays recorded and the question pending.
func TestAReplayedPressOfATeamConnectionThatIsDownIsKeptUntilItRuns(t *testing.T) {
	f := newFixture(t)
	d, ix, mgr, platforms := downParts(t, f.b)
	k := pendingQuestion(t, d.st, downTeam, "ix-team", time.Now().Add(time.Hour), "100.1")
	press := store.Press{InteractionID: "ix-team", User: "U1", Connection: downTeam, Channel: k.Channel, MessageRef: "100.1", Inputs: []byte("{}"), PressedAt: time.Now()}
	if _, err := d.st.RecordPress(t.Context(), press, store.PressLimits{Total: 10, PerUser: 10}); err != nil {
		t.Fatal(err)
	}
	startInteractions(t, d, ix, mgr)

	testutil.Eventually(t, "the replayed press to be tried", func() bool { return platforms.askedFor(downTeam) > 0 }, testutil.Within(15*time.Second), testutil.Every(10*time.Millisecond))
	row, err := d.st.GetInteraction(t.Context(), "ix-team")
	if err != nil || row.State != store.InteractionPending {
		t.Errorf("the interaction = %+v, %v; want it pending: nothing was decided", row.State, err)
	}
	if presses, err := d.st.ListPresses(t.Context()); err != nil || len(presses) != 1 {
		t.Errorf("presses = %v, %v; want the one press kept", presses, err)
	}
}
