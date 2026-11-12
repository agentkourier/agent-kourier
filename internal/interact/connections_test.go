package interact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// keyB is a thread of a second connection's app that has the same channel, thread and message IDs as key's: two apps in one
// workspace share them, so nothing but the connection tells the two threads apart.
var keyB = store.ConversationKey{Connection: "slack/team", Channel: key.Channel, Thread: key.Thread}

// twoApps is the press pipeline and the decider over two connections' platforms: who may answer and what a person is told
// are asked of the platform of the interaction's connection.
type twoApps struct {
	*decideRig
	azA, azB   *authz
	authorizer chat.PlatformSet[interact.Authorizer] // who may answer, by connection: a test takes a connection out to take it down
	ephA, ephB *ephemerals
	pl         *interact.Pipeline
	now        time.Time
}

func newTwoApps(t *testing.T) *twoApps {
	t.Helper()
	r := &twoApps{decideRig: newDecideRig(t), now: d0.Add(time.Minute)}
	r.ephA, r.ephB = r.eph, &ephemerals{}
	r.azA, r.azB = &authz{members: map[string]bool{"UALICE1": true}}, &authz{members: map[string]bool{"UBOB22": true}}
	ephemeral := chat.PlatformSet[interact.Ephemeral]{key.Connection: r.ephA, keyB.Connection: r.ephB}
	r.authorizer = chat.PlatformSet[interact.Authorizer]{key.Connection: r.azA, keyB.Connection: r.azB}
	decider, err := interact.NewDecider(decideCfg(interact.DecideConfig{
		Store: r.st, Auditor: r.aud, Ephemerals: ephemeral, Session: r.sess, Sessions: r.st, Outbox: r.out, Logger: testutil.DiscardLogger(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	b := interact.Binding{Dialect: kagentv1.New(time.Second), AskUser: true, Timeout: time.Minute, Identity: "secret:ns/tok"}
	r.pl, err = interact.NewPipeline(pressCfg(interact.PressConfig{
		Store: r.st, Sessions: r.st, Lookup: lookup{b}, Decider: decider, Auditor: r.aud, Ephemerals: ephemeral,
		Authorizers: r.authorizer,
		Limits:      store.PressLimits{Total: 10, PerUser: 2}, Logger: testutil.DiscardLogger(), Now: func() time.Time { return r.now },
	}))
	if err != nil {
		t.Fatal(err)
	}
	for id, k := range map[string]store.ConversationKey{"ix-a": key, "ix-b": keyB} {
		if err := r.st.UpsertSession(t.Context(), store.Session{Key: k, Binding: "ns/b", LastActivity: d0}); err != nil {
			t.Fatal(err)
		}
		req, _ := json.Marshal(question)
		if _, err := r.st.CreateInteraction(t.Context(), store.Interaction{ID: id, Key: k, TaskID: "task-1", Kind: store.InteractionAskUser, Request: req, CreatedAt: d0, Deadline: deadline}); err != nil {
			t.Fatal(err)
		}
		if err := r.st.SetInteractionMessageRef(t.Context(), id, "100.1"); err != nil { // the same message ID on both apps
			t.Fatal(err)
		}
	}
	return r
}

// press is user's Submit on the prompt id, as it came in on connection conn.
func (r *twoApps) press(t *testing.T, conn, id, user string) (interact.Result, error) {
	t.Helper()
	i := chat.Interaction{User: user, At: chat.Ref{Platform: "slack", Conn: conn, Channel: key.Channel, Thread: key.Thread, ID: "100.1"}, PromptID: id,
		Answers: []chat.Answer{{Question: 0, Values: []string{strconv.Itoa(0)}}}}
	if err := r.pl.Press(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	presses, err := r.st.ListPresses(t.Context())
	if err != nil || len(presses) == 0 {
		t.Fatalf("presses = %v, %v", presses, err)
	}
	return r.pl.Process(t.Context(), presses[len(presses)-1])
}

func (r *twoApps) state(t *testing.T, id string) store.InteractionState {
	t.Helper()
	row, err := r.st.GetInteraction(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row.State
}

// TestEachCheckAndWordReachesThePlatformOfTheInteractionsConnection: who may answer is asked of the app that posted the
// prompt, and a person is told on it too, though the two apps' prompts have the same channel, thread and message IDs.
func TestEachCheckAndWordReachesThePlatformOfTheInteractionsConnection(t *testing.T) {
	r := newTwoApps(t)

	// Bob is a member of b's channel only, and presses on b's prompt: a would say he is not a member.
	if res, err := r.press(t, keyB.Connection, "ix-b", "UBOB22"); res != interact.Done || err != nil {
		t.Fatalf("Bob's press on b = %v, %v", res, err)
	}
	if r.state(t, "ix-b") != store.InteractionAnswered || r.state(t, "ix-a") != store.InteractionPending {
		t.Errorf("states = %s on b, %s on a; want b answered and a untouched", r.state(t, "ix-b"), r.state(t, "ix-a"))
	}
	if r.azA.asked != 0 || r.azB.asked == 0 {
		t.Errorf("a was asked %d times and b %d; want b alone", r.azA.asked, r.azB.asked)
	}

	// Alice is a member of a's channel only; on b's prompt she is not a member, and is told so by b's app.
	if res, err := r.press(t, keyB.Connection, "ix-b", "UALICE1"); res != interact.Done || err != nil {
		t.Fatalf("Alice's press on b = %v, %v", res, err)
	}
	if len(r.ephB.got) != 1 || len(r.ephA.got) != 0 {
		t.Errorf("b told %v and a told %v; want the refusal told by b's app alone", r.ephB.got, r.ephA.got)
	}
	for _, to := range r.ephB.to {
		if to.Conn != keyB.Connection {
			t.Errorf("told in %+v; want connection %s", to, keyB.Connection)
		}
	}

	// And on a's prompt Alice answers.
	if res, err := r.press(t, key.Connection, "ix-a", "UALICE1"); res != interact.Done || err != nil || r.state(t, "ix-a") != store.InteractionAnswered {
		t.Errorf("Alice's press on a = %v, %v, state %s; want it taken", res, err, r.state(t, "ix-a"))
	}
}

// TestAPressFromAnotherConnectionIsRefused: a press whose connection is not its interaction's decides nothing, asks no
// platform who the person is, and is dropped; the same press from the interaction's own connection is taken. A Slack ID that
// collides between two apps is unlikely, and this is what holds if one does.
func TestAPressFromAnotherConnectionIsRefused(t *testing.T) {
	r := newTwoApps(t)

	res, err := r.press(t, keyB.Connection, "ix-a", "UALICE1") // a's prompt, pressed on b's app
	if res != interact.Done || err != nil {
		t.Fatalf("press = %v, %v; want it finished: nothing to retry", res, err)
	}
	if r.state(t, "ix-a") != store.InteractionPending {
		t.Errorf("a's interaction is %s after b's press; want it pending", r.state(t, "ix-a"))
	}
	if r.azA.asked != 0 || r.azB.asked != 0 || len(r.ephA.got)+len(r.ephB.got) != 0 {
		t.Errorf("platforms were asked %d and %d times and told %v, %v; want no call: the press is not this interaction's", r.azA.asked, r.azB.asked, r.ephA.got, r.ephB.got)
	}
	if left, _ := r.st.ListPresses(t.Context()); len(left) != 0 {
		t.Errorf("%d presses are kept; want it deleted", len(left))
	}

	if res, err := r.press(t, key.Connection, "ix-a", "UALICE1"); res != interact.Done || err != nil || r.state(t, "ix-a") != store.InteractionAnswered {
		t.Errorf("the same press from a's own app = %v, %v, state %s; want it taken", res, err, r.state(t, "ix-a"))
	}
}

// TestAPressOnAConnectionThatIsDownIsKept: nothing can say who pressed, which is no decision, so the press waits for the
// connection as a check that fails does.
func TestAPressOnAConnectionThatIsDownIsKept(t *testing.T) {
	r := newTwoApps(t)
	r.pl = nil
	b := interact.Binding{Dialect: kagentv1.New(time.Second), AskUser: true, Timeout: time.Minute, Identity: "secret:ns/tok"}
	var err error
	r.pl, err = interact.NewPipeline(pressCfg(interact.PressConfig{
		Store: r.st, Sessions: r.st, Lookup: lookup{b}, Decider: noDecider{}, Auditor: r.aud, Ephemerals: chat.PlatformSet[interact.Ephemeral]{},
		Authorizers: chat.PlatformSet[interact.Authorizer]{}, Limits: store.PressLimits{Total: 10, PerUser: 2}, Now: func() time.Time { return r.now },
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res, err := r.press(t, key.Connection, "ix-a", "UALICE1"); res != interact.Retry || !errors.Is(err, chat.ErrConnectionDown) {
		t.Errorf("press = %v, %v; want Retry and ErrConnectionDown", res, err)
	}
	if left, _ := r.st.ListPresses(t.Context()); len(left) != 1 {
		t.Errorf("%d presses are kept; want the one", len(left))
	}
}

type noDecider struct{}

func (noDecider) Decide(_ context.Context, _ interact.PressDecision) error {
	return errors.New("not decided")
}

// TestPausedPostsThePromptThroughTheConnectionOfThePause: the prompt's outbox row names the pause's connection, and a pause on a
// connection no platform runs posts nothing and says the connection is down.
func TestPausedPostsThePromptThroughTheConnectionOfThePause(t *testing.T) {
	r, task := newRig(t, nil), askUser("req-1")
	if err := r.paused(t, task); err != nil {
		t.Fatal(err)
	}
	row := r.row(t, task)
	if out, err := r.st.GetOutbox(t.Context(), outbox.ID(row.ID, store.OutboxReply)); err != nil || out.Connection != key.Connection {
		t.Errorf("prompt row = %+v, %v; want connection %s", out, err, key.Connection)
	}

	down := newRig(t, nil)
	in, err := interact.New(intakeCfg(interact.Config{
		Store: down.st, AuditReader: down.st, Lookup: lookup{interact.Binding{Dialect: kagentv1.New(30 * time.Second), AskUser: true, Timeout: timeout}},
		Platforms: chat.PlatformSet[interact.Fitter]{"slack/other": fitter{down}}, Outbox: down.outbox, Session: down.sess, Audit: down.audit,
		Now: func() time.Time { return t0.Add(time.Hour) },
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Paused(t.Context(), interact.Pause{Key: key, Binding: "ns/b", Task: task}); !errors.Is(err, chat.ErrConnectionDown) || len(down.slack.posted) != 0 {
		t.Errorf("Paused on a connection that is down = %v after %d posts; want ErrConnectionDown and none", err, len(down.slack.posted))
	}
}

// decidedOn adds the interaction id of the thread of conn, with a posted prompt, ended by the timeout.
func (r *replaceRig) decidedOn(t *testing.T, conn, id string) {
	t.Helper()
	k := key
	k.Connection = conn
	req, _ := json.Marshal(asked)
	if _, err := r.st.CreateInteraction(t.Context(), store.Interaction{ID: id, Key: k, TaskID: "task-1", Kind: store.InteractionAskUser, Request: req, CreatedAt: d0, Deadline: deadline}); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(r.st.SetInteractionMessageRef(t.Context(), id, "100.001"), r.resolve(store.InteractionTimedOut)(id)); err != nil {
		t.Fatal(err)
	}
}

// TestRepairEditsThroughTheConnectionOfEachPromptAndLeavesOnesThatAreDown: the prompts of two connections are edited each by
// its own platform, though their message IDs are the same; the prompts of a connection no platform runs are neither edited
// nor counted as failed edits, and do not take the places of the others' in a batch.
func TestRepairEditsThroughTheConnectionOfEachPromptAndLeavesOnesThatAreDown(t *testing.T) {
	a, b := &edits{}, &edits{}
	r := newRepairRig(t, func(c *interact.ReplacerConfig) {
		c.Batch = 2
		c.Updaters = chat.PlatformSet[interact.Editor]{key.Connection: a, keyB.Connection: b}
	})
	r.decidedOn(t, "slack/down", "0-down-1") // listed first
	r.decidedOn(t, "slack/down", "0-down-2")
	r.decidedOn(t, key.Connection, "a-1")
	r.decidedOn(t, keyB.Connection, "b-1")

	if err := r.r.Repair(t.Context()); err != nil {
		t.Fatalf("Repair = %v; want none: a connection that is down is no failed edit", err)
	}
	if len(a.got) != 1 || len(b.got) != 1 {
		t.Errorf("a made %d edits and b %d; want one each, in a batch of two that two prompts of a connection that is down came first in", len(a.got), len(b.got))
	}
}

// TestSweepEditsAndNotifiesThroughTheConnectionOfTheInteraction: the prompt that times out is edited by the platform of its
// connection, and the notice that says so is queued on the outbox for that connection.
func TestSweepEditsAndNotifiesThroughTheConnectionOfTheInteraction(t *testing.T) {
	r := newSweepRig(t)
	onB := &updates{log: &callLog{}}
	sw := r.sweeper(t, func(c *interact.SweepConfig) {
		c.Updaters = chat.PlatformSet[interact.Updater]{key.Connection: r.upd, keyB.Connection: onB}
	})
	if err := r.st.UpsertSession(t.Context(), store.Session{Key: keyB, ContextID: "ctx-1", TaskID: "task-1", Binding: "ns/b", LastActivity: t0}); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(asked)
	if _, err := r.st.CreateInteraction(t.Context(), store.Interaction{ID: "ix-b", Key: keyB, TaskID: "task-1", Kind: store.InteractionAskUser, Request: req, CreatedAt: t0, Deadline: t0.Add(timeout)}); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetInteractionMessageRef(t.Context(), "ix-b", "100.001"); err != nil {
		t.Fatal(err)
	}

	if err := sw.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(onB.got) != 1 || len(r.upd.got) != 0 {
		t.Errorf("b's platform made %d edits and a's %d; want b's alone", len(onB.got), len(r.upd.got))
	}
	rows := r.notices(t)
	if len(rows) != 1 || rows[0].Connection != keyB.Connection {
		t.Errorf("notices = %+v; want one, for connection %s", rows, keyB.Connection)
	}
}

// TestARateLimitPausesTheEditsOfItsOwnConnectionOnly: the limit is the app's own, so the Retry-After of one connection holds
// back that connection's prompts and the others go on, in the pass that met it and in the ones after.
func TestARateLimitPausesTheEditsOfItsOwnConnectionOnly(t *testing.T) {
	a := &edits{err: fmt.Errorf("slack chat.update: %w", &slackgo.RateLimitedError{RetryAfter: 30 * time.Second})}
	b := &edits{}
	r := newRepairRig(t, func(c *interact.ReplacerConfig) {
		c.Updaters = chat.PlatformSet[interact.Editor]{key.Connection: a, keyB.Connection: b}
	})
	r.decidedOn(t, key.Connection, "a-1")
	r.decidedOn(t, key.Connection, "a-2")
	r.decidedOn(t, keyB.Connection, "b-1")
	r.decidedOn(t, keyB.Connection, "b-2")

	if err := r.r.Repair(t.Context()); err != nil {
		t.Fatalf("Repair = %v; want none: a rate limit is waited out, not an error", err)
	}
	if len(a.got) != 1 || len(b.got) != 2 {
		t.Fatalf("a made %d edits and b %d; want one that was refused, and both of b's: a's limit does not hold b back", len(a.got), len(b.got))
	}
	r.advance(10 * time.Second)
	_ = r.r.Repair(t.Context())
	if len(a.got) != 1 {
		t.Errorf("a made %d edits after 10 seconds of its 30; want it left alone", len(a.got))
	}
	r.advance(21 * time.Second)
	_ = r.r.Repair(t.Context())
	if len(a.got) != 2 {
		t.Errorf("a made %d edits once its wait was over; want it asked again", len(a.got))
	}
}

// TestAPressOfAConnectionThatIsDownDecidesNothingUntilItRuns: a press replayed at a takeover, for a connection that nothing runs
// yet, cannot be checked (who may answer is the connection's platform's to say), so it is kept, and nothing reaches the session that
// would resume the paused turn. Once the connection runs the same press is decided.
func TestAPressOfAConnectionThatIsDownDecidesNothingUntilItRuns(t *testing.T) {
	r := newTwoApps(t)
	delete(r.authorizer, keyB.Connection)

	res, err := r.press(t, keyB.Connection, "ix-b", "UBOB22")
	if res != interact.Retry || !errors.Is(err, chat.ErrConnectionDown) {
		t.Fatalf("the press of a connection that is down = %v, %v; want Retry, with an error that says the connection is down", res, err)
	}
	if got := r.state(t, "ix-b"); got != store.InteractionPending {
		t.Errorf("the interaction is %s; want it still pending: nothing was decided", got)
	}
	if got := r.sess.answers(); len(got) != 0 {
		t.Errorf("%d answers reached the session; want none: the turn stays paused", len(got))
	}
	presses, err := r.st.ListPresses(t.Context())
	if err != nil || len(presses) != 1 {
		t.Fatalf("presses = %v, %v; want the one press kept", presses, err)
	}

	r.authorizer[keyB.Connection] = r.azB
	if res, err := r.pl.Process(t.Context(), presses[0]); res != interact.Done || err != nil {
		t.Fatalf("the press once the connection runs = %v, %v", res, err)
	}
	if got := r.state(t, "ix-b"); got != store.InteractionAnswered {
		t.Errorf("the interaction is %s; want it answered by the press that was kept", got)
	}
}
