//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// The scenarios that wait for MC-5, the failure isolation of a team connection. They are written and skip until the change is in the broker. To turn them on,
// delete the skip constants and the t.Skip line of each test, and run them (E2E_RUN='TestMC_(ARejectedToken|ALiveReply)') on the tier of the other MC_ tests.
const (
	// mcWaitsForMC5 is the reason of the case about a token that Slack rejects: today a rejected token ends the connection's run and nothing else is
	// told, so no Binding says so and the series stays.
	mcWaitsForMC5 = "waits for the broker to stop only a team connection whose token Slack rejects (MC-5): remove this skip, and mcWaitsForMC5, once it is in"
	// mcWaitsForRecovery is the reason of the case about a turn recovered before its team connection runs: its live writes fail for as long as the renderer
	// tries, and are given up on, after the connection runs. MC-5 is to make Recover leave the turns of a connection that is not running until it runs.
	mcWaitsForRecovery = "waits for MC-5: a live reply of a turn that a new leader recovers before the team's connection runs is never finished (the renderer's writes fail with 'no platform is running the connection' for ten seconds, after the connection is up, and are given up on); remove this skip once Recover leaves the turn until the connection runs"
)

// fixTeamA does what an operator does after Slack rejected team-a's tokens: it gets a new pair for the app (the fake rotates it: the old pair is refused for
// good, so a pair that the connection was started with cannot work again) and puts it in team-a's Secret. The pair is new each time, so that the Secret
// differs from what the connection last ran with, which is what starts a connection whose credentials were rejected: the supervisor's fingerprint of a
// connection holds the values of the keys it references, not the Secret's version.
func (m *mc) fixTeamA(team *kube.CRs) error {
	ctx := context.WithoutCancel(m.ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	bot, app := "xoxb-fake-team-a-"+suffix, "xapp-fake-team-a-"+suffix
	if err := m.slack.RotateTokens(ctx, AppTeamA.AppID, bot, app); err != nil {
		return err
	}
	if err := m.slack.SetRevoked(ctx, AppTeamA.AppID, false); err != nil {
		return err
	}
	return team.SetSecretData(ctx, "slack-team-a", map[string]string{"botToken": bot, "appToken": app})
}

// restoreTeamA puts the pair of tokens that the tier was brought up with back, in the fake (rotating to a pair the app held before brings it back) and in
// team-a's Secret, which restarts the connection once more.
func (m *mc) restoreTeamA(team *kube.CRs) error {
	ctx := context.WithoutCancel(m.ctx)
	if err := m.slack.SetRevoked(ctx, AppTeamA.AppID, false); err != nil { // idempotent: a case that ended before the fix left the app revoked
		return err
	}
	if err := m.slack.RotateTokens(ctx, AppTeamA.AppID, AppTeamA.BotToken, AppTeamA.AppToken); err != nil {
		return err
	}
	return team.SetSecretData(ctx, "slack-team-a", map[string]string{"botToken": AppTeamA.BotToken, "appToken": AppTeamA.AppToken})
}

// TestMC_ARejectedTokenStopsOnlyThatConnection: Slack revokes team-a's tokens. The connection stops and says so, and nothing else does: its
// agentkourier_chat_connected is 0, its Binding's ChannelJoined is False with the reason ChatCredentialsRejected, the platform's connection stays up and its
// Binding keeps answering through the platform's app, and the broker is not restarted. When the team fixes its Secret the connection starts again by itself:
// the series is 1, the app has its client, the Binding is ready and answers through team-a's app.
//
// "Fixed" is what a team does: the app gets new tokens (the fake rotates them, fixTeamA) and the team puts them in its Secret. The pair then in the Secret
// is not the one the tier was brought up with; the case puts the first pair back at its end.
func TestMC_ARejectedTokenStopsOnlyThatConnection(t *testing.T) {
	t.Skip(mcWaitsForMC5)
	m := newMC(t)
	before := m.restarts()
	team, err := m.crs.In(kube.MultiTeamNamespace)
	if err != nil {
		t.Fatal(err)
	}
	fixed := false
	t.Cleanup(func() { // the tier is left as it was brought up: the pair of tokens the other cases use (ChannelMCDuplicate's connection holds it), and a running connection
		if !fixed { // a case that ended before the fix: the connection stays down until its Secret changes
			if err := m.fixTeamA(team); err != nil {
				t.Errorf("fix team-a at the end: %v", err)
			}
		}
		if err := m.restoreTeamA(team); err != nil {
			t.Errorf("put team-a's first tokens back at the end: %v", err)
		}
	})

	if err := m.slack.SetRevoked(m.ctx, AppTeamA.AppID, true); err != nil {
		t.Fatal(err)
	}
	err = Wait(m.ctx, 2*time.Minute, "agentkourier_chat_connected{connection="+ConnTeamA+"} to be 0", func(ctx context.Context) (bool, error) {
		v, err := m.leaderCounter(ctx, "agentkourier_chat_connected", map[string]string{"connection": ConnTeamA})
		if errors.Is(err, ErrNoSample) {
			return false, nil
		}
		return v == 0, err
	})
	if err != nil {
		t.Fatal(err)
	}
	m.waitCondition(kube.MultiTeamNamespace, "team-a", "ChannelJoined", "False", "ChatCredentialsRejected")

	// Everything else goes on: the platform's connection, its Binding, and the process.
	m.postedBy(m.answered(AppPlatform, ChannelMCPlatform, crServeTimeout), AppPlatform)
	m.waitUp(ConnPlatform)
	m.waitPlatformBindingReady()
	if after := m.restarts(); after != before {
		t.Errorf("the replicas were %s before the revocation and %s after: a team's rejected token must not restart the broker", before, after)
	}

	// The team fixes its Secret, with new tokens, and the app works again.
	fixed = true
	if err := m.fixTeamA(team); err != nil {
		t.Fatal(err)
	}
	m.waitApp(AppTeamA, 3*time.Minute)
	m.waitUp(ConnTeamA)
	m.waitCondition(kube.MultiTeamNamespace, "team-a", "ChannelJoined", "True", "")
	m.waitCondition(kube.MultiTeamNamespace, "team-a", "Ready", "True", "")
	m.postedBy(m.answered(AppTeamA, ChannelMCTeamA, crServeTimeout), AppTeamA)
	if after := m.restarts(); after != before {
		t.Errorf("the replicas were %s before and %s after the fix: the broker restarted", before, after)
	}
}

// TestMC_ALiveReplyOfARecoveredTeamTurnIsFinished: a turn is held on a gate in each connection's channel, with live output (a stream), and the leader is killed.
// The replica that takes over recovers both turns, and each thread ends with the one whole reply, posted by the bot of its app. Team-a's connection is started
// after the leader has begun to recover, so the turn's first writes find no platform for it: the reply must still end.
//
// The fast tier on Postgres with two replicas (see TestMC_FailoverServesBothConnections). On main before MC-5 the platform's thread ends and team-a's stays
// "Holding on gate G." streaming: the log says 'live: the turn's connection is not running; its writes fail until it is', and the writes go on failing with
// 'chat: no platform is running the connection' after the connection is serving, until the renderer gives up on the message.
func TestMC_ALiveReplyOfARecoveredTeamTurnIsFinished(t *testing.T) {
	t.Skip(mcWaitsForRecovery)
	m, f := newMCFailover(t)
	type turn struct {
		app     FakeApp
		channel string
		binding string
		gate    string
		root    fakeslack.UserMessage
	}
	turns := []*turn{
		{app: AppPlatform, channel: ChannelMCPlatform, binding: "agent-kourier/mc-platform"},
		{app: AppTeamA, channel: ChannelMCTeamA, binding: "team-a/team-a"},
	}
	old := f.leader()
	before := f.restarts()
	for i, tn := range turns {
		tn.gate = f.gate(fmt.Sprintf("MC-live-%d", i))
		um, err := m.say(tn.app, tn.channel, "UMCLIVE", "answer me gate:"+tn.gate)
		if err != nil {
			t.Fatal(err)
		}
		tn.root = um
		f.thread(tn.channel, um.TS, "the first chunk of "+tn.app.AppID+"'s reply, while its task is held", held(tn.gate, "", true))
	}
	if err := f.sig.Kill(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	next := f.takeover(old)
	for _, tn := range turns {
		if err := f.stub.Release(f.ctx, tn.gate); err != nil {
			t.Fatal(err)
		}
	}
	for _, tn := range turns {
		m.postedBy(f.thread(tn.channel, tn.root.TS, "the reply of "+tn.app.AppID+" to end, once, after the takeover", answered(tn.gate, "")), tn.app)
		if got := f.completedTurns(next, tn.binding); got != 1 {
			t.Errorf("the new leader counts %v completed turns for %s; want 1", got, tn.binding)
		}
	}
	f.settle()
	f.onlyEnded(before, old.Name)
}
