package broker

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// accessSlack is an idle Slack whose conversations.info answers come from the test, and which counts them.
type accessSlack struct {
	idleSlack
	mu     sync.Mutex
	access map[string]chat.Access // by channel; one that is not here is a channel the bot is not in
	calls  atomic.Int64
}

func (s *accessSlack) ChannelAccess(_ context.Context, channel string) (chat.Access, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.access[channel], nil
}

func (s *accessSlack) set(channel string, a chat.Access) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.access[channel] = a
}

// The example config's two Bindings (sre/sre-alerts in C0123456789 and sre/chat-only in G0123456789), the first on a stub agent that
// serves its card.
const (
	verifiedBinding   = "sre/sre-alerts"
	verifiedChannel   = "C0123456789"
	unverifiedBinding = "sre/chat-only"
)

// verifyWorld is a RunConfig for the verification of the Bindings: the example config with its backend on a stub agent that serves
// the card, a Slack that says where the bot is a member, and a log the test reads. The probes run every 40 ms, and the config is
// looked at every 10.
func verifyWorld(t *testing.T) (RunConfig, *accessSlack, *logBuffer) {
	t.Helper()
	stub := stubagent.New()
	t.Cleanup(stub.Close)
	stub.Agent("kagent", "k8s-agent")
	c := startConfig(t)
	be := filepath.Join(c.ConfigDir, "agentbackend.yaml")
	raw, err := os.ReadFile(be)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(be, []byte(strings.Replace(string(raw), "http://kagent-controller.kagent-pilot:8083", stub.URL(), 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Client = slackCluster("xoxb-fake-bot")
	slk := &accessSlack{access: map[string]chat.Access{verifiedChannel: {Member: true}}}
	c.newSlack = func(slack.Config) slackAPI { return slk }
	logs := &logBuffer{}
	c.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c.probeEvery, c.probeTick = 40*time.Millisecond, 10*time.Millisecond
	return c, slk, logs
}

// The conditions of the Bindings show in /metrics and in the log as the leader finds them, while /readyz stays what it was: a Binding in a
// channel the bot is not in is not ready, the other one is, and a change of the channel (archived, then not) is seen on the next probe,
// warned of once as it goes False and told of once as it comes back. A Binding that is fine is never warned of.
func TestRunReportsTheConditionsOfTheBindingsAsMetricsAndLogLines(t *testing.T) {
	c, slk, logs := verifyWorld(t)
	r := start(t, c)

	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 1`)
	r.waitMetric(`agentkourier_binding_ready{binding="` + unverifiedBinding + `"} 0`)
	r.waitMetric(`agentkourier_binding_condition{binding="` + verifiedBinding + `",condition="BackendReachable",status="true"} 1`)
	r.waitMetric(`agentkourier_binding_condition{binding="` + verifiedBinding + `",condition="ChannelJoined",status="true"} 1`)
	r.waitMetric(`agentkourier_binding_condition{binding="` + unverifiedBinding + `",condition="ChannelJoined",status="false"} 1`)
	r.waitMetric(`agentkourier_binding_condition{binding="` + unverifiedBinding + `",condition="BackendReachable",status="true"} 1`)
	if code, _ := r.get("/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d with a Binding that is not ready; want 200: readiness is the process's", code)
	}

	slk.set(verifiedChannel, chat.Access{Member: true, Archived: true})
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 0`)
	r.waitMetric(`agentkourier_binding_condition{binding="` + verifiedBinding + `",condition="ChannelJoined",status="false"} 1`)
	slk.set(verifiedChannel, chat.Access{Member: true})
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 1`)
	time.Sleep(200 * time.Millisecond) // several more probes: a probe that finds what it found before is not logged again

	out := logs.String()
	count := func(parts ...string) int {
		n := 0
		for _, line := range strings.Split(out, "\n") {
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
	if n := count("level=WARN", "a condition is false", "binding="+unverifiedBinding, "condition=ChannelJoined", "reason=NotInChannel"); n != 1 {
		t.Errorf("%d warnings that %s is not in its channel; want one\n%s", n, unverifiedBinding, out)
	}
	if n := count("level=WARN", "a condition is false", "binding="+verifiedBinding, "reason=ChannelArchived"); n != 1 {
		t.Errorf("%d warnings that %s's channel is archived; want one\n%s", n, verifiedBinding, out)
	}
	if n := count("level=INFO", "true again", "binding="+verifiedBinding, "condition=ChannelJoined"); n != 1 {
		t.Errorf("%d notices that %s's channel is joined again; want one\n%s", n, verifiedBinding, out)
	}
	if n := count("level=WARN", "binding verification", "binding="+verifiedBinding) - 1; n != 0 {
		t.Errorf("%s was warned of more than for its archived channel\n%s", verifiedBinding, out)
	}
	if strings.Contains(out, "xoxb-fake-bot") {
		t.Errorf("the log holds a token\n%s", out)
	}
}

// A standby probes nothing and exports no series of the verification (the leader's alone); the leader that holds the lease exports them.
func TestRunOnlyTheLeaderProbesTheBindings(t *testing.T) {
	client, fence := cluster(), &fakeFence{}
	ca, a := electionConfig(t, client, fence, "pod-a_1")
	slkA := &accessSlack{access: map[string]chat.Access{verifiedChannel: {Member: true}}}
	ca.newSlack = func(slack.Config) slackAPI { a.built.Add(1); return slkA }
	ca.probeEvery, ca.probeTick = 40*time.Millisecond, 10*time.Millisecond
	a.running = start(t, ca)
	eventually(t, 10*time.Second, "the first replica did not start the leader half", func() bool { return a.built.Load() == 1 })
	cb, b := electionConfig(t, client, fence, "pod-b_1")
	slkB := &accessSlack{access: map[string]chat.Access{}}
	cb.newSlack = func(slack.Config) slackAPI { b.built.Add(1); return slkB }
	cb.probeEvery, cb.probeTick = 40*time.Millisecond, 10*time.Millisecond
	b.running = start(t, cb)
	b.until("/readyz", http.StatusOK)

	eventually(t, 10*time.Second, "the leader exports no verification series", func() bool {
		return a.metric(`agentkourier_binding_ready{binding="`+verifiedBinding+`"} 0`) || a.metric(`agentkourier_binding_ready{binding="`+verifiedBinding+`"} 1`)
	})
	time.Sleep(testElection.RetryPeriod * 5)
	if _, body := b.get("/metrics"); strings.Contains(body, "agentkourier_binding_") {
		t.Errorf("the standby's /metrics holds verification series:\n%s", body)
	}
	if slkB.calls.Load() != 0 {
		t.Errorf("the standby read %d channels; want none", slkB.calls.Load())
	}
}

// credentialsCluster is cluster() with a resourceVersion on the chat credentials Secret, which the fake API server does not set itself.
func credentialsCluster(version string) *fake.Clientset {
	conn := slackSecret(version)
	return fake.NewClientset(conn, secretOf("sre", "agent-kourier-sre-alerts-oidc", "token"))
}

func slackSecret(version string) *corev1.Secret {
	s := secretOf("agent-kourier-system", "slack-main", "botToken", "appToken")
	s.ResourceVersion = version
	return s
}

// A Secret of the chat credentials that changes after the broker read the tokens is told of: the channel probe says
// Unknown/ChatCredentialsChanged and stops calling Slack, since the tokens the broker holds are no longer the Secret's, and a restart is
// what picks the new ones up. The baseline is the version the tokens were read under, taken from the cluster by Run itself.
func TestRunTellsOfChatCredentialsThatChangedAfterTheTokensWereRead(t *testing.T) {
	c, slk, _ := verifyWorld(t)
	client := credentialsCluster("1")
	c.Client = client
	r := start(t, c)
	r.waitMetric(`agentkourier_binding_condition{binding="` + verifiedBinding + `",condition="ChannelJoined",status="true"} 1`)

	if _, err := client.CoreV1().Secrets("agent-kourier-system").Update(t.Context(), slackSecret("2"), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.waitMetric(`agentkourier_binding_condition{binding="` + verifiedBinding + `",condition="ChannelJoined",status="unknown"} 1`)
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 0`)
	before := slk.calls.Load()
	time.Sleep(200 * time.Millisecond)
	if after := slk.calls.Load(); after != before {
		t.Errorf("Slack was asked %d more times after the credentials changed; want none", after-before)
	}
}

// The verification stops with the interactions' loops, before the manager, the drain, the pool and the resolver: after the resolver stops
// every Secret reads as unreadable, the revision moves, and a prober still running would probe a closed pool and a closed Slack client.
// The probes are made every 5 ms, and the steps after the interactions' are held for 300 ms, so a prober that outlives its step is caught
// asking Slack in between.
func TestRunStopsTheVerificationBeforeTheManagerThePoolAndTheResolver(t *testing.T) {
	c, slk, _ := verifyWorld(t)
	c.probeEvery, c.probeTick = 5*time.Millisecond, 5*time.Millisecond
	var atManager, atResolver atomic.Int64
	trace := &stepTrace{hook: func(step string) {
		switch step {
		case "manager":
			atManager.Store(slk.calls.Load())
			time.Sleep(300 * time.Millisecond)
		case "resolver":
			atResolver.Store(slk.calls.Load())
		}
	}}
	c.shutdownTrace = trace.record
	r := start(t, c)
	eventually(t, 10*time.Second, "the verification did not probe", func() bool { return slk.calls.Load() >= 4 })

	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v after its context ended, want nil", err)
	}

	if atManager.Load() == 0 {
		t.Fatal("the manager's step never began, so the test saw nothing")
	}
	if atManager.Load() != atResolver.Load() {
		t.Errorf("Slack was asked %d times while the manager, the drain and the pool stopped; the verification must have stopped in the interactions' step", atResolver.Load()-atManager.Load())
	}
}
