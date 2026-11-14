package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// The two connections of twoConnections, as the verification names them.
const (
	connA = "platform/slack-a"
	connB = "platform/slack-b"
)

var (
	refA = v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-a"}
	refB = v1alpha1.ObjectRef{Namespace: "platform", Name: "slack-b"}
)

// twoConnectionsWithApprovers is twoConnections where sre/oncall (on slack-a) and sre/other-bot (on slack-b) ask for tool approvals from
// the one group, approverGroups[0]: two Bindings of two apps that name one group ID and one channel.
func twoConnectionsWithApprovers(t *testing.T) *config.Snapshot {
	t.Helper()
	set := twoConnectionsSet()
	for i := range set.Bindings {
		if k := set.Bindings[i].Namespace + "/" + set.Bindings[i].Name; k == "sre/oncall" || k == "sre/other-bot" {
			set.Bindings[i].Spec.Interactions.ToolApprovals, set.Bindings[i].Spec.Interactions.ApproverGroups = true, []string{approverGroups[0]}
		}
	}
	snap, err := config.Resolve(t.Context(), set, keys{})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// twoConnectionsSet is the Set that twoConnections resolves.
func twoConnectionsSet() *config.Set {
	conn := func(name string) v1alpha1.ChatConnection {
		return v1alpha1.ChatConnection{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: name},
			Spec: v1alpha1.ChatConnectionSpec{Platform: "slack", CredentialsSecretRef: v1alpha1.SecretRef{Name: name}, AllowedNamespaces: []string{"sre", "ops"}}}
	}
	binding := func(ns, name, connection, channel string) v1alpha1.Binding {
		return v1alpha1.Binding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: v1alpha1.BindingSpec{
			Agent: v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "platform", Name: "kagent"}, Name: "a", Namespace: "kagent"},
			Chat:  v1alpha1.ChatBinding{ConnectionRef: v1alpha1.ObjectRef{Namespace: "platform", Name: connection}, Channel: channel},
		}}
	}
	return &config.Set{
		AgentBackends:   []v1alpha1.AgentBackend{{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "kagent"}, Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A, URL: "http://x", AllowedNamespaces: []string{"sre", "ops"}}}},
		ChatConnections: []v1alpha1.ChatConnection{conn("slack-a"), conn("slack-b")},
		Bindings: []v1alpha1.Binding{
			binding("sre", "oncall", "slack-a", "C1"), binding("ops", "oncall", "slack-a", "C2"),
			binding("sre", "other-bot", "slack-b", "C1"), binding("ops", "also-other", "slack-b", "C3"),
		},
	}
}

// secretsByName is an identity.SecretReader of several Secrets, each a secretStore, by "namespace/name".
type secretsByName map[string]*secretStore

func (s secretsByName) Value(ctx context.Context, ns, name, key string) (secret.Value, error) {
	st, ok := s[ns+"/"+name]
	if !ok {
		return secret.Value{}, fmt.Errorf("%s/%s: %w", ns, name, config.ErrSecretNotFound)
	}
	return st.Value(ctx, ns, name, key)
}

// bothSecretsHold is the Secrets of slack-a and slack-b holding the tokens that tokensOf gives their connections.
func bothSecretsHold() secretsByName {
	return secretsByName{
		connA: {vals: map[string]string{"botToken": "bot-a", "appToken": "app-a"}},
		connB: {vals: map[string]string{"botToken": "bot-b", "appToken": "app-b"}},
	}
}

// bothConnections is the served connections, each with the tokens its platform was built with.
func bothConnections() []Connection {
	return []Connection{
		{Ref: refA, Platform: "slack", Credentials: tokens("bot-a", "app-a")},
		{Ref: refB, Platform: "slack", Credentials: tokens("bot-b", "app-b")},
	}
}

// reachable is a backend probe that finds every agent fine, and counts the Bindings it was asked about.
type reachable struct {
	mu    sync.Mutex
	asked map[string]int
}

func (r *reachable) CheckAgent(_ context.Context, binding string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.asked == nil {
		r.asked = map[string]int{}
	}
	r.asked[binding]++
	return nil
}

// downSet is a Resolver that says ErrConnectionDown for the connections in down, which the test moves, and has the platform of any other.
type downSet struct {
	platforms chat.PlatformSet[platformReader]
	mu        sync.Mutex
	down      map[string]bool
}

func (d *downSet) Platform(conn string) (platformReader, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down[conn] {
		return nil, fmt.Errorf("%w: %q", chat.ErrConnectionDown, conn)
	}
	return d.platforms.Platform(conn)
}

func (d *downSet) setDown(conn string, down bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.down[conn] = down
}

func (f *accessFake) channelsAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	asked := slices.Clone(f.channels)
	slices.Sort(asked)
	return slices.Compact(asked)
}

func (f *accessFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls + len(f.groupsCalls)
}

// Each Binding is probed with the bot of its own connection, through the resolver. The two connections' apps answer differently about
// the one channel (C1) and the one group their Bindings name, and each answer lands on its own connection's Binding: the channel and
// the group are read once on each bot, and never on the other connection's.
func TestEachBindingIsProbedWithTheBotOfItsOwnConnection(t *testing.T) {
	current := &snaps{}
	current.p.Store(twoConnectionsWithApprovers(t))
	botA := &accessFake{access: chat.Access{Member: true}}
	botB := &accessFake{access: chat.Access{}, groupsErr: groupErr(approverGroups[0], chat.ErrGroupNotFound, chat.ErrGroupsMisconfigured)}
	backends := &reachable{}
	p := verification{
		Bindings: backends, Platforms: chat.PlatformSet[platformReader]{connA: botA, connB: botB},
		Conns: bothConnections, Snaps: current, Secrets: bothSecretsHold(), Revision: func(context.Context) string { return "r" },
		Metrics: mustMetrics(t, nil), Log: testutil.DiscardLogger(), Interval: time.Hour, Tick: 5 * time.Millisecond,
	}.newProber()
	got := settled(t, p, 4)

	for _, b := range []string{"sre/oncall", "ops/oncall", "sre/other-bot", "ops/also-other"} {
		if backends.asked[b] != 1 {
			t.Errorf("the backend of %s was probed %d times; want once", b, backends.asked[b])
		}
	}
	for b, exp := range map[string]struct {
		channel want
		groups  *want
	}{
		"sre/oncall":     {channel: want{bindingcheck.True, bindingcheck.ReasonOK}, groups: &want{bindingcheck.True, bindingcheck.ReasonOK}},
		"ops/oncall":     {channel: want{bindingcheck.True, bindingcheck.ReasonOK}},
		"sre/other-bot":  {channel: want{bindingcheck.False, bindingcheck.ReasonNotInChannel}, groups: &want{bindingcheck.False, bindingcheck.ReasonGroupNotFound}},
		"ops/also-other": {channel: want{bindingcheck.False, bindingcheck.ReasonNotInChannel}},
	} {
		if c := got[b][bindingcheck.ChannelJoined]; c.Status != exp.channel.status || c.Reason != exp.channel.reason {
			t.Errorf("%s: ChannelJoined is %s/%s; want %s/%s, the answer of its own connection's bot", b, c.Status, c.Reason, exp.channel.status, exp.channel.reason)
		}
		c, has := got[b][bindingcheck.ApproversReadable]
		switch {
		case exp.groups == nil && has:
			t.Errorf("%s has an ApproversReadable condition; it names no groups", b)
		case exp.groups != nil && (c.Status != exp.groups.status || c.Reason != exp.groups.reason):
			t.Errorf("%s: ApproversReadable is %s/%s; want %s/%s, the answer of its own connection's bot", b, c.Status, c.Reason, exp.groups.status, exp.groups.reason)
		}
	}
	if asked := botA.channelsAsked(); !slices.Equal(asked, []string{"C1", "C2"}) {
		t.Errorf("the bot of slack-a was asked about %v; want C1 and C2, the channels of its Bindings", asked)
	}
	if asked := botB.channelsAsked(); !slices.Equal(asked, []string{"C1", "C3"}) {
		t.Errorf("the bot of slack-b was asked about %v; want C1 and C3, the channels of its Bindings", asked)
	}
	// A pass reads each group once for each bot: the group two Bindings of two apps name is two reads, not one shared across apps.
	if len(botA.groupsCalls) != 1 || len(botB.groupsCalls) != 1 {
		t.Errorf("group reads: %d on slack-a's bot and %d on slack-b's; want one on each", len(botA.groupsCalls), len(botB.groupsCalls))
	}
}

// A changed Secret of one connection says ChatCredentialsChanged on that connection's Bindings only, and makes no call as that
// connection's bot; a Secret that is gone says ChatCredentialsUnusable the same way. The other connection's Bindings are read as before,
// whatever the first one's Secret holds.
func TestAChangedSecretOfOneConnectionHoldsItsOwnBindingsOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(secretsByName)
		reason bindingcheck.Reason
	}{
		{"changed", func(s secretsByName) { s[connB].vals["botToken"] = "bot-b-rotated" }, bindingcheck.ReasonChatCredentialsChanged},
		{"changed in a key the call does not use", func(s secretsByName) { s[connB].vals["appToken"] = "app-b-rotated" }, bindingcheck.ReasonChatCredentialsChanged},
		{"gone", func(s secretsByName) { s[connB].missing = true }, bindingcheck.ReasonChatCredentialsUnusable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := &snaps{}
			current.p.Store(twoConnectionsWithApprovers(t))
			secrets := bothSecretsHold()
			tc.mutate(secrets)
			botA, botB := &accessFake{access: chat.Access{Member: true}}, &accessFake{access: chat.Access{Member: true}}
			p := verification{
				Bindings: &reachable{}, Platforms: chat.PlatformSet[platformReader]{connA: botA, connB: botB},
				Conns: bothConnections, Snaps: current, Secrets: secrets, Revision: func(context.Context) string { return "r" },
				Metrics: mustMetrics(t, nil), Log: testutil.DiscardLogger(), Interval: time.Hour, Tick: 5 * time.Millisecond,
			}.newProber()
			got := settled(t, p, 4)

			for _, b := range []string{"sre/other-bot", "ops/also-other"} {
				if c := got[b][bindingcheck.ChannelJoined]; c.Status != bindingcheck.Unknown || c.Reason != tc.reason {
					t.Errorf("%s: ChannelJoined is %s/%s; want Unknown/%s, the Secret of its connection being different", b, c.Status, c.Reason, tc.reason)
				}
			}
			if c := got["sre/other-bot"][bindingcheck.ApproversReadable]; c.Status != bindingcheck.Unknown || c.Reason != tc.reason {
				t.Errorf("sre/other-bot: ApproversReadable is %s/%s; want Unknown/%s", c.Status, c.Reason, tc.reason)
			}
			for _, b := range []string{"sre/oncall", "ops/oncall"} {
				if c := got[b][bindingcheck.ChannelJoined]; c.Status != bindingcheck.True {
					t.Errorf("%s: ChannelJoined is %s/%s; want True: its connection's Secret did not change", b, c.Status, c.Reason)
				}
			}
			if c := got["sre/oncall"][bindingcheck.ApproversReadable]; c.Status != bindingcheck.True {
				t.Errorf("sre/oncall: ApproversReadable is %s/%s; want True", c.Status, c.Reason)
			}
			if n := botB.callCount(); n != 0 {
				t.Errorf("the bot of slack-b was called %d times; want none while its Secret differs", n)
			}
			if n := botA.callCount(); n == 0 {
				t.Error("the bot of slack-a was not called")
			}
		})
	}
}

// A Binding of a connection that is not running (the resolver says ErrConnectionDown) makes no call as a bot: its ChannelJoined and
// ApproversReadable are Unknown/ChatUnavailable, with a message that says the connection is not running, and they are read again once
// it runs. The backend of the Binding is probed as before, since that is no chat call, and the Bindings of the other connection are
// not held back.
func TestABindingOfAConnectionThatIsNotRunningIsToldSoWithoutACall(t *testing.T) {
	current := &snaps{}
	current.p.Store(twoConnectionsWithApprovers(t))
	botA, botB := &accessFake{access: chat.Access{Member: true}}, &accessFake{access: chat.Access{Member: true}}
	resolver := &downSet{platforms: chat.PlatformSet[platformReader]{connA: botA, connB: botB}, down: map[string]bool{connB: true}}
	var revision atomic.Int64
	backends := &reachable{}
	var buf logBuffer
	p := verification{
		Bindings: backends, Platforms: resolver, Conns: bothConnections, Snaps: current, Secrets: bothSecretsHold(),
		Revision: func(context.Context) string { return fmt.Sprint(revision.Load()) },
		Metrics:  mustMetrics(t, nil), Log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Interval: time.Hour, Tick: 5 * time.Millisecond, MinRevisionGap: time.Millisecond,
	}.newProber()
	runProber(t, p)
	got := waitSettled(t, p, 4)

	for _, cond := range []bindingcheck.ConditionType{bindingcheck.ChannelJoined, bindingcheck.ApproversReadable} {
		c := got["sre/other-bot"][cond]
		if c.Status != bindingcheck.Unknown || c.Reason != bindingcheck.ReasonChatUnavailable || !strings.Contains(c.Message, "not running") {
			t.Errorf("sre/other-bot: %s is %s/%s %q; want Unknown/ChatUnavailable saying the connection is not running", cond, c.Status, c.Reason, c.Message)
		}
	}
	if c := got["ops/also-other"][bindingcheck.ChannelJoined]; c.Reason != bindingcheck.ReasonChatUnavailable {
		t.Errorf("ops/also-other: ChannelJoined is %s/%s; want Unknown/ChatUnavailable", c.Status, c.Reason)
	}
	if r := got["sre/other-bot"][bindingcheck.Ready]; r.Status != bindingcheck.Unknown || r.Reason != "ChannelJoined:ChatUnavailable" {
		t.Errorf("Ready of sre/other-bot is %s/%s; want Unknown/ChannelJoined:ChatUnavailable", r.Status, r.Reason)
	}
	if c := got["sre/other-bot"][bindingcheck.BackendReachable]; c.Status != bindingcheck.True {
		t.Errorf("sre/other-bot: BackendReachable is %s/%s; want True: the agent is no chat call", c.Status, c.Reason)
	}
	if c := got["sre/oncall"][bindingcheck.ChannelJoined]; c.Status != bindingcheck.True {
		t.Errorf("sre/oncall: ChannelJoined is %s/%s; want True: the other connection runs", c.Status, c.Reason)
	}
	if n := botB.callCount(); n != 0 {
		t.Errorf("the bot of the connection that is not running was called %d times; want none", n)
	}
	// The operator's log has why: the INFO line of the transition carries the resolver's error, which names the connection.
	// (The line is written by the Prober's hook, a moment after the condition is set.)
	testutil.Eventually(t, "no INFO line with the connection as the cause of sre/other-bot's ChannelJoined", func() bool {
		return countOf(buf.String(), "level=INFO", "a condition is unknown", "binding=sre/other-bot", "condition=ChannelJoined", "cause=", connB) == 1
	}, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("a connection that is not running logged a warning:\n%s", buf.String())
	}

	// It starts: the next pass reads with its bot.
	resolver.setDown(connB, false)
	revision.Add(1)
	testutil.Eventually(t, "the Bindings of the connection that now runs were not read", func() bool {
		return p.State().Snapshot()["sre/other-bot"] != nil && func() bool {
			for _, c := range p.State().Snapshot()["sre/other-bot"] {
				if c.Type == bindingcheck.ChannelJoined {
					return c.Status == bindingcheck.True
				}
			}
			return false
		}()
	}, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))
	if botB.callCount() == 0 {
		t.Error("the bot of slack-b was not called after the connection started")
	}
}

// A resolver that fails for another reason than a connection being down is a probe that could not tell, not a finding about the
// Binding: Unknown/ProbeFailed, with the error told to the log.
func TestAPlatformTheResolverCannotGiveIsAProbeThatCouldNotTell(t *testing.T) {
	current := &snaps{}
	current.p.Store(twoConnections(t, "ops/oncall"))
	var buf logBuffer
	p := verification{
		Bindings: &reachable{}, Platforms: failingResolver{errors.New("the platform of slack-a is a *int")}, Conns: func() []Connection { return bothConnections()[:1] },
		Snaps: current, Revision: func(context.Context) string { return "r" },
		Metrics: mustMetrics(t, nil), Log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), Interval: time.Hour, Tick: 5 * time.Millisecond,
	}.newProber()
	got := settled(t, p, 1)
	if c := got["sre/oncall"][bindingcheck.ChannelJoined]; c.Status != bindingcheck.Unknown || c.Reason != bindingcheck.ReasonProbeFailed {
		t.Errorf("ChannelJoined is %s/%s; want Unknown/ProbeFailed", c.Status, c.Reason)
	}
	if !strings.Contains(buf.String(), "a probe failed") {
		t.Errorf("no line for the probe that could not tell:\n%s", buf.String())
	}
}

type failingResolver struct{ err error }

func (f failingResolver) Platform(string) (platformReader, error) { return nil, f.err }

// The Targets cover every connection served and no other, each Target naming its connection; a Binding on a connection that is not
// served is left out (the files route can hold such a Binding), as is every Binding before the first load.
func TestBindingTargetsCoverEveryServedConnection(t *testing.T) {
	snap := twoConnections(t)
	got := bindingTargets(snap, []v1alpha1.ObjectRef{refA, refB})
	want := []bindingcheck.Target{
		{Binding: "ops/also-other", Connection: connB, Channel: "C3"}, {Binding: "ops/oncall", Connection: connA, Channel: "C2"},
		{Binding: "sre/oncall", Connection: connA, Channel: "C1"}, {Binding: "sre/other-bot", Connection: connB, Channel: "C1"},
	}
	if !slices.EqualFunc(got, want, func(a, b bindingcheck.Target) bool {
		return a.Binding == b.Binding && a.Connection == b.Connection && a.Channel == b.Channel
	}) {
		t.Errorf("targets of both connections = %v; want %v", got, want)
	}
	if got := bindingTargets(snap, []v1alpha1.ObjectRef{refB}); len(got) != 2 || got[0].Connection != connB || got[1].Connection != connB {
		t.Errorf("targets of slack-b alone = %v; want its two Bindings", got)
	}
	if got := bindingTargets(snap, nil); len(got) != 0 {
		t.Errorf("targets of no connection = %v; want none", got)
	}
}

// What a Binding's connection does not hold is not probed for another: a Binding that moves to the other connection is read with that
// connection's bot at the next pass.
func TestABindingThatMovesToAnotherConnectionIsReadWithThatConnectionsBot(t *testing.T) {
	current := &snaps{}
	current.p.Store(twoConnections(t, "ops/oncall", "ops/also-other", "sre/other-bot"))
	botA, botB := &accessFake{access: chat.Access{Member: true}}, &accessFake{access: chat.Access{}}
	var revision atomic.Int64
	p := verification{
		Bindings: &reachable{}, Platforms: chat.PlatformSet[platformReader]{connA: botA, connB: botB}, Conns: bothConnections, Snaps: current,
		Revision: func(context.Context) string { return fmt.Sprint(revision.Load()) }, Secrets: bothSecretsHold(),
		Metrics: mustMetrics(t, nil), Log: testutil.DiscardLogger(), Interval: time.Hour, Tick: 5 * time.Millisecond, MinRevisionGap: time.Millisecond,
	}.newProber()
	runProber(t, p)
	if c := waitSettled(t, p, 1)["sre/oncall"][bindingcheck.ChannelJoined]; c.Status != bindingcheck.True {
		t.Fatalf("ChannelJoined on slack-a is %s/%s; want True", c.Status, c.Reason)
	}
	current.p.Store(twoConnectionsMoved(t, map[string]string{"sre/oncall": "slack-b"}, "ops/oncall", "ops/also-other", "sre/other-bot"))
	revision.Add(1)
	testutil.Eventually(t, "sre/oncall was not read with the bot of slack-b", func() bool {
		for _, c := range p.State().Snapshot()["sre/oncall"] {
			if c.Type == bindingcheck.ChannelJoined {
				return c.Status == bindingcheck.False && c.Reason == bindingcheck.ReasonNotInChannel
			}
		}
		return false
	}, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))
}

// sreSlack is the one connection of the verification tests that serve one: snapshotWith's.
var sreSlack = v1alpha1.ObjectRef{Namespace: "sre", Name: "slack"}

// soleBot is the Platforms of a verification that serves one connection, which platform runs.
func soleBot(ref v1alpha1.ObjectRef, platform platformReader) chat.Resolver[platformReader] {
	return chat.PlatformSet[platformReader]{v1alpha1.NamespacedName(ref.Namespace, ref.Name): platform}
}

// soleConn is the Conns of a verification that serves one connection, with no credentials to tell a change against.
func soleConn(ref v1alpha1.ObjectRef) func() []Connection {
	return func() []Connection { return []Connection{{Ref: ref, Platform: "slack"}} }
}

// soleChannels is a channelChecker as the Prober's ChannelChecker, for the tests of what one connection's checker finds: whatever
// connection the Target names, it reads with the one bot.
type soleChannels struct{ channelChecker }

func (s soleChannels) CheckChannel(ctx context.Context, _, channel string) (bindingcheck.Outcome, error) {
	return s.channelChecker.CheckChannel(ctx, channel)
}
