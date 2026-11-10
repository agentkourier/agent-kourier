package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// errorSet is a Resolver whose connections in down answer with the error the test set for them, and have the platform of any other.
type errorSet struct {
	platforms chat.PlatformSet[platformReader]
	mu        sync.Mutex
	down      map[string]error
}

func (e *errorSet) Platform(conn string) (platformReader, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.down[conn]; err != nil {
		return nil, err
	}
	return e.platforms.Platform(conn)
}

// A connection down says why when the table knows: the platform rejected its credentials is False/ChatCredentialsRejected, and the Secret
// that cannot be used is Unknown/ChatCredentialsUnusable. Both say that the connection starts again by itself, the reason a team connection
// is the only one that can be down for. Any other connection that is down is Unknown/ChatUnavailable, as before. The cause is for the log.
func TestConnectionDownOutcomeSaysWhyTheConnectionIsDown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status bindingcheck.Status
		reason bindingcheck.Reason
		says   string
	}{
		{"rejected", &connectionStopped{conn: connB, rejected: true, code: "invalid_auth"}, bindingcheck.False, bindingcheck.ReasonChatCredentialsRejected, "starts again by itself"},
		{"rejected, wrapped", fmt.Errorf("find it: %w", &connectionStopped{conn: connB, rejected: true, code: "token_revoked"}), bindingcheck.False, bindingcheck.ReasonChatCredentialsRejected, "starts again by itself"},
		{"unusable", &connectionStopped{conn: connB, cause: fmt.Errorf("read botToken: %w", secret.ErrKeyNotFound)}, bindingcheck.Unknown, bindingcheck.ReasonChatCredentialsUnusable, "starts by itself once the Secret is mended"},
		{"down", downError(connB), bindingcheck.Unknown, bindingcheck.ReasonChatUnavailable, "not running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := connectionDownOutcome(tc.err)
			if out.Status != tc.status || out.Reason != tc.reason || !strings.Contains(out.Message, tc.says) {
				t.Errorf("outcome = %s/%s %q; want %s/%s saying %q", out.Status, out.Reason, out.Message, tc.status, tc.reason, tc.says)
			}
			if strings.Contains(out.Message, "restart Agent Kourier") {
				t.Errorf("message %q advises a restart of Agent Kourier; a connection that is down starts again by itself", out.Message)
			}
			if !strings.Contains(out.Cause, connB) {
				t.Errorf("cause = %q; want it to name the connection, for the log", out.Cause)
			}
		})
	}
}

// The Bindings of a connection the platform rejected are False/ChatCredentialsRejected on ChannelJoined and ApproversReadable, with no call as
// that connection's bot, and Ready follows; the Bindings of the connection that runs are read as before. The log's line for it is a WARN with
// the cause.
func TestTheBindingsOfARejectedConnectionSayTheCredentialsWereRejectedWithoutACall(t *testing.T) {
	current := &snaps{}
	current.p.Store(twoConnectionsWithApprovers(t))
	botA, botB := &accessFake{access: chat.Access{Member: true}}, &accessFake{access: chat.Access{Member: true}}
	resolver := &errorSet{platforms: chat.PlatformSet[platformReader]{connA: botA, connB: botB},
		down: map[string]error{connB: &connectionStopped{conn: connB, rejected: true, code: "invalid_auth"}}}
	var buf logBuffer
	p := verification{
		Own: refA, Bindings: &reachable{}, Platforms: resolver, Conns: bothConnections, Snaps: current, Secrets: bothSecretsHold(),
		Revision: func(context.Context) string { return "r" },
		Metrics:  mustMetrics(t, nil), Log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Interval: time.Hour, Tick: 5 * time.Millisecond,
	}.newProber()
	got := settled(t, p, 4)

	for _, cond := range []bindingcheck.ConditionType{bindingcheck.ChannelJoined, bindingcheck.ApproversReadable} {
		if c := got["sre/other-bot"][cond]; c.Status != bindingcheck.False || c.Reason != bindingcheck.ReasonChatCredentialsRejected {
			t.Errorf("sre/other-bot: %s is %s/%s; want False/ChatCredentialsRejected", cond, c.Status, c.Reason)
		}
	}
	if c := got["ops/also-other"][bindingcheck.ChannelJoined]; c.Reason != bindingcheck.ReasonChatCredentialsRejected {
		t.Errorf("ops/also-other: ChannelJoined is %s/%s; want False/ChatCredentialsRejected", c.Status, c.Reason)
	}
	if r := got["sre/other-bot"][bindingcheck.Ready]; r.Status != bindingcheck.False {
		t.Errorf("Ready of sre/other-bot is %s/%s; want False", r.Status, r.Reason)
	}
	if n := botB.callCount(); n != 0 {
		t.Errorf("the bot of the rejected connection was called %d times; want none", n)
	}
	if c := got["sre/oncall"][bindingcheck.ChannelJoined]; c.Status != bindingcheck.True {
		t.Errorf("sre/oncall: ChannelJoined is %s/%s; want True: its connection runs", c.Status, c.Reason)
	}
	eventually(t, 10*time.Second, "no WARN line for the rejected connection's ChannelJoined", func() bool {
		return countOf(buf.String(), "level=WARN", "a condition is false", "binding=sre/other-bot", "condition=ChannelJoined", "reason=ChatCredentialsRejected", "cause=", connB) == 1
	})
}

// The advice to restart Agent Kourier is the own connection's. A team connection restarts by itself when its Secret holds other tokens, and
// the message of a changed Secret and of a rejected token says so for a team connection, whichever checker finds it; the own connection's
// messages are as they were.
func TestTheAdviceForAChangedOrRejectedTokenFitsTheKindOfConnection(t *testing.T) {
	changed := func(s secretsByName) {
		s[connA].vals["botToken"] = "bot-a-rotated"
		s[connB].vals["botToken"] = "bot-b-rotated"
	}
	for _, tc := range []struct {
		name         string
		cond         bindingcheck.ConditionType
		secrets      func() secretsByName
		verdict      chat.Verdict
		ownSays      string
		teamSays     string
		notInTheTeam string
	}{
		{"a changed Secret, the channel", bindingcheck.ChannelJoined, func() secretsByName { s := bothSecretsHold(); changed(s); return s }, chat.Verdict{},
			"restart Agent Kourier to use the new ones", "restarts by itself to use the new ones", "restart Agent Kourier"},
		{"a changed Secret, the groups", bindingcheck.ApproversReadable, func() secretsByName { s := bothSecretsHold(); changed(s); return s }, chat.Verdict{},
			"restart Agent Kourier to use the new ones", "restarts by itself to use the new ones", "restart Agent Kourier"},
		{"a Secret that is gone", bindingcheck.ChannelJoined, func() secretsByName {
			s := bothSecretsHold()
			s[connA].missing, s[connB].missing = true, true
			return s
		}, chat.Verdict{}, "a restart fails until the Secret is mended", "restarts by itself once the Secret holds others", "a restart fails"},
		{"a rejected token, the channel", bindingcheck.ChannelJoined, bothSecretsHold, chat.Verdict{Class: chat.FatalAuth, Code: "invalid_auth"},
			"replace it and restart Agent Kourier", "the connection starts again by itself", "restart Agent Kourier"},
		{"a rejected token, the groups", bindingcheck.ApproversReadable, bothSecretsHold, chat.Verdict{Class: chat.FatalAuth, Code: "invalid_auth"},
			"replace it and restart Agent Kourier", "the connection starts again by itself", "restart Agent Kourier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := &snaps{}
			current.p.Store(twoConnectionsWithApprovers(t))
			fake := func() *accessFake {
				return &accessFake{access: chat.Access{Member: true}, verdict: tc.verdict, err: errIf(tc.verdict.Class == chat.FatalAuth, "rejected"), groupsErr: errIf(tc.verdict.Class == chat.FatalAuth, "rejected")}
			}
			p := verification{
				Own: refA, Bindings: &reachable{}, Platforms: chat.PlatformSet[platformReader]{connA: fake(), connB: fake()},
				Conns: bothConnections, Snaps: current, Secrets: tc.secrets(), Revision: func(context.Context) string { return "r" },
				Metrics: mustMetrics(t, nil), Log: slog.New(slog.DiscardHandler), Interval: time.Hour, Tick: 5 * time.Millisecond,
			}.newProber()
			got := settled(t, p, 4)

			own, team := got["sre/oncall"][tc.cond], got["sre/other-bot"][tc.cond]
			if !strings.Contains(own.Message, tc.ownSays) {
				t.Errorf("the own connection's Binding says %q; want it to say %q", own.Message, tc.ownSays)
			}
			if !strings.Contains(team.Message, tc.teamSays) || strings.Contains(team.Message, tc.notInTheTeam) {
				t.Errorf("the team connection's Binding says %q; want it to say %q, and not %q", team.Message, tc.teamSays, tc.notInTheTeam)
			}
		})
	}
}

func errIf(cond bool, msg string) error {
	if cond {
		return errors.New(msg)
	}
	return nil
}

// With no own connection every connection is a team's, which the supervisor restarts by itself: a changed Secret, a gone one and a rejected token
// are told as a team connection's are, for both connections, and nothing advises to restart Agent Kourier.
func TestWithNoOwnConnectionEveryConnectionIsToldItRestartsByItself(t *testing.T) {
	changed := func(s secretsByName) {
		s[connA].vals["botToken"] = "bot-a-rotated"
		s[connB].vals["botToken"] = "bot-b-rotated"
	}
	for _, tc := range []struct {
		name    string
		cond    bindingcheck.ConditionType
		secrets func() secretsByName
		verdict chat.Verdict
		says    string
	}{
		{"a changed Secret, the channel", bindingcheck.ChannelJoined, func() secretsByName { s := bothSecretsHold(); changed(s); return s }, chat.Verdict{}, "restarts by itself to use the new ones"},
		{"a changed Secret, the groups", bindingcheck.ApproversReadable, func() secretsByName { s := bothSecretsHold(); changed(s); return s }, chat.Verdict{}, "restarts by itself to use the new ones"},
		{"a Secret that is gone", bindingcheck.ChannelJoined, func() secretsByName {
			s := bothSecretsHold()
			s[connA].missing, s[connB].missing = true, true
			return s
		}, chat.Verdict{}, "restarts by itself once the Secret holds others"},
		{"a rejected token", bindingcheck.ChannelJoined, bothSecretsHold, chat.Verdict{Class: chat.FatalAuth, Code: "invalid_auth"}, "the connection starts again by itself"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := &snaps{}
			current.p.Store(twoConnectionsWithApprovers(t))
			fake := func() *accessFake {
				return &accessFake{access: chat.Access{Member: true}, verdict: tc.verdict, err: errIf(tc.verdict.Class == chat.FatalAuth, "rejected"), groupsErr: errIf(tc.verdict.Class == chat.FatalAuth, "rejected")}
			}
			p := verification{
				NoOwn: true, Bindings: &reachable{}, Platforms: chat.PlatformSet[platformReader]{connA: fake(), connB: fake()},
				Conns: bothConnections, Snaps: current, Secrets: tc.secrets(), Revision: func(context.Context) string { return "r" },
				Metrics: mustMetrics(t, nil), Log: slog.New(slog.DiscardHandler), Interval: time.Hour, Tick: 5 * time.Millisecond,
			}.newProber()
			got := settled(t, p, 4)

			for _, binding := range []string{"sre/oncall", "sre/other-bot"} {
				msg := got[binding][tc.cond].Message
				if !strings.Contains(msg, tc.says) || strings.Contains(msg, "restart Agent Kourier") || strings.Contains(msg, "a restart fails") {
					t.Errorf("%s says %q; want it to say %q, and not to advise a restart of Agent Kourier", binding, msg, tc.says)
				}
			}
		})
	}
}
