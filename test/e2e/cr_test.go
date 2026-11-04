//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// The scenarios of the custom resources route (E2E_CONFIG_SOURCE=crds): a team in a namespace of its own makes Bindings, and Agent Kourier, which is
// not restarted for any of it, serves them. They run in the fast tier only, since they reach the stub agent, and skip on the ConfigMap route, where
// there is no such thing as a team's Binding. The team is the namespace e2e-team (kube.TeamNamespace), which the runner may write in and nowhere
// else, and whose Bindings the tier's ChatConnection allows (allowedNamespaces).

// What the stub's agents say, which is how a scenario tells one Binding's agent from another's: test-agent says it to anything, and scenario-alert
// answers with the first line of the prompt.
const (
	crAnswer       = "The answer is 4."
	crAnswerAlert  = "Investigating: "
	crAgentAnswer  = "test-agent"
	crAgentInvest  = "scenario-alert"
	crUser         = "UCR"
	crServeTimeout = 2 * time.Minute // a change of a custom resource is seen within the reload's 10 seconds; the rest is a probe or two
)

// Channels of the scenarios below, one each, so that no two Bindings share a thread. None is in the tier's config: a Binding the scenario makes takes
// the channel.
const (
	ChannelCRNewNamespace = "CCRNEWNS"
	ChannelCRInvalid      = "CCRINVALID"
	ChannelCRConflict     = "CCRCONFLICT"
	ChannelCRStatus       = "CCRSTATUS"
	ChannelCRStatusJoined = "CCRSTATUSOK"
	ChannelCRFailover     = "CCRFAILOVER"
	ChannelCRMigration    = "CCRMIGRATE"
)

// crScenario is a scenario in the fast tier on the custom resources route, with the client of a team.
type crScenario struct {
	*scenario
	crs *kube.CRs
}

// newCRScenario returns what a scenario of the custom resources route needs, or skips on the ConfigMap route. Both routes are the same tier, brought
// up with E2E_CONFIG_SOURCE (see hack/kind/up-fast.sh).
func newCRScenario(t *testing.T) *crScenario {
	t.Helper()
	env := NewEnv(os.Getenv)
	if err := CheckTier(env.Tier); err != nil {
		t.Fatal(err)
	}
	if env.Tier != "fast" {
		t.Skipf("the custom resource scenarios reach the stub agent, which the %s tier does not run", env.Tier)
	}
	if env.ConfigSource != "crds" {
		t.Skip("Agent Kourier reads its config from the ConfigMap here; these scenarios are about the custom resources. Bring the tier up with: E2E_CONFIG_SOURCE=crds make e2e-fast-up")
	}
	crs, err := kube.NewCRsInCluster()
	if err != nil {
		t.Fatal(err)
	}
	return &crScenario{scenario: newScenario(t), crs: crs}
}

// team is what a team of the namespace e2e-team has: a token Secret that Agent Kourier may read (the Role and RoleBinding the team adds), and an
// AgentBackend that is the stub agent. Everything a case makes is removed when it ends, and what an earlier run left of the same names, before it starts.
type team struct {
	*crScenario
	prefix  string // the prefix of every object the case makes
	secret  string
	backend string
	made    []string // the Bindings the case made
}

// newTeam makes the team's Secret (a fake unsigned JWT, which is all the stub agent asks of a bearer), grants Agent Kourier that one Secret, and makes
// the AgentBackend. It is the RBAC a team must add: without the grant Agent Kourier cannot read the token.
func (c *crScenario) newTeam(prefix string) *team {
	c.t.Helper()
	tm := &team{crScenario: c, prefix: prefix, secret: prefix + "-token", backend: prefix + "-agents"}
	c.t.Cleanup(tm.remove)
	// Whatever an earlier run left, under these names or any other: a run killed by its timeout leaves its Bindings, and one of the same name fails the Create.
	for _, gvr := range []schema.GroupVersionResource{kube.BindingGVR, kube.AgentBackendGVR} {
		if err := c.crs.Purge(context.WithoutCancel(c.ctx), gvr); err != nil {
			c.t.Fatal(err)
		}
	}
	tm.remove()
	enc := base64.RawURLEncoding
	token := enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString([]byte(`{"email":"e2e-team@example.test"}`)) + "."
	if err := c.crs.CreateSecret(c.ctx, tm.secret, map[string]string{"token": token}); err != nil {
		c.t.Fatal(err)
	}
	if err := c.crs.GrantSecret(c.ctx, tm.secret); err != nil {
		c.t.Fatal(err)
	}
	tm.create(kube.AgentBackendGVR, tm.backend, map[string]any{"dialect": "kagent-v1", "url": c.env.StubURL})
	return tm
}

// remove deletes what the case made, Bindings first so that nothing serves on a Secret that is gone, and what an earlier run of it left: each is by
// name, and a missing one is not an error.
func (tm *team) remove() {
	tm.t.Helper()
	ctx := context.WithoutCancel(tm.ctx)
	for _, name := range append(tm.made, tm.prefix) {
		if err := tm.crs.Delete(ctx, kube.BindingGVR, kube.TeamNamespace, name); err != nil {
			tm.t.Errorf("clean up Binding %s: %v", name, err)
		}
	}
	tm.made = nil
	if err := tm.crs.Delete(ctx, kube.AgentBackendGVR, kube.TeamNamespace, tm.backend); err != nil {
		tm.t.Errorf("clean up AgentBackend %s: %v", tm.backend, err)
	}
	if err := tm.crs.RemoveSecret(ctx, tm.secret); err != nil {
		tm.t.Errorf("clean up Secret %s: %v", tm.secret, err)
	}
}

// create makes an object of the team's namespace from its spec.
func (tm *team) create(gvr schema.GroupVersionResource, name string, spec map[string]any) *unstructured.Unstructured {
	tm.t.Helper()
	kind := "Binding"
	if gvr == kube.AgentBackendGVR {
		kind = "AgentBackend"
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentkourier.dev/v1alpha1", "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": kube.TeamNamespace},
		"spec":     spec,
	}}
	got, err := tm.crs.Create(tm.ctx, gvr, obj)
	if err != nil {
		tm.t.Fatal(err)
	}
	if gvr == kube.BindingGVR {
		tm.made = append(tm.made, name)
	}
	return got
}

// bindingSpec is the spec of a Binding of the team on a channel, answered by the stub's agent of that name, with the team's own backend and token.
// A mention is how a person starts a session, which is the default trigger.
func (tm *team) bindingSpec(channel, agent string) map[string]any {
	return map[string]any{
		"agent":    map[string]any{"backendRef": map[string]any{"name": tm.backend}, "namespace": "kagent", "name": agent},
		"identity": map[string]any{"userId": "e2e-team", "tokenSecretRef": map[string]any{"name": tm.secret}},
		"chat": map[string]any{
			"connectionRef": map[string]any{"namespace": kube.AgentKourierNamespace, "name": "slack-fast"},
			"channel":       channel, "output": "live",
		},
	}
}

// addBinding makes a Binding of the team.
func (tm *team) addBinding(name, channel, agent string) *unstructured.Unstructured {
	tm.t.Helper()
	return tm.create(kube.BindingGVR, name, tm.bindingSpec(channel, agent))
}

// rewrite replaces a Binding's spec, as a team's apply of an edited file does, and returns the object as stored (its generation counts the change)
// or the API server's refusal.
func (tm *team) rewrite(name string, spec map[string]any) (*unstructured.Unstructured, error) {
	tm.t.Helper()
	next := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentkourier.dev/v1alpha1", "kind": "Binding",
		"metadata": map[string]any{"name": name, "namespace": kube.TeamNamespace},
		"spec":     spec,
	}}
	return tm.crs.Replace(tm.ctx, kube.BindingGVR, next) // against whatever is there now, as the status writer may have changed it since a read
}

// mustRewrite is rewrite for a change the API server must accept.
func (tm *team) mustRewrite(name string, spec map[string]any) *unstructured.Unstructured {
	tm.t.Helper()
	got, err := tm.rewrite(name, spec)
	if err != nil {
		tm.t.Fatal(err)
	}
	return got
}

// answered mentions Agent Kourier in channel until a Binding answers, and returns what its bot says in the thread: a mention that no Binding serves yet
// is ignored, with no reply, so the mention is made again until one gets an answer or the time is out. The thread's last reply is returned when it has ended.
func (c *crScenario) answered(channel string, within time.Duration) string {
	c.t.Helper()
	deadline := time.Now().Add(within)
	for attempt := 1; ; attempt++ {
		m := c.mention(channel, crUser, fmt.Sprintf("hello %d", attempt))
		th, err := c.slack.WaitThread(c.ctx, 12*time.Second, channel, m.TS, func(th []fakeslack.MessageView) bool {
			bots := BotMessages(th)
			return len(bots) >= 1 && finished(bots[len(bots)-1])
		})
		if err == nil {
			bots := BotMessages(th)
			return bots[len(bots)-1].Text
		}
		if c.ctx.Err() != nil || time.Now().After(deadline) {
			c.t.Fatalf("no Binding answered in %s within %s: %v\nthe last thread: %s", channel, within, err, describe(th))
		}
	}
}

// answeredBy is answered until the bot says want, which is the text of the agent that is meant to answer: after a change the Binding it replaces may
// answer first, for as long as the change is on its way.
func (c *crScenario) answeredBy(channel, want string, within time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(within)
	var got string
	for time.Now().Before(deadline) {
		got = c.answered(channel, time.Until(deadline))
		if strings.Contains(got, want) {
			return
		}
		time.Sleep(3 * time.Second) // the next attempt: the Binding being replaced may answer until the change is seen
	}
	c.t.Fatalf("the Binding in %s answered %q, want it to say %q within %s", channel, got, want, within)
}

// rejections reads agentkourier_config_binding_rejections for a reason, from the process that reads the config: it counts the Bindings that are not
// accepted as written, which is how a scenario knows that the reload has seen a change that leaves nothing else to see.
func (c *crScenario) rejections(reason string) float64 {
	c.t.Helper()
	return c.counter("agentkourier_config_binding_rejections", map[string]string{"reason": reason})
}

// waitRejections waits until the count of reason is want.
func (c *crScenario) waitRejections(reason string, want float64) {
	c.t.Helper()
	err := Wait(c.ctx, 90*time.Second, fmt.Sprintf("agentkourier_config_binding_rejections{reason=%q} to be %v", reason, want), func(ctx context.Context) (bool, error) {
		got, err := c.leaderCounter(ctx, "agentkourier_config_binding_rejections", map[string]string{"reason": reason})
		if errors.Is(err, ErrNoSample) {
			return want == 0, nil
		}
		return got == want, err
	})
	if err != nil {
		c.t.Fatal(err)
	}
}

// TestCR_ABindingInANewNamespaceServesWithNoRestart: a team makes a namespace's first Binding: its token Secret, the Role and RoleBinding that let Agent
// Kourier list and watch that Secret by name (the tier grants no namespace as a whole on this route), an AgentBackend and the Binding. Agent Kourier is
// not restarted: the same containers run before and after, and the Binding answers a mention in its channel with its agent's text. The namespace e2e-team
// is empty when the tier comes up, and nothing the tier is configured with is in it. In a run of the whole suite the other cases have made Secrets here
// before, so an informer of Agent Kourier may have been asked about a Secret of this namespace: what this case shows is the Binding's own Secret, which
// is new, read with no restart.
func TestCR_ABindingInANewNamespaceServesWithNoRestart(t *testing.T) {
	c := newCRScenario(t)
	before, err := c.reps.All(c.ctx)
	if err != nil || len(before) == 0 {
		t.Fatalf("the replicas before = %v, %v", before, err)
	}
	tm := c.newTeam("newns")
	tm.addBinding("newns", ChannelCRNewNamespace, crAgentAnswer)

	c.answeredBy(ChannelCRNewNamespace, crAnswer, crServeTimeout)

	after, err := c.reps.All(c.ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(rs []kube.Replica) string {
		var out []string
		for _, r := range rs {
			out = append(out, fmt.Sprintf("%s restarts=%d", r.ID(), r.Restarts))
		}
		return strings.Join(out, ", ")
	}
	if ids(before) != ids(after) {
		t.Errorf("the replicas were %s before the Binding and %s after: Agent Kourier restarted for a namespace it had never seen", ids(before), ids(after))
	}
}

// TestCR_AnInvalidUpdateKeepsTheLastGoodGeneration: a Binding that serves test-agent is updated, in one write, to another agent and to two identical
// triggers, which the broker refuses and the API server does not (a rule of the broker's only: the schema has no way to say it). The API server accepts
// the write and the generation moves, and Agent Kourier counts the Binding as rejected, InvalidSpec, and goes on serving the generation that was good:
// test-agent's text, not the other agent's. A repaired update is served once it is seen.
func TestCR_AnInvalidUpdateKeepsTheLastGoodGeneration(t *testing.T) {
	c := newCRScenario(t)
	tm := c.newTeam("invalid")
	good := tm.addBinding(tm.prefix, ChannelCRInvalid, crAgentAnswer)
	c.answeredBy(ChannelCRInvalid, crAnswer, crServeTimeout)
	base := c.rejections("InvalidSpec")

	bad := tm.bindingSpec(ChannelCRInvalid, crAgentInvest)
	bad["chat"].(map[string]any)["triggers"] = []any{map[string]any{"type": "mention"}, map[string]any{"type": "mention"}}
	stored, err := tm.rewrite(tm.prefix, bad)
	if apierrors.IsInvalid(err) {
		t.Fatalf("%v: the API server must accept a duplicate trigger, which the schema cannot say; if it refuses now, this scenario needs another rule that only the broker has", err)
	} else if err != nil {
		t.Fatal(err)
	}
	if kept, _, _ := unstructured.NestedSlice(stored.Object, "spec", "chat", "triggers"); len(kept) != 2 {
		t.Fatalf("the stored Binding has %d triggers, want the 2 written: the API server pruned a field, so the update is not the one this scenario means", len(kept))
	}
	if stored.GetGeneration() != good.GetGeneration()+1 {
		t.Fatalf("generation %d after the update, want %d: the write did not change the spec", stored.GetGeneration(), good.GetGeneration()+1)
	}
	c.waitRejections("InvalidSpec", base+1)
	// The last good generation still serves: test-agent's text, never the new agent's.
	if got := c.answered(ChannelCRInvalid, time.Minute); !strings.Contains(got, crAnswer) || strings.Contains(got, crAnswerAlert) {
		t.Fatalf("the Binding answered %q while its generation %d was refused; want generation %d's %q", got, stored.GetGeneration(), good.GetGeneration(), crAnswer)
	}

	// What the Binding says of it: not accepted, at the generation that was refused, and the message names the generation that is served.
	err = Wait(c.ctx, statusWithin, "the status to say that the new generation is refused and the old one served", func(ctx context.Context) (bool, error) {
		obj, err := c.crs.Get(ctx, kube.BindingGVR, kube.TeamNamespace, tm.prefix)
		if err != nil {
			return false, err
		}
		acc, ok := condition(obj, "Accepted")
		if !ok || acc["status"] != "False" || acc["reason"] != "InvalidSpec" || acc["observedGeneration"] != stored.GetGeneration() {
			return false, nil
		}
		if msg := fmt.Sprint(acc["message"]); !strings.Contains(msg, fmt.Sprintf("serving generation %d", good.GetGeneration())) {
			return false, fmt.Errorf("Accepted says %q; want it to name the generation served, %d", msg, good.GetGeneration())
		}
		return true, nil
	})
	if err != nil {
		t.Error(err)
	}

	fixed := tm.mustRewrite(tm.prefix, tm.bindingSpec(ChannelCRInvalid, crAgentInvest))
	if fixed.GetGeneration() != stored.GetGeneration()+1 {
		t.Fatalf("generation %d after the repair, want %d", fixed.GetGeneration(), stored.GetGeneration()+1)
	}
	c.waitRejections("InvalidSpec", base)
	c.answeredBy(ChannelCRInvalid, crAnswerAlert, crServeTimeout)
}

// TestCR_TwoBindingsOnOneChannelLeaveTheOlderInPlace: the oldest Binding of a channel keeps it, and a younger one on the same channel is not served,
// whatever its name: the older here is named so that it sorts after the younger, so the age, and not the order of the names, decides. The younger is
// counted as rejected with ChannelConflict. When the older is deleted the younger takes the channel at the next reload, and the count goes back.
func TestCR_TwoBindingsOnOneChannelLeaveTheOlderInPlace(t *testing.T) {
	c := newCRScenario(t)
	tm := c.newTeam("conflict")
	tm.addBinding("z-first", ChannelCRConflict, crAgentAnswer)
	c.answeredBy(ChannelCRConflict, crAnswer, crServeTimeout)
	base := c.rejections("ChannelConflict")
	time.Sleep(3 * time.Second) // creationTimestamp has a resolution of a second: the younger must be younger by it

	tm.addBinding("a-second", ChannelCRConflict, crAgentInvest)
	c.waitRejections("ChannelConflict", base+1)
	if got := c.answered(ChannelCRConflict, time.Minute); !strings.Contains(got, crAnswer) || strings.Contains(got, crAnswerAlert) {
		t.Fatalf("the channel answered %q with a younger Binding on it; want the older's %q", got, crAnswer)
	}

	if err := c.crs.Delete(c.ctx, kube.BindingGVR, kube.TeamNamespace, "z-first"); err != nil {
		t.Fatal(err)
	}
	c.waitRejections("ChannelConflict", base)
	c.answeredBy(ChannelCRConflict, crAnswerAlert, crServeTimeout)
}
