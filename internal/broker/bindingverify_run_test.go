package broker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/testutil"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// accessSlack is an idle Slack whose conversations.info answers come from the test, and which counts them.
type accessSlack struct {
	idleSlack
	mu     sync.Mutex
	access map[string]chat.Access // by channel; one that is not here is a channel the bot is not in
	calls  atomic.Int64

	groupsErr  error // what GroupsReadable answers
	groupCalls atomic.Int64
}

func (s *accessSlack) ChannelAccess(_ context.Context, channel string) (chat.Access, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.access[channel], nil
}

// GroupsReadable is conversations-style: it answers with the error the test set, for any groups, and counts the calls.
func (s *accessSlack) GroupsReadable(_ context.Context, _ []string) error {
	s.groupCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.groupsErr
}

// setGroups makes the groups unreadable with err, or readable with nil.
func (s *accessSlack) setGroups(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groupsErr = err
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
	c.probeEvery, c.probeTick, c.probeGap = 40*time.Millisecond, 10*time.Millisecond, time.Millisecond
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
	r.waitMoreProbes("ChannelJoined", 6) // several more probes: a probe that finds what it found before is not logged again

	out := logs.String()
	if n := countOf(out, "level=WARN", "a condition is false", "binding="+unverifiedBinding, "condition=ChannelJoined", "reason=NotInChannel"); n != 1 {
		t.Errorf("%d warnings that %s is not in its channel; want one\n%s", n, unverifiedBinding, out)
	}
	if n := countOf(out, "level=WARN", "a condition is false", "binding="+verifiedBinding, "reason=ChannelArchived"); n != 1 {
		t.Errorf("%d warnings that %s's channel is archived; want one\n%s", n, verifiedBinding, out)
	}
	if n := countOf(out, "level=INFO", "is now true", "binding="+verifiedBinding, "condition=ChannelJoined"); n != 1 {
		t.Errorf("%d notices that %s's channel is joined again; want one\n%s", n, verifiedBinding, out)
	}
	if n := countOf(out, "level=WARN", "binding verification", "binding="+verifiedBinding) - 1; n != 0 {
		t.Errorf("%s was warned of more than for its archived channel\n%s", verifiedBinding, out)
	}
	if strings.Contains(out, "xoxb-fake-bot") {
		t.Errorf("the log holds a token\n%s", out)
	}
}

// A Binding that asks for tool approvals from a user group has ApproversReadable, probed with the served connection's bot: False while
// the group cannot be read, True once it can, counted in the probe-duration histogram under its own condition, and counted toward the
// Binding's readiness. A Binding that names no group has none. An edit that takes the group away deletes the Binding's ApproversReadable
// series and leaves the others, and the probes of groups stop; the log says so once.
func TestRunProbesTheApproverGroupsOfABindingThatNamesThem(t *testing.T) {
	c, slk, logs := verifyWorld(t)
	c.reloadPoll = 10 * time.Millisecond
	file := filepath.Join(c.ConfigDir, "binding.yaml")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	const asks = "  interactions:\n    askUser: true\n"
	if !strings.Contains(string(raw), asks) {
		t.Fatal("the example Binding has no interactions to add approvals to")
	}
	withGroups := strings.Replace(string(raw), asks, asks+"    toolApprovals: true\n    approverGroups: [S0123456789]\n", 1)
	if err := os.WriteFile(file, []byte(withGroups), 0o600); err != nil {
		t.Fatal(err)
	}
	slk.setGroups(&chat.GroupError{Group: "S0123456789", Err: fmt.Errorf("fake: %w: %w", chat.ErrGroupNotFound, chat.ErrGroupsMisconfigured)})
	r := start(t, c)

	series := func(binding, status string) string {
		return `agentkourier_binding_condition{binding="` + binding + `",condition="ApproversReadable",status="` + status + `"} `
	}
	r.waitMetric(series(verifiedBinding, "false") + "1")
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 0`)
	if _, body := r.get("/metrics"); strings.Contains(body, `binding="`+unverifiedBinding+`",condition="ApproversReadable"`) {
		t.Errorf("a Binding that names no group has an ApproversReadable series:\n%s", body)
	}
	slk.setGroups(nil)
	r.waitMetric(series(verifiedBinding, "true") + "1")
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 1`)
	if r.probeCount("ApproversReadable") == 0 {
		t.Error("no probe of ApproversReadable was timed")
	}

	// The edit that takes the group away. It is staged and renamed into place: the broker reads the directory every 10ms, and a write in
	// place can be read half done, which removes the Binding (a prune) where the test wants only its groups gone (a drop).
	staging := filepath.Join(c.ConfigDir, ".binding.yaml.tmp") // a dot name is not a config file
	if err := os.WriteFile(staging, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, file); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the ApproversReadable series was not deleted after the groups were taken away", func() bool {
		_, body := r.get("/metrics")
		return !strings.Contains(body, `binding="`+verifiedBinding+`",condition="ApproversReadable"`)
	}, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 1`)
	r.waitMetric(`agentkourier_binding_condition{binding="` + verifiedBinding + `",condition="ChannelJoined",status="true"} 1`)
	calls := slk.groupCalls.Load()
	r.waitMoreProbes("ChannelJoined", 4)
	if after := slk.groupCalls.Load(); after != calls {
		t.Errorf("%d reads of groups after the Binding stopped naming any; want none", after-calls)
	}
	out := logs.String()
	if n := countOf(out, "level=WARN", "a condition is false", "binding="+verifiedBinding, "condition=ApproversReadable", "reason=GroupNotFound"); n != 1 {
		t.Errorf("%d warnings that the group is not found; want one\n%s", n, out)
	}
	if n := countOf(out, "level=INFO", "stopped verifying a condition", "binding="+verifiedBinding, "condition=ApproversReadable"); n != 1 {
		t.Errorf("%d notices that the condition was dropped; want one\n%s", n, out)
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
	testutil.Eventually(t, "the first replica did not start the leader half", func() bool { return a.built.Load() == 1 }, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))
	cb, b := electionConfig(t, client, fence, "pod-b_1")
	slkB := &accessSlack{access: map[string]chat.Access{}}
	cb.newSlack = func(slack.Config) slackAPI { b.built.Add(1); return slkB }
	cb.probeEvery, cb.probeTick = 40*time.Millisecond, 10*time.Millisecond
	b.running = start(t, cb)
	b.until("/readyz", http.StatusOK)

	testutil.Eventually(t, "the leader exports no verification series", func() bool {
		return a.metric(`agentkourier_binding_ready{binding="`+verifiedBinding+`"} 0`) || a.metric(`agentkourier_binding_ready{binding="`+verifiedBinding+`"} 1`)
	}, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))
	time.Sleep(testElection.RetryPeriod * 5)
	if _, body := b.get("/metrics"); strings.Contains(body, "agentkourier_binding_") {
		t.Errorf("the standby's /metrics holds verification series:\n%s", body)
	}
	if slkB.calls.Load() != 0 {
		t.Errorf("the standby read %d channels; want none", slkB.calls.Load())
	}
}

// probeCount is the number of probes of the condition that the leader has timed (the count of agentkourier_binding_probe_duration_seconds), which
// rises with every probe whatever it finds.
func (r *running) probeCount(condition string) float64 {
	r.t.Helper()
	_, body := r.get("/metrics")
	prefix := `agentkourier_binding_probe_duration_seconds_count{condition="` + condition + `"} `
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				r.t.Fatalf("the probe count %q: %v", line, err)
			}
			return n
		}
	}
	return 0
}

// waitMoreProbes waits until n more probes of the condition have ended than had when it was called, which is the signal that the
// probes went on for long enough for the test to look at what they left.
func (r *running) waitMoreProbes(condition string, n float64) {
	r.t.Helper()
	base := r.probeCount(condition)
	testutil.Eventually(r.t, "the leader did not make "+strconv.FormatFloat(n, 'f', 0, 64)+" more probes of "+condition, func() bool {
		return r.probeCount(condition) >= base+n
	}, testutil.Within(20*time.Second), testutil.Every(10*time.Millisecond))
}

// The chat credentials Secret as the broker's tokens were read from it, with its tokens changed to bot and app and any annotations.
func slackSecret(version, bot string, annotations map[string]string) *corev1.Secret {
	s := secretOf("agent-kourier-system", "slack-main")
	s.Data = map[string][]byte{"botToken": []byte(bot), "appToken": []byte("xapp-fake-app")}
	s.ResourceVersion, s.Annotations = version, annotations
	return s
}

// Tokens in the chat credentials Secret that differ from the ones the broker is using are told of: the channel probe says
// Unknown/ChatCredentialsChanged, with a WARN, and stops calling Slack, since the bot the broker holds is no longer the Secret's, and a
// restart is what picks the new ones up. A write to the Secret that leaves the tokens alone (a new resourceVersion, an annotation) is no
// change: the Binding stays ready, though the write moves the config's revision and so makes the leader probe at once. No token, old or
// new, reaches the log or /metrics.
func TestRunTellsOfChatTokensThatChangedAndNotOfAWriteThatLeavesThemAlone(t *testing.T) {
	const old, rotated = "xoxb-fake-bot", "xoxb-ROTATED-CANARY"
	c, slk, logs := verifyWorld(t)
	c.probeEvery = time.Hour
	client := slackCluster(old)
	c.Client = client
	r := start(t, c)
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 1`)
	secrets := client.CoreV1().Secrets("agent-kourier-system")

	// The probes are an hour apart here, so that only the config's revision can make the leader probe again (the first pass came at
	// start). A write that leaves the tokens alone moves the revision, so the leader probes again at once, and finds nothing wrong. Two
	// such writes, so that the first one's pass has certainly been recorded when the second one's has begun.
	update := func(s *corev1.Secret) {
		t.Helper()
		if _, err := secrets.Update(t.Context(), s, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for rv, note := range []string{"first", "second"} {
		before := slk.calls.Load()
		update(slackSecret(strconv.Itoa(rv+2), old, map[string]string{"owner": note}))
		testutil.Eventually(t, "the leader did not probe again after a write to the Secret", func() bool { return slk.calls.Load() > before }, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))
	}
	if !strings.Contains(metricsOf(r), `agentkourier_binding_ready{binding="`+verifiedBinding+`"} 1`) {
		t.Errorf("%s is not ready after a write that left the tokens alone", verifiedBinding)
	}

	update(slackSecret("4", rotated, nil))
	r.waitMetric(`agentkourier_binding_condition{binding="` + verifiedBinding + `",condition="ChannelJoined",status="unknown"} 1`)
	r.waitMetric(`agentkourier_binding_ready{binding="` + verifiedBinding + `"} 0`)
	// Another write moves the revision again. The example config has two Bindings, so a pass is two probes of the channel: when two
	// more are counted, the pass this write calls for, which Slack is not asked for, has run.
	calls, probes := slk.calls.Load(), r.probeCount("ChannelJoined")
	update(slackSecret("5", rotated, map[string]string{"owner": "again"}))
	testutil.Eventually(t, "the leader did not probe again", func() bool { return r.probeCount("ChannelJoined") >= probes+2 }, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))
	if after := slk.calls.Load(); after != calls {
		t.Errorf("Slack was asked %d more times after the tokens changed; want none", after-calls)
	}

	// The Secret deleted: a restart would now fail, which is a reason of its own and a second line.
	if err := secrets.Delete(t.Context(), "slack-main", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the unusable credentials were not told of", func() bool {
		return countOf(logs.String(), "level=WARN", "binding="+verifiedBinding, "reason=ChatCredentialsUnusable") == 1
	}, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))

	out := logs.String()
	if n := countOf(out, "level=WARN", "a condition is unknown", "binding="+verifiedBinding, "reason=ChatCredentialsChanged"); n != 1 {
		t.Errorf("%d warnings that the chat credentials changed; want one\n%s", n, out)
	}
	for _, text := range []string{old, rotated, "xapp-fake-app"} {
		if strings.Contains(out, text) || strings.Contains(metricsOf(r), text) {
			t.Errorf("the log or /metrics holds a token (%q...)", text[:8])
		}
	}
}

// metricsOf is the body of /metrics.
func metricsOf(r *running) string {
	r.t.Helper()
	_, body := r.get("/metrics")
	return body
}

// countOf counts the lines of text that hold every one of parts.
func countOf(text string, parts ...string) int {
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
	testutil.Eventually(t, "the verification did not probe", func() bool { return slk.calls.Load() >= 4 }, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))

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
