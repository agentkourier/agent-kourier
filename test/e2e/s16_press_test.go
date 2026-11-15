//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// S16's press cases share S19's channel and Binding (spike-approve, with tool approvals): each run's mention starts a thread of its own,
// and everything the cases count is that thread's, or that interaction's, so they cannot be confused with S19's.
const (
	pressChannel = ChannelS19
	pressBinding = "agent-kourier/fast-s19"
)

// What spike-approve says once it is given the decision, and what the decided prompt shows of it, by the action pressed.
var pressVerdicts = map[chat.Action]struct{ verdict, continuation string }{
	chat.ActionApproveAll: {"Approved", "I have added 3 and 5. The result is 8."},
	chat.ActionDenyAll:    {"Denied", "Resumed after the tool result."},
}

// pressCase is an approval prompt that S16 presses across a failover, and what it counts before the leader is lost: the audit
// entries before the press, and two counters of each running process, since a counter starts from 0 with each process.
type pressCase struct {
	f       *failover
	mention fakeslack.UserMessage
	prompt  fakeslack.MessageView
	// paused is the bot's messages of the thread while the turn waits on the prompt, the prompt among them: the turn's live
	// message comes before the prompt, and is finished later with the request's summary.
	paused []fakeslack.MessageView
	audit  []store.AuditEntry
	// approvals and retries are agentkourier_approval_outcomes_total of the Binding (approved and denied together) and
	// agentkourier_slack_http_retries_total of the connection, by process (kube.Replica.ID).
	approvals, retries map[string]float64
}

// newPress posts a mention to the approval agent, as the first of people, all of whom it makes members of the channel so that
// each may decide, and returns once the prompt is in the thread, with the counts taken. It runs before the leader is lost.
func (f *failover) newPress(name string, people ...string) *pressCase {
	f.t.Helper()
	for _, u := range people {
		f.know(u)
	}
	if err := f.slack.SetMembers(f.ctx, pressChannel, people...); err != nil {
		f.t.Fatal(err)
	}
	p := &pressCase{f: f, audit: f.auditEntries(f.ctx), approvals: map[string]float64{}, retries: map[string]float64{}}
	all, err := f.reps.All(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, r := range all {
		p.approvals[r.ID()], p.retries[r.ID()] = p.counts(r)
	}
	p.mention = f.mention(pressChannel, people[0], "use the approval tool "+f.stub.Gate(name))
	th := f.thread(pressChannel, p.mention.TS, "the tool approval prompt", func(th []fakeslack.MessageView) bool {
		_, ok := ApprovalPrompt(th)
		return ok
	})
	p.prompt, _ = ApprovalPrompt(th)
	p.paused = BotMessages(th)
	err = Consistently(f.ctx, 3*time.Second, "the paused turn posts nothing after its prompt", func(ctx context.Context) (bool, error) {
		th, err := f.slack.Thread(ctx, pressChannel, p.mention.TS)
		return len(BotMessages(th)) == len(p.paused), err
	})
	if err != nil {
		th, _ := f.slack.Thread(context.WithoutCancel(f.ctx), pressChannel, p.mention.TS)
		f.t.Fatalf("%v\nthe thread: %s", err, describe(th))
	}
	return p
}

// promptOf is the prompt as the thread shows it now, or false when it is not there.
func (p *pressCase) promptOf(th []fakeslack.MessageView) (fakeslack.MessageView, bool) {
	for _, b := range BotMessages(th) {
		if b.TS == p.prompt.TS {
			return b, true
		}
	}
	return fakeslack.MessageView{}, false
}

// counts reads the two counters of pressCase from the process r, each 0 while it has no sample.
func (p *pressCase) counts(r kube.Replica) (approvals, retries float64) {
	p.f.t.Helper()
	read := func(name string, labels map[string]string) float64 {
		v, err := p.f.reps.Counter(p.f.ctx, r, name, labels)
		if err != nil && !errors.Is(err, ErrNoSample) {
			p.f.t.Fatal(err)
		}
		return v
	}
	for _, outcome := range []string{"approved", "denied"} {
		approvals += read(approvalMetric, map[string]string{"binding": pressBinding, "outcome": outcome})
	}
	return approvals, read("agentkourier_slack_http_retries_total", map[string]string{"connection": ConnPlatform})
}

// press presses the prompt's control for action as user, once, and returns the envelope (in HTTP mode, the fake's ID of the request
// and its retries), or "" when the fake Slack had no connection of the app to send it on, as in Socket Mode while nobody leads. It
// presses the prompt as it was posted (PressStaleApproval): an earlier press, retried, may have decided it and taken its controls off
// by now, which a person's open Slack has not shown yet.
func (p *pressCase) press(user string, action chat.Action) string {
	p.f.t.Helper()
	id, err := p.f.slack.PressStaleApproval(p.f.ctx, user, p.prompt, action)
	if se := (*StatusError)(nil); errors.As(err, &se) && se.Status == http.StatusConflict && p.f.env.SlackMode == SlackModeSocket {
		return ""
	}
	if err != nil {
		p.f.t.Fatalf("press %s as %s: %v", action, user, err)
	}
	return id
}

// delivered returns once a press of user's for action is acked, given the envelope of the press made (or "" for one that went
// nowhere). In HTTP mode the fake Slack sends a request again until it is answered 200, 1, 5 and 30 seconds after an attempt that was
// not (fakeslack.DefaultHTTPRetries), which spans a takeover: the one press must be acked, by whichever process leads by then, and is
// never made twice. In Socket Mode the fake never sends an envelope again (UNCONFIRMED(WP-04) in fakeslack: Slack may), and has none
// to send while nobody leads, so the person presses again, as one whose press went unanswered does, until a press is acked.
func (p *pressCase) delivered(id, user string, action chat.Action) {
	p.f.t.Helper()
	if p.f.env.SlackMode == SlackModeHTTP {
		if _, err := p.f.slack.AckWait(p.f.ctx, 2*time.Minute, id); err != nil {
			p.f.t.Fatalf("the fake Slack's retries of %s's press never reached a leader: %v", user, err)
		}
		return
	}
	presses := 1
	err := Wait(p.f.ctx, 2*time.Minute, fmt.Sprintf("a press of %s's to be acked", user), func(ctx context.Context) (bool, error) {
		if id != "" {
			if _, err := p.f.slack.AckWait(ctx, 10*time.Second, id); err == nil {
				return true, nil
			}
		}
		id, presses = p.press(user, action), presses+1
		return false, nil
	})
	if err != nil {
		p.f.t.Fatal(err)
	}
	p.f.t.Logf("a press of %s's was acked, after %d presses in all", user, presses)
}

// decidedOnce checks that the prompt was decided once, by one of the people who pressed (pressed, with the action each pressed), on
// the process next, which leads now:
//   - the prompt shows that person and their verdict, and no controls;
//   - the thread holds the bot's messages of the paused turn, each finished and none of them an answer, and after them
//     spike-approve's one continuation for that verdict, and no other message of the bot;
//   - the audit has one answered decision of the interaction, by that person, and any other decision of it by those who pressed is
//     a loser's, superseded (decisions by anyone else, such as the timeout of a prompt an earlier run left, are not this case's);
//   - the agent was sent the answer once, and next counted one approval outcome;
//   - in HTTP mode next answered a retry, so the press it decided was one the fake sent again.
//
// It returns who won.
func (p *pressCase) decidedOnce(next kube.Replica, pressed map[string]chat.Action) string {
	f := p.f
	f.t.Helper()
	answers := func(text string) bool {
		for _, v := range pressVerdicts {
			if strings.Contains(text, v.continuation) {
				return true
			}
		}
		return false
	}
	// decided returns the winner, or why the thread does not show one decision yet.
	decided := func(th []fakeslack.MessageView) (winner, why string) {
		bots := BotMessages(th)
		if len(bots) != len(p.paused)+1 {
			return "", fmt.Sprintf("%d messages of the bot; want the %d of the paused turn and a continuation", len(bots), len(p.paused))
		}
		for i, was := range p.paused { // the same messages, and none of them is an answer of the agent
			if bots[i].TS != was.TS || !finished(bots[i]) || bots[i].TS != p.prompt.TS && answers(bots[i].Text) {
				return "", fmt.Sprintf("the bot's message %d is %s %q (streaming: %v); it was %s %q", i, bots[i].TS, bots[i].Text, bots[i].Streaming, was.TS, was.Text)
			}
		}
		pr, _ := p.promptOf(th)
		cont := bots[len(p.paused)]
		if pr.Edited == nil || pr.Text != "Answered" || HasButton(pr.Blocks, chat.ActionApproveAll.String()) || HasButton(pr.Blocks, chat.ActionDenyAll.String()) {
			return "", fmt.Sprintf("the prompt is not decided: %q (edited: %v) %s", pr.Text, pr.Edited != nil, pr.Blocks)
		}
		for user, action := range pressed {
			v := pressVerdicts[action]
			if HasBlockText(pr.Blocks, "<@"+user+">") && strings.Contains(string(pr.Blocks), v.verdict) && finished(cont) && cont.Text == v.continuation {
				if winner != "" {
					return "", fmt.Sprintf("the prompt names %s and %s: %s", winner, user, pr.Blocks)
				}
				winner = user
			}
		}
		if winner == "" {
			return "", fmt.Sprintf("no one who pressed is both the prompt's decider and the verdict of the continuation %q (streaming: %v): %s", cont.Text, cont.Streaming, pr.Blocks)
		}
		return winner, ""
	}
	var winner, why string
	err := Wait(f.ctx, stepTimeout, "the prompt decided once, and the agent's one continuation", func(ctx context.Context) (bool, error) {
		th, err := f.slack.Thread(ctx, pressChannel, p.mention.TS)
		winner, why = decided(th)
		return why == "", err
	})
	if err == nil {
		err = Consistently(f.ctx, 5*time.Second, "the thread keeps the one decided prompt and the one continuation", func(ctx context.Context) (bool, error) {
			th, err := f.slack.Thread(ctx, pressChannel, p.mention.TS)
			w, now := decided(th)
			why = now
			return now == "" && w == winner, err
		})
	}
	if err != nil {
		th, _ := f.slack.Thread(context.WithoutCancel(f.ctx), pressChannel, p.mention.TS)
		f.t.Fatalf("%v: %s\nthe thread: %s", err, why, describe(th))
	}

	// The audit: the decisions of this Binding by those who pressed, since the prompt, are all of the one interaction, one of them answered.
	var rows []store.AuditEntry
	for _, r := range approvalAuditAfter(f.auditEntries(f.ctx), p.audit, pressBinding) {
		if _, ok := pressed[r.User]; ok {
			rows = append(rows, r)
		}
	}
	var interaction string
	answered := 0
	for _, r := range rows {
		id, _, _ := strings.Cut(strings.TrimPrefix(r.Subject, "interaction="), " task=")
		if interaction == "" {
			interaction = id
		}
		switch {
		case id != interaction:
			f.t.Errorf("decisions of two interactions since the prompt: %q and %q; want one", interaction, id)
		case strings.HasPrefix(r.Outcome, "answered"):
			answered++
			if r.User != winner {
				f.t.Errorf("the answered decision is %s's; the prompt shows %s's", r.User, winner)
			}
		case r.Outcome == "superseded" && r.User != winner:
		default:
			f.t.Errorf("a decision of %s by %s is %q; want the winner's one answered and only the others' superseded", id, r.User, r.Outcome)
		}
	}
	if answered != 1 {
		f.t.Errorf("the audit has %d answered decisions of the prompt; want 1\nthe rows: %+v", answered, rows)
	}

	// The agent: the answer's message ID is the interaction's, with a suffix, and the stub counts each send of it, a repeated one too.
	if interaction != "" {
		state, err := f.stub.SendGate(f.ctx)
		if err != nil {
			f.t.Fatal(err)
		}
		sends := 0
		for id, n := range state.Sends {
			if strings.HasPrefix(id, interaction) {
				sends += n
			}
		}
		if sends != 1 {
			f.t.Errorf("the agent was sent the answer to %s %d times; want once", interaction, sends)
		}
	}

	approvals, retries := p.counts(next)
	if got := approvals - p.approvals[next.ID()]; got != 1 { // a process that did not run before counts from 0
		f.t.Errorf("the new leader %s counts %v approval outcomes for %s since the prompt; want 1", next.ID(), got, pressBinding)
	}
	if got := retries - p.retries[next.ID()]; f.env.SlackMode == SlackModeHTTP && got < 1 {
		f.t.Errorf("the new leader %s answered %v requests that the fake Slack sent again; want at least 1: the press it decided was the dead leader's, retried", next.ID(), got)
	}
	return winner
}

// s16KillWithPressInFlight (case 5): the leader posted an approval prompt and is killed, and the approver presses at once, while nobody
// leads. In HTTP mode that request finds no leader behind the chat Service, and the fake Slack sends it again until the process that
// takes over answers it; in Socket Mode the fake has no connection to send it on, or sends it on the dead one, and the approver
// presses again. The new leader decides the press once, from the prompt the dead leader posted, the agent is sent the answer once
// and goes on, and nothing in the thread is lost or twice.
func s16KillWithPressInFlight(t *testing.T) {
	f := newFailover(t)
	const approver = "US16E"
	p := f.newPress("S16e", approver)
	old := f.leader()
	before := f.restarts()

	if err := f.sig.Kill(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	id := p.press(approver, chat.ActionApproveAll)
	next := f.takeover(old)
	p.delivered(id, approver, chat.ActionApproveAll)
	p.decidedOnce(next, map[string]chat.Action{approver: chat.ActionApproveAll})
	f.settle()
	f.onlyEnded(before, old.Name)
}

// s16StallWithRacingPresses (case 6): two people press one approval prompt across a takeover. The leader that posted it is frozen
// (SIGSTOP), and the first approves at once: the press goes to the frozen process, on its Socket Mode connection or, in HTTP mode, to
// the one pod the chat Service selects, which does not answer, so the fake Slack sends it again. Once another process leads, the second
// denies. Then the old leader is resumed with whatever it was sent while frozen: it must end, as a deposed leader does (exit 5), and
// decide nothing. Whichever press the new leader records first wins (in Socket Mode the second, since the first never reaches it; in
// HTTP mode either, by the timing of the retries, and the second may press a prompt that the first's retry has decided already): one
// decision, the winner's, in the prompt, the audit and the agent's one answer, and the other press, if it is decided at all, is
// superseded.
//
// In HTTP mode both presses reach the new leader, the first by the fake's retries, which do not lose it: both are acked, and the
// loser is told who decided first, in an ephemeral notice in the thread. That notice is what shows the loser's press was taken and
// processed, since a press on a prompt already decided is told and not audited. In Socket Mode the first press went to the frozen
// leader's connection, the fake never sends an envelope again (UNCONFIRMED(WP-04) in fakeslack), and the resumed leader is fenced
// before it can record it: nobody is told, and no notice is expected.
func s16StallWithRacingPresses(t *testing.T) {
	f := newFailover(t)
	const first, second = "US16F1", "US16F2"
	p := f.newPress("S16f", first, second)
	old := f.leader()
	restarts := f.restarts()

	t.Cleanup(func() { // a container left frozen would hold the tier until the kubelet's liveness probe killed it
		_ = f.sig.Cont(context.WithoutCancel(f.ctx), old.ContainerID)
	})
	frozen := time.Now()
	if err := f.sig.Stop(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	early := p.press(first, chat.ActionApproveAll)
	if early == "" {
		t.Fatal("the fake Slack had no connection for the first press: the frozen leader's Socket Mode connection stays open, and the press goes to it")
	}
	next := f.takeover(old)
	late := p.press(second, chat.ActionDenyAll)
	p.delivered(late, second, chat.ActionDenyAll)

	// Resumed once the new leader has decided, and before the kubelet's liveness restart of the frozen container (50 to 60 s).
	f.thread(pressChannel, p.mention.TS, "the prompt decided by the new leader while the old one is frozen", func(th []fakeslack.MessageView) bool {
		pr, ok := p.promptOf(th)
		return ok && pr.Edited != nil && pr.Text == "Answered" &&
			!HasButton(pr.Blocks, chat.ActionApproveAll.String()) && !HasButton(pr.Blocks, chat.ActionDenyAll.String())
	})
	asleep := time.Since(frozen)
	if err := f.sig.Cont(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	exit, err := f.pods.WaitExit(f.ctx, kube.AgentKourierNamespace, old.Name, agentKourierContainer, old.ContainerID)
	if err != nil {
		t.Fatalf("the old leader, resumed after %s, did not end: %v", asleep.Round(time.Second), err)
	}
	if exit.ExitCode != exitLeadershipLost {
		t.Fatalf("the old leader ended with exit code %d (%s) after %s frozen; want %d, a lost leadership", exit.ExitCode, exit.Reason, asleep.Round(time.Second), exitLeadershipLost)
	}
	if logs, err := f.pods.RunLogs(f.ctx, kube.AgentKourierNamespace, old.Name, agentKourierContainer, old.ContainerID, 400); err == nil {
		t.Logf("the old leader was frozen for %s, resumed, and ended with exit code %d; its log says: %q", asleep.Round(time.Second), exit.ExitCode, ExitReasons(logs))
	}
	if f.env.SlackMode == SlackModeHTTP {
		if _, err := f.slack.AckWait(f.ctx, 2*time.Minute, early); err != nil {
			t.Errorf("the first press, sent to the frozen leader, was never answered 200 by a leader: the fake's retries lost it: %v", err)
		}
	}

	f.settle()
	f.onlyEnded(restarts, old.Name)
	if now := f.leader(); now.ID() != next.ID() {
		t.Errorf("the leader is %s now; want %s, the one that took over: the old leader must not lead again", now.ID(), next.ID())
	}
	winner := p.decidedOnce(next, map[string]chat.Action{first: chat.ActionApproveAll, second: chat.ActionDenyAll})
	t.Logf("%s's press won (%s); pressed: %s approve, to the frozen leader, then %s deny, after the takeover", winner, f.env.SlackMode, first, second)
	if f.env.SlackMode == SlackModeSocket && winner != second {
		t.Errorf("in Socket Mode %s's press won; want %s's: the first press went to the frozen leader's connection and only the resumed old leader could read it", winner, second)
	}
	if f.env.SlackMode == SlackModeHTTP {
		loser := first
		if winner == first {
			loser = second
		}
		p.toldLost(loser, winner)
	}
}

// toldLost waits for the ephemeral notice that tells loser, in the case's thread, that winner decided first, and checks that there
// is one. Press intake records each request it acknowledges (store.Presses has no key that a retry repeats), so a retry of a press
// whose 200 was lost on the way would be recorded and told twice: the fake sees the request unanswered and the broker saw it
// twice. That is not a duplicated decision, so more than one notice is logged rather than failed.
func (p *pressCase) toldLost(loser, winner string) {
	f := p.f
	f.t.Helper()
	const lost = "decided this request first."
	waitFullEphemeralWithWinner(f.t, f.scenario, pressChannel, p.mention.TS, loser, winner, lost)
	notices, err := f.slack.Ephemerals(f.ctx, pressChannel)
	if err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, e := range notices {
		if e.Recipient == loser && e.ThreadTS == p.mention.TS && strings.Contains(e.Text, lost) && HasBlockText(e.Blocks, "<@"+winner+">") {
			n++
		}
	}
	if n > 1 {
		f.t.Logf("%s was told %d times that %s decided first: a press of theirs was delivered more than once", loser, n, winner)
	}
}
