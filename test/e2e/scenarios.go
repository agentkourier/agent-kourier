package e2e

// The channels the Phase 1 scenarios drive: each is bound, in the fast tier's courier config
// (hack/kind/fast/courier/config/binding-s*.yaml), to the stub agent its scenario needs, so that
// scenarios never share a thread. test/kindmanifests pins the pairing, so a change on either side fails
// `make test`. The full tier's config is the same, with the AgentBackend of its front door.
const (
	// ChannelSmoke is the Binding for the stub's test-agent that the smoke test uses. S5 has its own, below.
	ChannelSmoke = "C1"
	// ChannelS1 is S1's: scenario-gated, live output.
	ChannelS1 = "CS1"
	// ChannelS2 is S2's: scenario-memory, across a courier restart.
	ChannelS2 = "CS2"
	// ChannelS3 is S3's: scenario-ask, with askUser on.
	ChannelS3 = "CS3"
	// ChannelS4 is S4's: scenario-gated, a queued reply.
	ChannelS4 = "CS4"
	// ChannelS5 is S5's: test-agent, for the crash-after-post fault.
	ChannelS5 = "CS5"
	// ChannelS11 is S11's: spike-approve, with toolApprovals off.
	ChannelS11 = "CS11"
	// ChannelS12 is S12's: scenario-fails, whose task fails or completes with no output. The fast tier only:
	// the full tier's kagent has no such agent.
	ChannelS12 = "CS12"
	// ChannelS13 is S13's: scenario-tools, whose reply has two tool calls, the second of them failing. Both tiers:
	// the full tier's kagent has the agent, with a tool server of its own to call.
	ChannelS13 = "CS13"
	// ChannelS14 is S14's: scenario-alert, behind a botMessage trigger with the alertmanager preset for the bot BotS14.
	// The fast tier only: the full tier's kagent has no such agent.
	ChannelS14 = "CS14"
	// BotS14 is the bot ID S14's alerts are posted as, which the Binding's trigger names (never a display name, D36).
	BotS14 = "B0S14ALERT1"
)
