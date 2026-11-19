package e2e

import (
	"encoding/json"
	"testing"

	"github.com/agentkourier/agent-kourier/test/faketeams"
)

func TestTeamsHelpersFindTheBotsMessagesTextAndButtons(t *testing.T) {
	buttons := json.RawMessage(`{"type":"AdaptiveCard","actions":[{"type":"Action.Execute","title":"Useful","verb":"kourier"},{"type":"Action.OpenUrl","title":"Not useful"}]}`)
	th := []faketeams.MessageView{
		{ID: "1", Kind: faketeams.SenderApp, From: TeamsWorkflows, Text: "[FIRING] Disk"},
		{ID: "2", Kind: faketeams.SenderBot, From: TeamsAppPush, Text: "Investigating: Disk"},
		{ID: "3", Kind: faketeams.SenderBot, From: TeamsAppPush, Cards: []json.RawMessage{buttons}},
		{ID: "4", Kind: faketeams.SenderBot, From: TeamsAppPoll, Text: "Investigating: other"},
	}
	bots := teamsBotMessages(th, TeamsAppPush)
	if len(bots) != 2 || bots[0].ID != "2" || bots[1].ID != "3" {
		t.Fatalf("teamsBotMessages = %+v, want the push bot's two messages", bots)
	}
	if m, ok := teamsWithText(bots, "Investigating:"); !ok || m.ID != "2" {
		t.Errorf("teamsWithText = %+v, %v; want message 2", m, ok)
	}
	if _, ok := teamsWithText(bots, "Released."); ok {
		t.Error("teamsWithText found a text no message has")
	}
	if m, ok := teamsWithAction(bots, "Useful"); !ok || m.ID != "3" {
		t.Errorf("teamsWithAction(Useful) = %+v, %v; want message 3", m, ok)
	}
	for _, title := range []string{"Not useful", "Approve"} { // an action that is no Action.Execute, and one no card has
		if _, ok := teamsWithAction(bots, title); ok {
			t.Errorf("teamsWithAction(%s) found a button no card has", title)
		}
	}
	if cardHasAction(json.RawMessage(`not json`), "Useful") {
		t.Error("cardHasAction took a card that is not JSON")
	}
}

func TestTeamsChannelsAreEachOnTheirConnectionsBot(t *testing.T) {
	for channel, app := range teamsChannels {
		want := TeamsAppPush
		if channel == ChannelTeamsAlertPoll {
			want = TeamsAppPoll
		}
		if app != want {
			t.Errorf("channel %s is on the bot %s, want %s", channel, app, want)
		}
	}
}
