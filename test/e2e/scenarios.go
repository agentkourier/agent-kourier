package e2e

// The channels the Phase 1 scenarios drive: each is bound, in the fast tier's Agent Kourier config
// (hack/kind/fast/agent-kourier/config/binding-*.yaml), to the stub agent its scenario needs, so that
// scenarios never share a thread. test/kindmanifests pins the pairing, so a change on either side fails
// `make test`. The full tier's config is the same, with the AgentBackend of its front door.
const (
	// ChannelSmoke is the Binding for the stub's test-agent that the smoke test uses. S5 has its own, below.
	ChannelSmoke = "C1"
	// ChannelS1 is S1's: scenario-gated, live output.
	ChannelS1 = "CS1"
	// ChannelS2 is S2's: scenario-memory, across an Agent Kourier restart.
	ChannelS2 = "CS2"
	// ChannelS3 is S3's: scenario-ask, with askUser on.
	ChannelS3 = "CS3"
	// ChannelS4 is S4's: scenario-gated, a queued reply.
	ChannelS4 = "CS4"
	// ChannelS5 is S5's: test-agent, for the crash-after-post fault.
	ChannelS5 = "CS5"
	// ChannelS11 is S11's: spike-approve, whose tool approval Agent Kourier denies.
	ChannelS11 = "CS11"
	// ChannelS19 is S19's: spike-approve, with tool approvals enabled, so its tool result reaches the thread.
	ChannelS19 = "CS19"
	// ChannelS20 is S20's: spike-approve, with tool approvals enabled, so its rejection continuation reaches the thread.
	ChannelS20 = "CS20"
	// ChannelS21 is S21's: an approval that times out before anyone decides it.
	ChannelS21 = "CS21"
	// ChannelS22 is S22's: membership and approver-group refusals before an authorized approval.
	ChannelS22 = "CS22"
	// ChannelS23 is S23's: a multi-tool approval with independent verdicts.
	ChannelS23 = "CS23"
	// ChannelS24 is S24's: two presses on one approval, where the first press wins.
	ChannelS24 = "CS24"
	// ChannelS12 is S12's: scenario-fails, whose task fails or completes with no output. The fast tier only:
	// the full tier's kagent has no such agent.
	ChannelS12 = "CS12"
	// ChannelS13 is S13's: scenario-tools, whose reply has two tool calls, the second of them failing. Both tiers:
	// the full tier's kagent has the agent, with a tool server of its own to call.
	ChannelS13 = "CS13"
	// ChannelS14 is S14's: scenario-alert, behind a botMessage trigger written as the Alertmanager trigger for the bot BotS14.
	// The fast tier only: the full tier's kagent has no such agent.
	ChannelS14 = "CS14"
	// BotS14 is the bot ID S14's alerts are posted as, which the Binding's trigger names (never a display name, D36).
	BotS14 = "B0S14ALERT1"
	// ChannelS15 is S15's: scenario-alert behind a botMessage trigger written as the Alertmanager trigger for the bot BotS15, whose
	// limits are three starts in 10 seconds. The fast tier only, as S14's.
	ChannelS15 = "CS15"
	// BotS15 is the bot ID S15's alerts are posted as.
	BotS15 = "B0S15ALERT1"
)

// The channels of the scenarios run again through the generic a2a dialect against the stub agent in its plain pod (the fast tier
// only, hack/kind/fast/agent-kourier/a2a/binding-a2a-*.yaml): the same agents as the kagent-v1 channels above, each reached at its
// exact agent URL as an agent in a plain Deployment would be. The text scenarios run, and S19's approval, which the dialect reads
// because the stub's card lists kagent's HITL extension. test/kindmanifests pins the dialect and the URL of each.
const (
	// ChannelA2AS1 is S1's over a2a: scenario-gated, live output.
	ChannelA2AS1 = "CA2AS1"
	// ChannelA2AS4 is S4's over a2a: scenario-gated, a queued reply.
	ChannelA2AS4 = "CA2AS4"
	// ChannelA2AS5 is S5's over a2a: test-agent, final output, for the crash-after-post fault.
	ChannelA2AS5 = "CA2AS5"
	// ChannelA2AS19 is S19's over a2a, with tool approvals enabled, through the HITL extension the card lists: the fast
	// tier's stub spike-approve, and the full tier's real kagent scenario-approval (each tier deploys its own Binding).
	ChannelA2AS19 = "CA2AS19"
	// ChannelA2AS20 is S20's over a2a, the full tier's only: real kagent's scenario-approval, denied.
	ChannelA2AS20 = "CA2AS20"
	// ChannelA2AContractS19 and ChannelA2AContractS20 are S19's and S20's over a2a with an agent that is no stub of
	// kagent's: test/hitlagent, written from the agent contract alone (the fast tier's only).
	ChannelA2AContractS19 = "CA2AC19"
	ChannelA2AContractS20 = "CA2AC20"
	// ChannelA2AMemory is the channel of the scenario that restarts the stub agent between two turns: scenario-memory, which
	// says what its context has had, so that a context the agent lost shows.
	ChannelA2AMemory = "CA2AM"
)

// S17's channel and bot: the feedback buttons on an investigation an alert started.
const (
	// ChannelS17 is S17's: scenario-alert behind a botMessage trigger written as the Alertmanager trigger for the bot BotS17. The fast
	// tier only, as S14's.
	ChannelS17 = "CS17"
	// BotS17 is the bot ID S17's alerts are posted as.
	BotS17 = "B0S17ALERT1"
)

// S18's channel and bot: a thread whose replies need a mention (chat.threadReplies: mention).
const (
	// ChannelS18 is S18's: scenario-context behind a mention trigger and a botMessage trigger written as the Alertmanager trigger for the
	// bot BotS18, with threadReplies mention. The fast tier only: the full tier's kagent has no such agent.
	ChannelS18 = "CS18"
	// BotS18 is the bot ID S18's alerts are posted as.
	BotS18 = "B0S18ALERT1"
)

// The failover scenario's channels (S16, the fast tier on Postgres with two Agent Kourier replicas only), one for each kind of Binding
// its cases need.
const (
	// ChannelS16 is S16's cases 1 and 4: scenario-gated, live output, a mention.
	ChannelS16 = "CS16"
	// ChannelS16B is S16's case 2: test-agent, final output, for the crash-after-post fault.
	ChannelS16B = "CS16B"
	// ChannelS16C is S16's case 3: scenario-gated behind a botMessage trigger written as the Alertmanager trigger for the bot BotS16,
	// whose limits are three starts in 10 seconds.
	ChannelS16C = "CS16C"
	// BotS16 is the bot ID S16's alerts are posted as.
	BotS16 = "B0S16ALERT1"
)
