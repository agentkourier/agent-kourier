//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/testhooks"
	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// TestS16_FailoverLosesNoEventAndDuplicatesNone is the Phase 4 exit test (spec 12): two replicas on Postgres, a leader and a standby,
// pass a failover test with no lost or duplicated events. Its cases, one after another, each leaving the tier healthy for the next: the
// leader is killed mid-turn, mid-post and mid-storm, and frozen past its lease and then resumed. In each another process takes the
// leadership, which courier counts, and the fake Slack's record shows every reply once. (After a kill the standby and the killed leader's
// own container, which the kubelet restarts within seconds, race for the lease, and either may win it: both are a failover. Only a freeze
// leaves the standby alone to take over.)
//
// The fast tier only, brought up on Postgres with two replicas (E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up); anywhere else it
// skips and says why.
func TestS16_FailoverLosesNoEventAndDuplicatesNone(t *testing.T) {
	requireReplicas(t)
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"kill the leader mid-turn", s16KillMidTurn},
		{"kill the leader mid-post", s16KillMidPost},
		{"kill the leader mid-storm", s16KillMidStorm},
		{"stall the leader past its lease, then resume it", s16StalledLeader},
	} {
		t.Run(tc.name, tc.run)
	}
}

// exitLeadershipLost is the exit code of a courier that was the leader and no longer is (broker.ExitLeadershipLost): its lease
// lapsed or could not be renewed, or a write found that another replica has led since. exitCrashAfterPost is the testhooks fault's.
const (
	exitLeadershipLost = 5
	exitCrashAfterPost = 137
)

// failover is what S16 drives: the scenario, and the signaler that freezes or kills a replica's process, with the number of replicas
// the tier runs, which every case leaves running and healthy for the next.
type failover struct {
	*scenario
	sig  *Signaler
	want int
}

// requireReplicas skips the test unless it runs in the fast tier with two courier replicas on Postgres
// (E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up), and nowhere else is there a leader to lose: the full tier's courier is one
// SQLite replica, and the AX tier's too. It returns the environment, the pod client and how many replicas run.
func requireReplicas(t *testing.T) (Env, *kube.Pods, int) {
	t.Helper()
	env := NewEnv(os.Getenv)
	if err := CheckTier(env.Tier); err != nil {
		t.Fatal(err)
	}
	if env.Tier != "fast" {
		t.Skipf("S16 runs in the fast tier with two courier replicas on Postgres; the %s tier runs one courier on SQLite and has no standby to take over", env.Tier)
	}
	pods, err := kube.NewInCluster()
	if err != nil {
		t.Fatal(err)
	}
	running, err := pods.List(t.Context(), kube.CourierNamespace, courierPods)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range running {
		if p.DeletionTimestamp == nil {
			n++
		}
	}
	if n < 2 {
		t.Skipf("S16 needs two courier replicas, a leader and a standby, on Postgres; this tier runs %d (SQLite is one replica). Bring it up with: E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up", n)
	}
	return env, pods, n
}

// newFailover returns what a case of S16 needs, or skips (see requireReplicas), and leaves the tier healthy when the case ends.
func newFailover(t *testing.T) *failover {
	t.Helper()
	env, _, n := requireReplicas(t)
	f := &failover{scenario: newScenario(t), sig: NewSignaler(env.SignalerURL), want: n}
	if err := f.sig.Ready(t.Context()); err != nil {
		t.Fatalf("%v: the tier was brought up with two replicas, which brings up the signaler (hack/kind/fast/signaler.yaml) too", err)
	}
	t.Cleanup(f.settle)
	return f
}

// settle leaves the tier as it found it for the next case: every replica Ready, as many as there were, and exactly one leader, connected.
// A killed container is restarted by the kubelet, after a back-off that grows with each crash of the same pod, so it is waited for.
func (f *failover) settle() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(f.ctx), 8*time.Minute)
	defer cancel()
	err := Wait(ctx, 8*time.Minute, fmt.Sprintf("%d courier replicas to run, Ready, with one connected leader", f.want), func(ctx context.Context) (bool, error) {
		all, err := f.reps.All(ctx)
		if err != nil || len(all) != f.want {
			return false, err
		}
		if err := f.pods.WaitReady(ctx, kube.CourierNamespace, courierPods); err != nil {
			return false, err
		}
		_, err = f.reps.WaitLeader(ctx, 30*time.Second)
		return err == nil, err
	})
	if err != nil {
		f.t.Errorf("the tier is not healthy after the case: %v", err)
	}
}

// restarts says how many times each running replica's container has restarted, by pod, for onlyEnded.
func (f *failover) restarts() map[string]int32 {
	f.t.Helper()
	all, err := f.reps.All(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]int32{}
	for _, r := range all {
		out[r.Name] = r.Restarts
	}
	return out
}

// onlyEnded checks that no process ended but the one the case killed or froze, which is in the pod named: no other replica's container
// has restarted since before, as the leader's successor's would if it crashed on the work it took over. It runs once the tier has settled.
func (f *failover) onlyEnded(before map[string]int32, pod string) {
	f.t.Helper()
	for name, n := range f.restarts() {
		if was, ok := before[name]; ok && name != pod && n != was {
			f.t.Errorf("the container of %s restarted %d times since the case began; want none: only %s was to end", name, n-was, pod)
		}
	}
}

// gate returns a gate name for a case (Stub.Gate), and opens the gate when the case ends, whatever its outcome: a gate that is left
// closed holds its task for good, and with it the trigger's count of the runs still going, which a later run's limits would meet.
func (f *failover) gate(name string) string {
	g := f.stub.Gate(name)
	f.t.Cleanup(func() {
		if err := f.stub.Release(context.WithoutCancel(f.ctx), g); err != nil {
			f.t.Errorf("release the gate %s at the end: %v", g, err)
		}
	})
	return g
}

// leader is the one leader the tier has now.
func (f *failover) leader() kube.Replica {
	f.t.Helper()
	l, err := f.reps.WaitLeader(f.ctx, 2*time.Minute)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}

// takeover waits for a leader that is not the process old, which is dead or frozen, and returns it, once it has counted its takeover:
// courier_leader_takeovers_total is 1 in a process that took the leadership once, as the new leader did, and the standby has not led before.
// The new leader may be the other pod or, after a kill, the old pod's restarted container, which is another process: they race for the
// lease. It takes the lease duration of the tier (9 s) after the last renewal, and a start-up.
func (f *failover) takeover(old kube.Replica) kube.Replica {
	f.t.Helper()
	began := time.Now()
	next, err := f.reps.WaitLeader(f.ctx, 3*time.Minute, old.ID())
	if err != nil {
		f.t.Fatalf("no replica took over from %s: %v", old.ID(), err)
	}
	took, err := f.reps.Counter(f.ctx, next, "courier_leader_takeovers_total", nil)
	if err != nil || took != 1 {
		f.t.Errorf("the new leader %s counts %v takeovers (%v); want 1", next.ID(), took, err)
	}
	all, _ := f.reps.All(f.ctx)
	var state []string
	for _, r := range all {
		state = append(state, fmt.Sprintf("%s (restarts %d)", r.ID(), r.Restarts))
	}
	f.t.Logf("%s took over from %s, %s after the wait began; running: %s", next.ID(), old.ID(), time.Since(began).Round(100*time.Millisecond), strings.Join(state, ", "))
	return next
}

// completedTurns is how many turns of the Binding have completed in the leader's process, which counts from 0 with each process: a turn
// that the new leader recovers and finishes is counted once there, and a turn it handled twice would be counted twice.
func (f *failover) completedTurns(leader kube.Replica, binding string) float64 {
	f.t.Helper()
	v, err := f.reps.Counter(f.ctx, leader, "courier_turn_outcomes_total", map[string]string{"binding": binding, "outcome": "completed"})
	if err != nil && !errors.Is(err, ErrNoSample) {
		f.t.Fatal(err)
	}
	return v
}

// agentMessages are the bot's messages of a thread that the agent's turn made: the alert's own, which a trigger's thread starts with
// when the bot is the one that posted it, is not one (alertBot is its ID; empty when a person started the thread).
func agentMessages(th []fakeslack.MessageView, alertBot string) []fakeslack.MessageView {
	var out []fakeslack.MessageView
	for _, b := range BotMessages(th) {
		if alertBot == "" || b.BotID != alertBot {
			out = append(out, b)
		}
	}
	return out
}

// split divides the agent's messages by whether they carry the end of the gated reply, "Released.".
func split(th []fakeslack.MessageView, alertBot string) (whole, partial []fakeslack.MessageView) {
	for _, m := range agentMessages(th, alertBot) {
		if strings.Contains(m.Text, "Released.") {
			whole = append(whole, m)
		} else {
			partial = append(partial, m)
		}
	}
	return whole, partial
}

// held is the thread of a gated turn while it is held: one message of the agent with the first chunk and not the end, and, for a
// mention's reply, which is a stream, still streaming (a trigger's reply is an edit).
func held(gate, alertBot string, streaming bool) func([]fakeslack.MessageView) bool {
	return func(th []fakeslack.MessageView) bool {
		whole, partial := split(th, alertBot)
		return len(whole) == 0 && len(partial) == 1 && strings.Contains(partial[0].Text, "Holding on gate "+gate) && (!streaming || partial[0].Streaming)
	}
}

// answered is the thread of a gated turn that has ended: exactly one finished message with the whole reply, the text before the gate and
// after it each once, and no half reply the dead leader left behind: the new leader adopts the stream it began, as the outbox adopts a
// post (H4-5b), so there is nothing to leave.
func answered(gate, alertBot string) func([]fakeslack.MessageView) bool {
	return func(th []fakeslack.MessageView) bool {
		whole, partial := split(th, alertBot)
		return len(whole) == 1 && finished(whole[0]) && strings.Count(whole[0].Text, "Holding on gate "+gate) == 1 && strings.Count(whole[0].Text, "Released.") == 1 && len(partial) == 0
	}
}

// s16KillMidTurn (case 1): the leader is killed (SIGKILL: no shutdown, and no hand-over of the
// Lease) while its turn is held mid-stream. The standby takes over once the lease lapses, which it counts, finds the task and follows it,
// and when the task ends the thread has the whole reply in one finished message, with no text twice, no second answer and no half message
// left by the dead leader (the new leader adopts its stream, H4-5b).
func s16KillMidTurn(t *testing.T) {
	f := newFailover(t)
	old := f.leader()
	before := f.restarts()
	const binding = "courier/fast-s16"

	gate := f.gate("S16a")
	m := f.mention(ChannelS16, "US16A", "answer me gate:"+gate)
	f.thread(ChannelS16, m.TS, "the first chunk, while the task is held", held(gate, "", true))

	if err := f.sig.Kill(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	next := f.takeover(old)
	if err := f.stub.Release(f.ctx, gate); err != nil {
		t.Fatal(err)
	}
	f.thread(ChannelS16, m.TS, "the reply to end, once, after the takeover", answered(gate, ""))

	err := Consistently(f.ctx, 5*time.Second, "the thread holds the one whole reply, with its text once", func(ctx context.Context) (bool, error) {
		th, err := f.slack.Thread(ctx, ChannelS16, m.TS)
		return answered(gate, "")(th), err
	})
	if err != nil {
		th, _ := f.slack.Thread(context.WithoutCancel(f.ctx), ChannelS16, m.TS)
		t.Errorf("%v\nthe thread: %s", err, describe(th))
	}
	if got := f.completedTurns(next, binding); got != 1 {
		t.Errorf("the new leader counts %v completed turns for %s; want 1: the turn is finished once", got, binding)
	}
	f.settle()
	f.onlyEnded(before, old.Name)
}

// s16KillMidPost (case 2): S5's crash between posting a reply and recording that it did
// (the testhooks fault, exit 137), on the leader of two replicas. The standby that takes over finds the post by its tag, as a restart
// does, and the thread holds that one message, not a second; that the new leader is done with the turn it found unfinished is shown by a
// second message of the thread being answered after it, once. The fault is armed on both replicas (a Deployment's environment), and
// disarmed, with a rollout, once the takeover is seen.
func s16KillMidPost(t *testing.T) {
	f := newFailover(t)
	arm := func(ctx context.Context, fault string) error {
		rollout, cancel := context.WithTimeout(ctx, 4*time.Minute) // a stuck rollout fails here, with its reason
		defer cancel()
		return f.pods.SetFault(rollout, kube.CourierNamespace, kube.CourierDeployment, courierContainer, fault)
	}
	t.Cleanup(func() { // runs before settle: the fault must be gone for the tier to be left healthy
		if err := arm(context.WithoutCancel(f.ctx), ""); err != nil {
			t.Errorf("disarm at the end: %v", err)
		}
	})
	if err := arm(f.ctx, testhooks.CrashAfterPost); err != nil {
		t.Fatal(err)
	}
	f.connected()
	old := f.leader()

	tok := f.stub.Gate("S16b")
	m := f.mention(ChannelS16B, "US16B", "answer once "+tok)
	crash, err := f.pods.WaitExit(f.ctx, kube.CourierNamespace, old.Name, courierContainer, old.ContainerID)
	if err != nil {
		t.Fatalf("the leader never crashed after posting: %v", err)
	}
	if crash.ExitCode != exitCrashAfterPost {
		t.Fatalf("the leader ended with exit code %d (%s); want %d, the fault's", crash.ExitCode, crash.Reason, exitCrashAfterPost)
	}
	posted := f.thread(ChannelS16B, m.TS, "the reply the leader posted before it crashed", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) == 1 && OutboxID(bots[0]) != "" && strings.Contains(bots[0].Text, s5Answer)
	})
	f.takeover(old)
	if err := arm(f.ctx, ""); err != nil { // the new leader has the fault armed too
		t.Fatal(err)
	}
	f.connected()

	again := f.reply(ChannelS16B, m.TS, "US16B", "and again "+tok)
	th := f.thread(ChannelS16B, m.TS, "the answer to the second message", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) >= 2 && CompareTS(bots[len(bots)-1].TS, again.TS) > 0
	})
	check := func(th []fakeslack.MessageView) (bool, string) { return answeredOnceBeforeAndAfter(th, again) }
	if ok, got := check(th); !ok {
		t.Fatalf("want one tagged message of the bot before the second message and one after, each its own and the agent's answer: the first reply is not posted twice\nthe thread: %s", got)
	}
	if before := BotMessages(th)[0]; before.TS != BotMessages(posted)[0].TS {
		t.Errorf("the first reply is the message %s now; the leader posted %s: it was replaced, not found by its tag", before.TS, BotMessages(posted)[0].TS)
	}
	err = Consistently(f.ctx, 3*time.Second, "the thread keeps its two tagged messages of the bot", func(ctx context.Context) (bool, error) {
		th, err := f.slack.Thread(ctx, ChannelS16B, m.TS)
		ok, _ := check(th)
		return ok, err
	})
	if err != nil {
		th, _ := f.slack.Thread(context.WithoutCancel(f.ctx), ChannelS16B, m.TS)
		t.Errorf("%v\nthe thread: %s", err, describe(th))
	}
}

// stormAlertGated is S15's alert for S16's channel and bot, naming a gate in its detail line, which the gated agent takes from the prompt
// (the preset puts the whole alert in it) and holds on.
func stormAlertGated(name, group, gate string) fakeslack.WebhookPost {
	p := alertPost("FIRING", name, group)
	p.Channel, p.BotID = ChannelS16C, BotS16
	p.Attachments[0].Text += "gate:" + gate + "\n"
	return p
}

// s16KillMidStorm (case 3): six distinct alerts through the incoming webhook to a
// channel whose trigger allows three starts in 10 seconds, S15's, with an agent that holds each investigation on a gate. Courier takes all
// six in (each is acknowledged) and the leader is killed with the first three investigations running. The standby takes over: each
// started investigation ends in its own thread, once; the other three start nothing and get no reply; and one line in the channel, at the
// top level, says that three were not investigated: once, though the limits, the window and the digest are the dead leader's state, in the
// database.
func s16KillMidStorm(t *testing.T) {
	f := newFailover(t)
	const (
		storm   = 6
		started = 3
		digest  = "3 alert messages not investigated in the last 10s (rate limit)."
	)
	old := f.leader()
	before := f.restarts()

	// The channel and courier's database outlive a run, and a key holds its thread for the cooldown: the alerts are ones no earlier run
	// posted, each with a key of its own.
	suffix := fmt.Sprintf("x%d", time.Now().UnixNano()%1_000_000_000)
	gates := make([]string, storm)
	var alerts []fakeslack.UserMessage
	for i := range storm {
		gates[i] = f.gate(fmt.Sprintf("S16c-%d", i))
		name := fmt.Sprintf("AlertS16%s%d", suffix, i)
		m, err := f.slack.Webhook(f.ctx, stormAlertGated(name, name+" shop", gates[i]))
		if err != nil {
			t.Fatal(err)
		}
		f.acked(m) // courier takes them in the order they were posted, so the first three are the ones that start
		alerts = append(alerts, m)
	}
	// What the leader decided before it dies: the first three started and the other three were limited, which are the numbers that the
	// new leader must keep to.
	for decision, want := range map[string]float64{"started": started, "limited": storm - started} {
		got, err := f.reps.Counter(f.ctx, old, "courier_trigger_decisions_total", map[string]string{"binding": "courier/fast-s16c", "decision": decision})
		if err != nil || got != want {
			t.Errorf("the leader counts %v %q decisions (%v) before it is killed; want %v", got, decision, err, want)
		}
	}
	for i, m := range alerts[:started] {
		f.thread(ChannelS16C, m.TS, fmt.Sprintf("the investigation of alert %d, held", i), held(gates[i], BotS16, false))
	}

	if err := f.sig.Kill(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	f.takeover(old)
	for _, g := range gates {
		if err := f.stub.Release(f.ctx, g); err != nil {
			t.Fatal(err)
		}
	}

	// Each started investigation ends once, in its own thread, and says what its own gate held.
	for i, m := range alerts[:started] {
		f.thread(ChannelS16C, m.TS, fmt.Sprintf("the investigation of alert %d to end, once", i), answered(gates[i], BotS16))
	}
	err := Consistently(f.ctx, 5*time.Second, "the started threads keep their one whole reply and the others have none", func(ctx context.Context) (bool, error) {
		for i, m := range alerts {
			th, err := f.slack.Thread(ctx, ChannelS16C, m.TS)
			if err != nil {
				return false, err
			}
			if i < started && !answered(gates[i], BotS16)(th) || i >= started && len(agentMessages(th, BotS16)) != 0 {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Errorf("%v", err)
	}

	// The digest, from the new leader: the window ended 10 seconds after the first limited alert, and the digest loop passes every 15.
	isDigest := func(m fakeslack.MessageView) bool {
		return m.ThreadTS == "" && m.BotID != "" && m.BotID != BotS16 && m.TS > alerts[0].TS && strings.Contains(m.Text, "not investigated in the last")
	}
	digests := func(ctx context.Context) (n int, text string, err error) {
		msgs, err := f.slack.Messages(ctx, ChannelS16C)
		for _, m := range msgs {
			if isDigest(m) {
				n, text = n+1, m.Text
			}
		}
		return n, text, err
	}
	var text string
	err = Wait(f.ctx, 2*time.Minute, "the digest line in the channel", func(ctx context.Context) (bool, error) {
		n, got, err := digests(ctx)
		text = got
		return n >= 1, err
	})
	if err != nil {
		msgs, _ := f.slack.Messages(f.ctx, ChannelS16C)
		t.Fatalf("%v\nthe channel: %s", err, describe(msgs))
	}
	if !strings.Contains(text, digest) || strings.Contains(text, "AlertS16") {
		t.Errorf("the digest reads %q; want %q, naming no alert", text, digest)
	}
	// Once: the loop passes every 15 seconds, so a second line would be there within 20.
	err = Consistently(f.ctx, 20*time.Second, "the channel holds the one digest", func(ctx context.Context) (bool, error) {
		n, _, err := digests(ctx)
		return n == 1, err
	})
	if err != nil {
		t.Errorf("%v", err)
	}
	f.settle()
	f.onlyEnded(before, old.Name)
}

// s16StalledLeader (case 4): the leader's process is frozen (SIGSTOP, as a paused node or a long pause
// does: it neither runs nor answers, and its connections stay open) while its turn is held, and the task ends meanwhile (its gate opens).
// The standby takes over and renders the finished task, with the old leader still frozen: the thread has the whole reply, once. Then the
// old leader is resumed. It must not lead again, nor write: it ends at once with the exit code of a lost leadership (5), and the reason it
// logs must be a fence's (a store write refused for a stale epoch, or, once courier fences them, a refused chat write), not the loss of
// the lease alone, which refuses nothing. The new leader stays the leader.
//
// What the design accepts (D37) is the one Slack call that the paused process has in flight when it is resumed: an edit to a message that
// already existed is logged as RESIDUAL RISK OBSERVED. A second whole reply, a half reply left behind, or a message that is new after the
// old leader was fenced fails the test. The first of these is red until a deposed leader's Slack writes are fenced and its open stream
// is adopted (H4-5b): today the resumed leader finishes its own stream before its first store write is refused.
func s16StalledLeader(t *testing.T) {
	f := newFailover(t)
	old := f.leader()
	restarts := f.restarts()
	const binding = "courier/fast-s16"

	gate := f.gate("S16d")
	m := f.mention(ChannelS16, "US16D", "answer me gate:"+gate)
	f.thread(ChannelS16, m.TS, "the first chunk, while the task is held", held(gate, "", true))

	// A frozen container also fails its liveness probe: the kubelet restarts it after the probe's misses (the tier's chart values raise them
	// to six ten seconds apart, so 50 to 60 seconds), and the old leader must be resumed before.
	t.Cleanup(func() { // a container left frozen would hold the tier until the kubelet's liveness probe killed it
		_ = f.sig.Cont(context.WithoutCancel(f.ctx), old.ContainerID)
	})
	frozen := time.Now()
	if err := f.sig.Stop(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	// The task ends while nobody leads: its closing events wait in the frozen process's socket, and the new leader finds the task finished and
	// renders it. (Case 1 is the one where the new leader follows a task that still runs.) The new leader's rendering is done before the old
	// one is resumed, so that anything the thread shows after is the old leader's.
	if err := f.stub.Release(f.ctx, gate); err != nil {
		t.Fatal(err)
	}
	next := f.takeover(old)
	before := f.thread(ChannelS16, m.TS, "the reply to end, once, written by the new leader while the old one is frozen", answered(gate, ""))
	if got := f.completedTurns(next, binding); got != 1 {
		t.Errorf("the new leader counts %v completed turns for %s; want 1", got, binding)
	}
	asleep := time.Since(frozen)
	if asleep > 40*time.Second {
		t.Logf("the old leader was frozen for %s before it was resumed: getting close to the kubelet's liveness restart (50 to 60 s)", asleep.Round(time.Second))
	}

	if err := f.sig.Cont(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	exit, err := f.pods.WaitExit(f.ctx, kube.CourierNamespace, old.Name, courierContainer, old.ContainerID)
	if err != nil {
		t.Fatalf("the old leader, resumed after %s, did not end: %v", asleep.Round(time.Second), err)
	}
	if exit.ExitCode != exitLeadershipLost {
		t.Fatalf("the old leader ended with exit code %d (%s) after %s frozen; want %d, a lost leadership: the fail-stop did not run, or the kubelet got there first",
			exit.ExitCode, exit.Reason, asleep.Round(time.Second), exitLeadershipLost)
	}
	// Exit 5 is also what a lost lease gives, which refuses no write: so the reason it gave is read, and it must be a fence's. The resumed
	// process meets its renewal deadline and its first store write together, and either may end it first, so a run whose lease loss came
	// first fails here with that reason; with the lease at nine seconds the write has won every time it was run.
	logs, err := f.pods.Logs(f.ctx, kube.CourierNamespace, old.Name, courierContainer, exit.Previous, 400)
	if err != nil {
		t.Fatalf("the old leader's log is not readable, so why it stopped is not known: %v", err)
	}
	reasons := ExitReasons(logs)
	t.Logf("the old leader was frozen for %s, resumed, and ended with exit code %d; its log says: %q", asleep.Round(time.Second), exit.ExitCode, reasons)
	fenced := StoppedByFence(reasons) // judged below, against what it wrote

	// The tier is whole again, and the new leader was not disturbed: the old one came back as a standby.
	f.settle()
	// The kubelet restarts the old leader's container, which ended with 5; no other has ended.
	f.onlyEnded(restarts, old.Name)
	if now := f.leader(); now.ID() != next.ID() {
		t.Errorf("the leader is %s now; want %s, the one that took over: the old leader must not lead again", now.ID(), next.ID())
	}
	after, err := f.slack.Thread(f.ctx, ChannelS16, m.TS)
	if err != nil {
		t.Fatal(err)
	}
	was := map[string]fakeslack.MessageView{}
	for _, b := range before {
		was[b.TS] = b
	}
	changed := false
	for _, b := range after {
		switch prev, existed := was[b.TS]; {
		case !existed:
			t.Errorf("a message of the thread is new since the old leader was resumed: %q (%s); want none: a post after it was fenced\nthe thread: %s", b.Text, b.TS, describe(after))
		case prev.Text != b.Text || prev.Streaming != b.Streaming:
			changed = true
			t.Logf("the old leader's last moments changed %s: before %q (streaming: %v), after %q (streaming: %v)", b.TS, prev.Text, prev.Streaming, b.Text, b.Streaming)
		}
	}
	// A leader stopped only by losing its lease, with the thread unchanged, wrote nothing after the resume, so nothing of it was
	// refused and there is nothing to judge: the fences are proved by the store's tests (H4-5) and the chat fence's (H4-5b). One
	// that stopped so after changing the thread escaped the fences.
	if !fenced {
		if changed {
			t.Errorf("the old leader changed the thread after it was resumed and stopped only because its lease was lost (reasons %q); want a fence to refuse its writes", reasons)
		} else {
			t.Logf("the old leader stopped by losing its lease before it wrote anything (reasons %q): nothing to refuse, and the thread is unchanged", reasons)
		}
	}
	if whole, _ := split(after, ""); len(whole) > 1 {
		t.Errorf("the thread shows %d whole replies for one turn after the old leader was resumed; want one: a deposed leader's Slack writes are fenced (H4-5b)\nthe thread: %s", len(whole), describe(after))
	} else if changed {
		t.Logf("RESIDUAL RISK OBSERVED: the old leader changed a message of the thread after it was resumed (D37: each Slack call in flight when it was paused); still one whole reply")
	} else {
		t.Logf("no duplicate and no change: the thread is as the new leader left it")
	}
}
