package e2e

import (
	"encoding/json"
	"strings"

	"github.com/agentkourier/agent-kourier/test/faketeams"
)

// The Microsoft Teams side of the fast tier with E2E_TEAMS=1 (hack/kind/up-fast.sh): the fake Teams service (hack/kind/fast/teams/faketeams.yaml) and a
// team, teams-e2e, with two Teams connections and their Bindings (hack/kind/fast/teams/teams.yaml). The IDs are fake constants that the manifests name too.

// TeamsNamespace is the team's namespace, which the platform admits (hack/kind/teams-values.yaml); the TEAMS_ tests skip unless the broker admits it.
const TeamsNamespace = "teams-e2e"

// The Microsoft Entra IDs of the fake team: its tenant, the bots of its two connections, and the app that posts alerts' cards, as Workflows does.
const (
	TeamsTenant    = "7e2e7ea5-0000-4000-8000-000000000001"
	TeamsAppPush   = "7e2e7ea5-0000-4000-8000-0000000000a1"
	TeamsAppPoll   = "7e2e7ea5-0000-4000-8000-0000000000a2"
	TeamsWorkflows = "7e2e7ea5-0000-4000-8000-0000000000f1"
)

// The connections, as "namespace/name": the label of agentkourier_chat_connected. ConnTeamsPush keeps Graph subscriptions (spec.teams.notificationURL);
// ConnTeamsPoll has none, and polls.
const (
	ConnTeamsPush = TeamsNamespace + "/teams-push"
	ConnTeamsPoll = TeamsNamespace + "/teams-poll"
)

// The channels of the team, one for each Binding, so that no two share a thread.
const (
	// ChannelTeamsMention is the Binding mention's (scenario-gated, output live), on teams-push.
	ChannelTeamsMention = "19:teams-mention@thread.tacv2"
	// ChannelTeamsApproval is the Binding approval's (spike-approve, tool approvals), on teams-push.
	ChannelTeamsApproval = "19:teams-approval@thread.tacv2"
	// ChannelTeamsAlertPush is the Binding alert-push's (scenario-alert, a botMessage trigger on TeamsWorkflows), on teams-push.
	ChannelTeamsAlertPush = "19:teams-alert-push@thread.tacv2"
	// ChannelTeamsAlertPoll is the Binding alert-poll's, the same trigger on teams-poll.
	ChannelTeamsAlertPoll = "19:teams-alert-poll@thread.tacv2"
	// ChannelTeamsFailover is the Binding failover's, the same trigger on teams-push, for the case that kills the leader.
	ChannelTeamsFailover = "19:teams-failover@thread.tacv2"
)

// teamsChannels is each bound channel with the bot of the connection its Binding is on.
var teamsChannels = map[string]string{
	ChannelTeamsMention:   TeamsAppPush,
	ChannelTeamsApproval:  TeamsAppPush,
	ChannelTeamsAlertPush: TeamsAppPush,
	ChannelTeamsFailover:  TeamsAppPush,
	ChannelTeamsAlertPoll: TeamsAppPoll,
}

// teamsBotMessages are the messages of a thread that the bot of app posted.
func teamsBotMessages(th []faketeams.MessageView, app string) []faketeams.MessageView {
	var out []faketeams.MessageView
	for _, m := range th {
		if m.Kind == faketeams.SenderBot && m.From == app {
			out = append(out, m)
		}
	}
	return out
}

// teamsWithText is the first message of msgs whose text holds text, and false if none does.
func teamsWithText(msgs []faketeams.MessageView, text string) (faketeams.MessageView, bool) {
	for _, m := range msgs {
		if strings.Contains(m.Text, text) {
			return m, true
		}
	}
	return faketeams.MessageView{}, false
}

// teamsWithAction is the first message of msgs with a card that has an Action.Execute titled title, and false if none has.
func teamsWithAction(msgs []faketeams.MessageView, title string) (faketeams.MessageView, bool) {
	for _, m := range msgs {
		for _, c := range m.Cards {
			if cardHasAction(c, title) {
				return m, true
			}
		}
	}
	return faketeams.MessageView{}, false
}

// cardHasAction says whether an Adaptive Card has an Action.Execute titled title in its actions, where the Teams adapter puts its buttons and where
// the fake Teams service finds the action a press names.
func cardHasAction(card json.RawMessage, title string) bool {
	var c struct {
		Actions []struct {
			Type  string `json:"type"`
			Title string `json:"title"`
		} `json:"actions"`
	}
	if json.Unmarshal(card, &c) != nil {
		return false
	}
	for _, a := range c.Actions {
		if a.Type == "Action.Execute" && a.Title == title {
			return true
		}
	}
	return false
}
