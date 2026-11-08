package e2e

// The Slack apps of the scenarios of a broker that serves several ChatConnections (the MC_ tests; E2E_TWO_APPS=1 in hack/kind/up-fast.sh). The fake Slack
// of the fast tier hosts three apps (hack/kind/fast/doubles.yaml): the default one, which the tier's ChatConnection slack-fast uses, and team-a's and
// team-b's. test/kindmanifests pins that the manifests and these constants agree.

// FakeApp is a Slack app the fake Slack hosts: who it posts as, and the tokens a ChatConnection's Secret holds for it. The tokens are fake constants.
type FakeApp struct {
	AppID, BotUser, BotID string
	// BotToken and AppToken are the fake tokens the app owns; empty for the default app, which owns every token no other does.
	BotToken, AppToken string
}

// Mention is how a person addresses the app's bot in a channel it is in.
func (a FakeApp) Mention() string { return "<@" + a.BotUser + "> " }

var (
	// AppPlatform is the fake Slack's default app, the platform's own ChatConnection (agent-kourier/slack-fast).
	AppPlatform = FakeApp{AppID: "AFAKEAPP", BotUser: "UFAKEBOT", BotID: "BFAKEBOT"}
	// AppTeamA is team-a's app: its ChatConnection, Secret and Binding are in the tier from the start (hack/kind/fast/two-apps).
	AppTeamA = FakeApp{AppID: "ATEAMA", BotUser: "UTEAMA", BotID: "BTEAMA", BotToken: "xoxb-fake-team-a-token", AppToken: "xapp-fake-team-a-token"} //nolint:gosec // a fake constant: the tokens the fake Slack hosts the app with, which no real Slack accepts
	// AppTeamB is a third app, which a case connects while the broker runs.
	AppTeamB = FakeApp{AppID: "ATEAMB", BotUser: "UTEAMB", BotID: "BTEAMB", BotToken: "xoxb-fake-team-b-token", AppToken: "xapp-fake-team-b-token"} //nolint:gosec // a fake constant: the tokens the fake Slack hosts the app with, which no real Slack accepts
)

// The connections of the tier, as "namespace/name": the label of agentkourier_chat_connected.
const (
	// ConnPlatform is the platform's own connection.
	ConnPlatform = "agent-kourier/slack-fast"
	// ConnTeamA is team-a's.
	ConnTeamA = "team-a/team-a"
)

// The channels of the scenarios, one for each Binding, so that no two share a thread. The first four are bound in the tier (hack/kind/fast/two-apps) to the
// stub's scenario-gated: mc-platform and mc-platform-final in agent-kourier, on the platform's connection, and team-a and team-a-final in team-a, on team-a's.
// The others are for the Bindings a case makes while the broker runs.
const (
	// ChannelMCPlatform is the platform Binding's channel.
	ChannelMCPlatform = "CMCPLATFORM"
	// ChannelMCTeamA is team-a's Binding's channel.
	ChannelMCTeamA = "CMCTEAMA"
	// ChannelMCPlatformFinal and ChannelMCTeamAFinal are the channels of the same two Bindings with the output final: one post through the outbox when
	// the turn ends, in place of a live stream.
	ChannelMCPlatformFinal = "CMCPLATFINAL"
	ChannelMCTeamAFinal    = "CMCTEAMAFINAL"
	// ChannelMCTeamB is the channel of the Binding on the connection a case adds in team-b.
	ChannelMCTeamB = "CMCTEAMB"
	// ChannelMCDuplicate is the channel of the Binding on a second connection of team-a's app, in team-c.
	ChannelMCDuplicate = "CMCDUP"
)
