//go:build e2e

package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
)

// fakeBot is the bot user of the fake Slack: a channel it is a member of is one the bot is in.
const fakeBot = "UFAKEBOT"

// verifySeries is a series of the verification, read from the process that does the work.
type verifySeries struct {
	name   string
	labels map[string]string
	want   float64
}

// holds reports whether the series has the value it should now. A series that is not there yet is not an error: the first probe of a Binding
// has not ended.
func (s *scenario) holds(ctx context.Context, v verifySeries) (bool, error) {
	got, err := s.leaderCounter(ctx, v.name, v.labels)
	if errors.Is(err, ErrNoSample) {
		return false, nil
	}
	return got == v.want, err
}

// TestBV_BindingsThatCannotBeServedAreNotReadyAndTheLeaderSaysWhy: the leader probes each Binding of its connection and shows the result
// as metrics and log lines, while /readyz stays the process's. Four Bindings, each in a channel of its own: one that can be served, one
// whose agent the stub does not have (the card is a 404: BackendReachable is false, the channel is joined), one whose channel the bot is
// not in, and one whose channel is archived (ChannelJoined is false, the agent answers). The probes post nothing: no message lands in
// any of the four channels (that they run no turn and send the agent no message is asserted by the broker's own tests, not here). A
// probe is made when Agent Kourier starts, so the scenario sets up the fake Slack and then restarts it, as the next probe is five
// minutes off.
//
// The fast tier only. The Bindings are loaded on the full tier too (lib.sh's deploy_agentkourier gives it the fast tier's config), but the
// assertions rest on what the stub agent answers (a 404 card for an agent it does not have; kagent's front door answers 403), which the
// full tier does not run.
func TestBV_BindingsThatCannotBeServedAreNotReadyAndTheLeaderSaysWhy(t *testing.T) {
	s := newScenario(t)
	if s.env.Tier != "fast" {
		t.Skipf("the assertions rest on the stub agent's answers, such as a 404 card for an agent it does not have; the %s tier has no stub", s.env.Tier)
	}
	for channel, members := range map[string][]string{
		ChannelVerifyReady: {fakeBot}, ChannelVerifyNoAgent: {fakeBot}, ChannelVerifyNotJoined: {"UVFYOTHER"}, ChannelVerifyArchived: {fakeBot},
	} {
		if err := s.slack.SetMembers(s.ctx, channel, members...); err != nil {
			t.Fatal(err)
		}
	}
	for channel, archived := range map[string]bool{ChannelVerifyArchived: true, ChannelVerifyReady: false} { // the fake outlives a run: say it again
		if err := s.slack.SetArchived(s.ctx, channel, archived); err != nil {
			t.Fatal(err)
		}
	}

	restart, cancel := context.WithTimeout(s.ctx, 3*time.Minute) // a stuck rollout fails here, with its reason
	defer cancel()
	if err := s.pods.RolloutRestart(restart, kube.AgentKourierNamespace, kube.AgentKourierDeployment); err != nil {
		t.Fatal(err)
	}
	s.connected()

	cond := func(binding, condition, status string, want float64) verifySeries {
		return verifySeries{"agentkourier_binding_condition", map[string]string{"binding": binding, "condition": condition, "status": status}, want}
	}
	ready := func(binding string, want float64) verifySeries {
		return verifySeries{"agentkourier_binding_ready", map[string]string{"binding": binding}, want}
	}
	for _, v := range []verifySeries{
		ready("agent-kourier/fast-verify-ready", 1),
		cond("agent-kourier/fast-verify-ready", "BackendReachable", "true", 1),
		cond("agent-kourier/fast-verify-ready", "ChannelJoined", "true", 1),

		ready("agent-kourier/fast-verify-no-agent", 0),
		cond("agent-kourier/fast-verify-no-agent", "BackendReachable", "false", 1),
		cond("agent-kourier/fast-verify-no-agent", "ChannelJoined", "true", 1),

		ready("agent-kourier/fast-verify-not-joined", 0),
		cond("agent-kourier/fast-verify-not-joined", "ChannelJoined", "false", 1),
		cond("agent-kourier/fast-verify-not-joined", "BackendReachable", "true", 1),

		ready("agent-kourier/fast-verify-archived", 0),
		cond("agent-kourier/fast-verify-archived", "ChannelJoined", "false", 1),
		cond("agent-kourier/fast-verify-archived", "BackendReachable", "true", 1),
	} {
		desc := v.name + " " + v.labels["binding"] + " " + v.labels["condition"] + " " + v.labels["status"]
		if err := Wait(s.ctx, 2*time.Minute, desc, func(ctx context.Context) (bool, error) { return s.holds(ctx, v) }); err != nil {
			t.Error(err)
		}
	}

	// The same probe says why in the log, once for each Binding that is not ready, and says nothing of the one that is.
	logs := s.agentKourierLogs()
	for binding, reason := range map[string]string{
		"agent-kourier/fast-verify-no-agent":   "AgentNotFound",
		"agent-kourier/fast-verify-not-joined": "NotInChannel",
		"agent-kourier/fast-verify-archived":   "ChannelArchived",
	} {
		if n := countLines(logs, `"binding":"`+binding+`"`, `"reason":"`+reason+`"`, `"msg":"binding verification: a condition is false"`); n != 1 {
			t.Errorf("%d log lines that %s is false for %s; want one", n, binding, reason)
		}
	}
	if n := countLines(logs, `"binding":"agent-kourier/fast-verify-ready"`, `"msg":"binding verification: a condition is false"`); n != 0 {
		t.Errorf("%d log lines that the Binding that can be served is false; want none", n)
	}

	// The probes only read: nothing was posted to any of the channels.
	for _, channel := range []string{ChannelVerifyReady, ChannelVerifyNoAgent, ChannelVerifyNotJoined, ChannelVerifyArchived} {
		if msgs, err := s.slack.Messages(s.ctx, channel); err != nil || len(msgs) != 0 {
			t.Errorf("%d messages in %s (%v); want none: a probe posts nothing", len(msgs), channel, err)
		}
	}

	// A Binding that is not ready changes nothing about readiness: the process is as ready as before.
	if err := s.agentKourier.WaitReady(s.ctx, 10*time.Second); err != nil {
		t.Errorf("/readyz with Bindings that are not ready: %v", err)
	}
}

// agentKourierLogs is the tail of the log of every Agent Kourier pod, which is where a standby has nothing of the work.
func (s *scenario) agentKourierLogs() string {
	s.t.Helper()
	pods, err := s.pods.List(s.ctx, kube.AgentKourierNamespace, agentKourierPods)
	if err != nil {
		s.t.Fatal(err)
	}
	var all strings.Builder
	for _, p := range pods {
		if p.DeletionTimestamp != nil { // the pod a restart replaced: its log is an earlier run's
			continue
		}
		out, err := s.pods.Logs(s.ctx, kube.AgentKourierNamespace, p.Name, agentKourierContainer, false, 3000)
		if err != nil {
			s.t.Fatal(err)
		}
		all.WriteString(out)
	}
	return all.String()
}

// countLines counts the lines of text that hold every one of parts.
func countLines(text string, parts ...string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			n++
		}
	}
	return n
}
