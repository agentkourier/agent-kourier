package broker

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/testutil"
	"github.com/agentkourier/agent-kourier/internal/trigger"
)

// guardRig is a botGuard over a real Intake for the connection team-a/slack, whose Binding owns channel C1, and a table in which the
// bot BOWN is the own connection's and BTEAMB is team-b's.
type guardRig struct {
	guard botGuard
	mu    sync.Mutex
	start []string // the bots whose posts started something, by the Binding's OnStart
}

func (g *guardRig) started() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.start...)
}

type noSubmit struct{}

func (noSubmit) Submit(context.Context, session.Inbound) (session.Result, error) {
	return session.Result{}, nil
}

// newGuardRig makes the rig with the triggers of the Binding given: none is the default, the mention trigger.
func newGuardRig(t *testing.T, triggers ...v1alpha1.Trigger) *guardRig {
	t.Helper()
	meta := func(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
	b := v1alpha1.Binding{ObjectMeta: meta("team-a", "on-a"), Spec: v1alpha1.BindingSpec{
		Agent:    v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "team-a", Name: "kagent"}, Namespace: "kagent", Name: "agent"},
		Identity: v1alpha1.Identity{UserID: "agent-kourier-a"},
		Chat:     v1alpha1.ChatBinding{ConnectionRef: v1alpha1.ObjectRef{Namespace: "team-a", Name: "slack"}, Channel: "C1", Triggers: triggers},
	}}
	b.Default()
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	snap, err := config.Resolve(t.Context(), &config.Set{
		AgentBackends:   []v1alpha1.AgentBackend{{ObjectMeta: meta("team-a", "kagent"), Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1}}},
		ChatConnections: []v1alpha1.ChatConnection{{ObjectMeta: meta("team-a", "slack")}},
		Bindings:        []v1alpha1.Binding{b},
	}, everyKey{})
	if err != nil {
		t.Fatal(err)
	}
	g := &guardRig{}
	in := trigger.NewIntake(trigger.IntakeConfig{
		Connection: v1alpha1.ObjectRef{Namespace: "team-a", Name: "slack"}, Snapshot: func() *config.Snapshot { return snap }, Submitter: noSubmit{},
		OnStart: func(_ context.Context, m trigger.Match) error {
			g.mu.Lock()
			who := m.Message.User
			if m.Message.Bot != nil {
				who = m.Message.Bot.ID
			}
			g.start = append(g.start, who)
			g.mu.Unlock()
			return nil
		},
	})
	table := newRuntimeTable()
	table.identify("platform/slack", appIdentity{Bot: "BOWN", User: "UOWN"})
	table.identify("team-a/slack", appIdentity{Bot: "BTEAMA", User: "UTEAMA"})
	table.identify("team-b/slack", appIdentity{Bot: "BTEAMB", User: "UTEAMB"})
	g.guard = botGuard{in: in, self: "team-a/slack", bots: table, log: testutil.DiscardLogger()}
	return g
}

// mention is a bot's mention of Agent Kourier in C1, as the adapter decodes an app_mention that has a bot_id.
func mention(bot, ts string) chat.Inbound {
	return chat.Inbound{
		At:        chat.Ref{Platform: "slack", Conn: "team-a/slack", Channel: "C1", ID: ts},
		User:      "U" + bot,
		Text:      "<@UKOURIER> look at this",
		Mentioned: true,
		Bot:       &chat.BotMessage{ID: bot, Kind: v1alpha1.SenderKindBot},
	}
}

// A bot's mention starts a turn on a Binding that takes them, unless the bot is another connection's of this broker: two of its apps in one
// channel would otherwise answer each other without end. The bot of another connection, the own connection's included, is dropped by default,
// at the adapter's question and, were it asked anyway, at the message; any other bot is the Intake's as before.
func TestABotMentionByAnotherConnectionsBotIsDroppedByDefault(t *testing.T) {
	g := newGuardRig(t)
	for _, bot := range []string{"BOWN", "BTEAMB"} {
		if g.guard.AcceptsBotMention("C1", bot) {
			t.Errorf("AcceptsBotMention(%s) = true; want the bot of another connection of this broker dropped", bot)
		}
		if err := g.guard.Message(t.Context(), mention(bot, "100."+bot)); err != nil {
			t.Fatalf("Message(%s) = %v; want it acknowledged", bot, err)
		}
	}
	if got := g.started(); len(got) != 0 {
		t.Errorf("mentions by %v started something; want none", got)
	}

	// Another bot, or a person's app that this broker does not run, is as before.
	if !g.guard.AcceptsBotMention("C1", "BSTRANGER") {
		t.Error("AcceptsBotMention(BSTRANGER) = false; want a bot that is none of this broker's to be the Intake's")
	}
	if err := g.guard.Message(t.Context(), mention("BSTRANGER", "101.1")); err != nil {
		t.Fatal(err)
	}
	if got := g.started(); len(got) != 1 || got[0] != "BSTRANGER" {
		t.Errorf("started by %v; want the mention of the stranger to start a turn", got)
	}
}

// A connection's own bot is not "another connection's": the adapter never forwards its own posts, and the guard leaves that to it.
func TestTheGuardDoesNotCallAConnectionsOwnBotAnotherConnectionsBot(t *testing.T) {
	g := newGuardRig(t)
	if !g.guard.AcceptsBotMention("C1", "BTEAMA") {
		t.Error("AcceptsBotMention(own bot) = false; want the Intake's answer, which the adapter never asks for its own posts")
	}
}

// A Binding's trigger that names the other connection's bot reads its posts: the message is taken as the trigger's, by AcceptsBot, which
// the guard leaves as the Intake has it, and that bot's mention is not taken as a mention (the trigger reads the same post).
func TestABotThatATriggerNamesIsTakenByTheTrigger(t *testing.T) {
	g := newGuardRig(t, v1alpha1.Trigger{Type: v1alpha1.TriggerMention}, chattest.AlertmanagerTrigger("BTEAMB"))
	if !g.guard.AcceptsBot("C1", "BTEAMB") {
		t.Error("AcceptsBot(BTEAMB) = false; want the trigger that names it to take its message")
	}
	if g.guard.AcceptsBot("C1", "BOWN") {
		t.Error("AcceptsBot(BOWN) = true; want the bot no trigger names dropped")
	}
	if g.guard.AcceptsBotMention("C1", "BTEAMB") {
		t.Error("AcceptsBotMention(BTEAMB) = true; want its post read by the trigger and not as a mention")
	}
	if g.guard.AcceptsBotMention("C1", "BOWN") {
		t.Error("AcceptsBotMention(BOWN) = true; want the bot of another connection dropped")
	}
}

// A personMessage trigger reads a post with no bot ID as a person's. Slack's posts of another app carry a bot ID, so the post of another connection's
// bot that arrives without one is not expected; were it to, the broker's two apps would answer each other through the trigger as they would through a
// mention, so the guard drops a post whose user is another connection's bot. A person's message goes through to the trigger.
func TestAPersonMessageTriggerIsNotFedByAnotherConnectionsBot(t *testing.T) {
	trigger := v1alpha1.Trigger{Type: v1alpha1.TriggerPersonMessage, PromptTemplate: "A person wrote in {{ .Channel }}.",
		Limits: v1alpha1.TriggerLimits{
			RateLimit:     &v1alpha1.RateLimit{MaxRuns: 1000, Per: metav1.Duration{Duration: time.Hour}, MaxConcurrent: 1000},
			MaxRunsPerDay: new(int32(0)),
		}}
	g := newGuardRig(t, trigger)
	post := func(user, ts string) chat.Inbound {
		return chat.Inbound{At: chat.Ref{Platform: "slack", Conn: "team-a/slack", Channel: "C1", ID: ts}, User: user, Text: "checkout is down"}
	}
	for _, user := range []string{"UOWN", "UTEAMB"} {
		if err := g.guard.Message(t.Context(), post(user, "100."+user)); err != nil {
			t.Fatal(err)
		}
	}
	if got := g.started(); len(got) != 0 {
		t.Fatalf("posts as %v started something; want the bots of the broker's other connections not to feed the trigger", got)
	}
	if err := g.guard.Message(t.Context(), post("UPERSON", "101.1")); err != nil {
		t.Fatal(err)
	}
	if got := g.started(); len(got) != 1 || got[0] != "UPERSON" {
		t.Errorf("started by %v; want the person's message to reach the trigger", got)
	}
	if err := g.guard.Message(t.Context(), post("UTEAMA", "102.1")); err != nil { // the connection's own bot is the adapter's to refuse, not the guard's
		t.Fatal(err)
	}
}
