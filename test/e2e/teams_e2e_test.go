//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/faketeams"
)

// The TEAMS_ tests: the broker's Microsoft Teams connections in http mode against the fake Teams service, on the fast tier brought up with E2E_TEAMS=1
// (make e2e-teams runs them; make e2e-fast does not). They skip unless the broker admits the namespace teams-e2e. The fake keeps its messages in memory
// and the broker's store outlives it, so each test makes its texts and alert names unique to the run.

// The people of the team, by Microsoft Entra object ID.
const (
	teamsAda = "7e2e7ea5-0000-4000-8000-0000000000d1"
	teamsBob = "7e2e7ea5-0000-4000-8000-0000000000d2"
)

// teamsScenario is a scenario with the fake Teams service's control API.
type teamsScenario struct {
	*scenario
	teams *faketeams.Control
}

// requireTeams skips unless the broker admits teams-e2e, which E2E_TEAMS=1 sets up.
func requireTeams(t *testing.T, s *scenario) {
	t.Helper()
	admits, err := s.env.Admits(TeamsNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if !admits {
		t.Skipf("the broker does not admit %s (%s): bring the fast tier up with E2E_TEAMS=1 E2E_CONFIG_SOURCE=crds", TeamsNamespace, s.env.Admission())
	}
}

// newTeams returns what a TEAMS_ test needs, once both Teams connections are up and each bound channel is known to the broker.
func newTeams(t *testing.T) *teamsScenario {
	t.Helper()
	s := newScenario(t)
	requireTeams(t, s)
	ts := &teamsScenario{scenario: s, teams: faketeams.NewControl(s.env.TeamsURL)}
	ts.prepare()
	return ts
}

// prepare waits for the leader to say both Teams connections are up, and adds each bot to its channels again: the fake keeps nothing over a restart,
// and the conversationUpdate is what tells a connection a channel's team and group, which its subscriptions and polls need. A 200 from the bot is the
// broker serving the connection on its chat listener; anything else is tried again for a while (a rollout, a takeover).
func (ts *teamsScenario) prepare() {
	ts.t.Helper()
	if err := ts.teams.Healthy(ts.ctx); err != nil {
		ts.t.Fatalf("the fake Teams service: %v", err)
	}
	for _, conn := range []string{ConnTeamsPush, ConnTeamsPoll} {
		ts.waitUp(conn)
	}
	for _, channel := range slices.Sorted(maps.Keys(teamsChannels)) {
		app := teamsChannels[channel]
		err := Wait(ts.ctx, 2*time.Minute, "the bot "+app+" to take its install in "+channel, func(ctx context.Context) (bool, error) {
			d, err := ts.teams.Install(ctx, faketeams.Install{App: app, Channel: channel})
			return err == nil && d.Status == 200, nil
		})
		if err != nil {
			ts.t.Fatal(err)
		}
	}
}

// waitUp waits until the leader says the connection is up: agentkourier_chat_connected is 1 for it.
func (ts *teamsScenario) waitUp(conn string) {
	ts.t.Helper()
	err := Wait(ts.ctx, 3*time.Minute, "agentkourier_chat_connected{connection="+conn+"} to be 1", func(ctx context.Context) (bool, error) {
		v, err := ts.leaderCounter(ctx, "agentkourier_chat_connected", map[string]string{"connection": conn})
		if errors.Is(err, ErrNoSample) {
			return false, nil
		}
		return v == 1, err
	})
	if err != nil {
		ts.t.Fatal(err)
	}
}

// teamsUnique is a name no earlier run used: the channels, the stub and the broker's store outlive a run.
func teamsUnique(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%1_000_000_000)
}

// mention posts ada's mention of the channel's bot with text, and returns the post's ID once the bot has taken its activity with a 200.
func (ts *teamsScenario) mention(channel, text string) string {
	ts.t.Helper()
	d, err := ts.teams.Mention(ts.ctx, faketeams.Mention{App: teamsChannels[channel], Channel: channel, User: teamsAda, Name: "Ada", Text: text})
	if err != nil {
		ts.t.Fatal(err)
	}
	if d.Status != 200 {
		ts.t.Fatalf("the bot answered the mention's activity %d %q; want 200", d.Status, d.Body)
	}
	return d.ID
}

// alert posts Workflows' card for a firing alert named name into channel, and returns the post's ID.
func (ts *teamsScenario) alert(channel, name string) string {
	ts.t.Helper()
	card, err := json.Marshal(map[string]any{"type": "AdaptiveCard", "version": "1.4", "body": []any{
		map[string]any{"type": "TextBlock", "text": "[FIRING] " + name + " shop", "weight": "bolder"},
		map[string]any{"type": "FactSet", "facts": []any{map[string]string{"title": "namespace", "value": "shop"}}},
	}})
	if err != nil {
		ts.t.Fatal(err)
	}
	id, err := ts.teams.Workflows(ts.ctx, faketeams.Workflows{App: TeamsWorkflows, Channel: channel, Card: card})
	if err != nil {
		ts.t.Fatal(err)
	}
	return id
}

// thread waits until pred holds of the bot's messages in the thread of root, and returns them.
func (ts *teamsScenario) thread(channel, root, desc string, within time.Duration, pred func([]faketeams.MessageView) bool) []faketeams.MessageView {
	ts.t.Helper()
	var bots []faketeams.MessageView
	err := Wait(ts.ctx, within, desc, func(ctx context.Context) (bool, error) {
		th, err := ts.teams.Messages(ctx, channel, root)
		if err != nil {
			return false, err
		}
		bots = teamsBotMessages(th, teamsChannels[channel])
		return pred(bots), nil
	})
	if err != nil {
		ts.t.Fatalf("%v; the bot's messages: %s", err, describeTeams(bots))
	}
	return bots
}

// press presses the action titled action of the message as user, checks that the bot answered the invoke with a 200 and a text that Teams shows the
// presser, and returns that text: what the press came to, or that it was received when it was not decided in time.
func (ts *teamsScenario) press(channel, message, user, action string) string {
	ts.t.Helper()
	d, err := ts.teams.Press(ts.ctx, faketeams.Press{App: teamsChannels[channel], Message: message, User: user, Name: "Person " + user[len(user)-2:], Action: action})
	if err != nil {
		ts.t.Fatal(err)
	}
	var answer struct {
		StatusCode int    `json:"statusCode"`
		Type       string `json:"type"`
		Value      string `json:"value"`
	}
	if d.Status != 200 || json.Unmarshal([]byte(d.Body), &answer) != nil || answer.StatusCode != 200 ||
		answer.Type != "application/vnd.microsoft.activity.message" || answer.Value == "" {
		ts.t.Fatalf("the bot answered the press of %s %d %q; want 200 with a text for the presser", action, d.Status, d.Body)
	}
	return answer.Value
}

// subscribed waits until each of channels has exactly one live subscription, of the bot of its connection, and returns them by channel.
func (ts *teamsScenario) subscribed(channels ...string) map[string]faketeams.SubscriptionView {
	ts.t.Helper()
	var subs []faketeams.SubscriptionView
	out := map[string]faketeams.SubscriptionView{}
	err := Wait(ts.ctx, 3*time.Minute, fmt.Sprintf("one subscription to each of %q", channels), func(ctx context.Context) (bool, error) {
		var err error
		if subs, err = ts.teams.Subscriptions(ctx); err != nil {
			return false, err
		}
		clear(out)
		n := map[string]int{}
		for _, sub := range subs {
			n[sub.Channel]++
			out[sub.Channel] = sub
		}
		for _, c := range channels {
			if n[c] != 1 || out[c].App != teamsChannels[c] {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		ts.t.Fatalf("%v; live: %+v", err, subs)
	}
	return out
}

// stats reads what the fake counted.
func (ts *teamsScenario) stats() faketeams.Stats {
	ts.t.Helper()
	st, err := ts.teams.Stats(ts.ctx)
	if err != nil {
		ts.t.Fatal(err)
	}
	return st
}

// investigated waits for the bot's investigation of the alert in its thread: the final text, and the feedback buttons, which the Teams adapter posts as a
// card of their own after the text. It returns the message with the buttons.
func (ts *teamsScenario) investigated(channel, alert, name string) faketeams.MessageView {
	ts.t.Helper()
	bots := ts.thread(channel, alert, "the investigation of "+name+" with its feedback buttons", 2*time.Minute, func(bots []faketeams.MessageView) bool {
		_, text := teamsWithText(bots, "Investigating: An alert is FIRING")
		_, buttons := teamsWithAction(bots, "Useful")
		return text && buttons
	})
	if m, _ := teamsWithText(bots, "Investigating:"); !strings.Contains(m.Text, name) {
		ts.t.Errorf("the investigation says %q; want the alert's name %s, which the trigger extracted from the card's text", m.Text, name)
	}
	buttons, _ := teamsWithAction(bots, "Useful")
	return buttons
}

// describeTeams is the bot's messages, for a failure.
func describeTeams(msgs []faketeams.MessageView) string {
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "\n  %s (root %s, %d edits, %d cards): %q", m.ID, m.Root, m.Edits, len(m.Cards), m.Text)
	}
	return b.String()
}

// TestTEAMS_AMentionIsAnsweredInItsThread: a person's mention of the bot starts a turn of the channel's Binding, and the agent's answer is one reply in the
// mention's thread, posted through the Bot Connector at the service URL the activity named, which is the fake's.
func TestTEAMS_AMentionIsAnsweredInItsThread(t *testing.T) {
	ts := newTeams(t)
	root := ts.mention(ChannelTeamsMention, teamsUnique("hello "))
	bots := ts.thread(ChannelTeamsMention, root, "the answer to the mention", stepTimeout, func(bots []faketeams.MessageView) bool {
		_, ok := teamsWithText(bots, "No gate named, not holding.")
		return ok
	})
	if len(bots) != 1 || bots[0].Root != root {
		t.Errorf("the bot posted %s; want one reply in the mention's thread", describeTeams(bots))
	}
	err := Consistently(ts.ctx, 5*time.Second, "one reply to the mention", func(ctx context.Context) (bool, error) {
		th, err := ts.teams.Messages(ctx, ChannelTeamsMention, root)
		return len(teamsBotMessages(th, TeamsAppPush)) == 1, err
	})
	if err != nil {
		t.Error(err)
	}
}

// TestTEAMS_ALiveReplyIsEditedInPlace: with output live, the reply is posted as the stream begins and edited as it goes on: the turn holds on a gate after
// its first words, which are in the thread then, and its last words come by an edit of the same message, not a second one.
func TestTEAMS_ALiveReplyIsEditedInPlace(t *testing.T) {
	ts := newTeams(t)
	gate := ts.stub.Gate("teams-live")
	t.Cleanup(func() { _ = ts.stub.Release(context.WithoutCancel(ts.ctx), gate) })
	root := ts.mention(ChannelTeamsMention, "hold on gate:"+gate)
	held := ts.thread(ChannelTeamsMention, root, "the reply's first words, while the turn holds", stepTimeout, func(bots []faketeams.MessageView) bool {
		_, ok := teamsWithText(bots, "Holding on gate "+gate)
		return ok
	})
	if m, _ := teamsWithText(held, "Holding on gate"); strings.Contains(m.Text, "Released.") {
		t.Fatalf("the reply says %q while its turn holds; want its first words alone", m.Text)
	}
	first, _ := teamsWithText(held, "Holding on gate")
	if err := ts.stub.Release(ts.ctx, gate); err != nil {
		t.Fatal(err)
	}
	done := ts.thread(ChannelTeamsMention, root, "the reply edited to its last words", stepTimeout, func(bots []faketeams.MessageView) bool {
		m, ok := teamsWithText(bots, "Released.")
		return ok && m.ID == first.ID
	})
	last, _ := teamsWithText(done, "Released.")
	if len(done) != 1 || last.Edits < 1 || strings.Contains(last.Versions[0], "Released.") {
		t.Errorf("the bot posted %s, versions %q; want one reply, edited at least once, whose first version had not the last words", describeTeams(done), last.Versions)
	}
}

// TestTEAMS_AToolApprovalIsPressedOnItsCard: the agent asks to run a tool, which the broker posts as a card with Approve and Deny; a member's press of
// Approve is answered in the invoke, the card is replaced by one with no buttons, and the turn goes on with the tool's result.
func TestTEAMS_AToolApprovalIsPressedOnItsCard(t *testing.T) {
	ts := newTeams(t)
	root := ts.mention(ChannelTeamsApproval, teamsUnique("add three and five "))
	bots := ts.thread(ChannelTeamsApproval, root, "the approval card", stepTimeout, func(bots []faketeams.MessageView) bool {
		_, ok := teamsWithAction(bots, "Approve")
		return ok
	})
	card, _ := teamsWithAction(bots, "Approve")
	if _, ok := teamsWithAction([]faketeams.MessageView{card}, "Deny"); !ok {
		t.Errorf("the approval card has no Deny: %s", card.Cards)
	}
	ts.press(ChannelTeamsApproval, card.ID, teamsAda, "Approve")
	ts.thread(ChannelTeamsApproval, root, "the turn to go on after the approval, and the card to lose its buttons", stepTimeout, func(bots []faketeams.MessageView) bool {
		_, answered := teamsWithText(bots, "I have added 3 and 5. The result is 8.")
		i := slices.IndexFunc(bots, func(m faketeams.MessageView) bool { return m.ID == card.ID })
		if i < 0 {
			return false
		}
		_, still := teamsWithAction(bots[i:i+1], "Approve")
		return answered && !still
	})
}

// TestTEAMS_AnAlertPostedByWorkflowsIsInvestigatedThroughNotifications: on the connection with a notificationURL, the leader keeps one Graph subscription to
// each bound channel; another app's card (an alert, as Workflows posts one) reaches it as a change notification, which it reads, and the channel's
// botMessage trigger matches the card's text and starts an investigation in the alert's thread. Its final message has the feedback buttons, posted as a card
// after the text; a member's press of Useful is answered in the invoke and counted.
func TestTEAMS_AnAlertPostedByWorkflowsIsInvestigatedThroughNotifications(t *testing.T) {
	ts := newTeams(t)
	ts.subscribed(ChannelTeamsMention, ChannelTeamsApproval, ChannelTeamsAlertPush, ChannelTeamsFailover)
	name := teamsUnique("TeamsPush")
	alert := ts.alert(ChannelTeamsAlertPush, name)
	buttons := ts.investigated(ChannelTeamsAlertPush, alert, name)

	var delivered bool
	for _, n := range ts.stats().Notifications {
		delivered = delivered || (n.Message == alert && n.Delivered)
	}
	if !delivered {
		t.Errorf("the fake delivered no notification of the alert %s; want the investigation to have come through Graph's notification", alert)
	}
	if _, ok := teamsWithAction([]faketeams.MessageView{buttons}, "Not useful"); !ok {
		t.Errorf("the feedback card has no Not useful: %s", buttons.Cards)
	}

	labels := map[string]string{"binding": TeamsNamespace + "/alert-push", "agent": "kagent/scenario-alert", "alertname": name, "verdict": "useful"}
	if told := ts.press(ChannelTeamsAlertPush, buttons.ID, teamsBob, "Useful"); !strings.Contains(told, "recorded as useful") && !strings.Contains(told, "received your press") {
		t.Errorf("the presser was told %q; want the vote recorded as useful, or the press received", told)
	}
	err := Wait(ts.ctx, stepTimeout, "agentkourier_investigation_feedback_total{verdict=useful} to be 1", func(context.Context) (bool, error) {
		return ts.counter("agentkourier_investigation_feedback_total", labels) == 1, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTEAMS_AnAlertPostedByWorkflowsIsInvestigatedThroughPolling: on the connection with no notificationURL there is no subscription; the leader reads the
// channel's messages/delta (the first sync only sets where it reads from), and the card it finds there starts the investigation.
func TestTEAMS_AnAlertPostedByWorkflowsIsInvestigatedThroughPolling(t *testing.T) {
	ts := newTeams(t)
	err := Wait(ts.ctx, 2*time.Minute, "the leader's first sync of "+ChannelTeamsAlertPoll, func(ctx context.Context) (bool, error) {
		st, err := ts.teams.Stats(ctx)
		return err == nil && st.Reads[ChannelTeamsAlertPoll].Synced >= 1, err
	})
	if err != nil {
		t.Fatal(err)
	}
	name := teamsUnique("TeamsPoll")
	alert := ts.alert(ChannelTeamsAlertPoll, name)
	ts.investigated(ChannelTeamsAlertPoll, alert, name)
	subs, err := ts.teams.Subscriptions(ts.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range subs {
		if sub.App == TeamsAppPoll || sub.Channel == ChannelTeamsAlertPoll {
			t.Errorf("subscription %+v; want none of the connection that polls", sub)
		}
	}
}

// TestTEAMS_ANewLeaderAdoptsTheSubscriptions: on two replicas, the leader's container is killed. The replica that takes over finds the subscriptions the
// old leader made and adopts them: none is created or deleted, each channel still has one, and an alert posted after the takeover reaches the new leader
// through them and is investigated once.
func TestTEAMS_ANewLeaderAdoptsTheSubscriptions(t *testing.T) {
	f := newFailover(t)
	requireTeams(t, f.scenario)
	ts := &teamsScenario{scenario: f.scenario, teams: faketeams.NewControl(f.env.TeamsURL)}
	ts.prepare()
	pushed := []string{ChannelTeamsMention, ChannelTeamsApproval, ChannelTeamsAlertPush, ChannelTeamsFailover}
	before := ts.subscribed(pushed...)
	st := ts.stats()
	creates, deletes := maps.Clone(st.SubscriptionCreates), maps.Clone(st.SubscriptionDeletes)

	old := f.leader()
	restarts := f.restarts()
	if err := f.sig.Kill(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	next := f.takeover(old)
	for _, conn := range []string{ConnTeamsPush, ConnTeamsPoll} {
		err := Wait(f.ctx, 2*time.Minute, "agentkourier_chat_connected{connection="+conn+"} to be 1 in "+next.ID(), func(ctx context.Context) (bool, error) {
			v, err := f.reps.Counter(ctx, next, "agentkourier_chat_connected", map[string]string{"connection": conn})
			if errors.Is(err, ErrNoSample) {
				return false, nil
			}
			return v == 1, err
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	name := teamsUnique("TeamsFailover")
	alert := ts.alert(ChannelTeamsFailover, name)
	ts.investigated(ChannelTeamsFailover, alert, name)
	err := Consistently(f.ctx, 10*time.Second, "one investigation of the alert", func(ctx context.Context) (bool, error) {
		th, err := ts.teams.Messages(ctx, ChannelTeamsFailover, alert)
		n := 0
		for _, m := range teamsBotMessages(th, TeamsAppPush) {
			if strings.Contains(m.Text, "Investigating:") {
				n++
			}
		}
		return n == 1, err
	})
	if err != nil {
		t.Error(err)
	}

	after := ts.subscribed(pushed...)
	st = ts.stats()
	for _, c := range pushed {
		if after[c].ID != before[c].ID || st.SubscriptionCreates[c] != creates[c] || st.SubscriptionDeletes[c] != deletes[c] {
			t.Errorf("channel %s: subscription %s became %s, creates %d to %d, deletes %d to %d; want the same one adopted, none created or deleted",
				c, before[c].ID, after[c].ID, creates[c], st.SubscriptionCreates[c], deletes[c], st.SubscriptionDeletes[c])
		}
	}
	f.settle()
	f.onlyEnded(restarts, old.Name)
}
