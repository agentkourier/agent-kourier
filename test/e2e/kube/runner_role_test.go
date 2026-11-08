package kube

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// runnerRoles reads the Roles in the runner's manifest that are named e2e-runner, by namespace: the ones that restart the doubles and Agent Kourier. The
// Roles of the custom resources scenarios are runnerRolesByName's.
func runnerRoles(t *testing.T) map[string][]rbacv1.Role {
	t.Helper()
	roles := map[string][]rbacv1.Role{}
	for _, r := range runnerRolesByName(t) {
		if r.Name == "e2e-runner" {
			roles[r.Namespace] = append(roles[r.Namespace], r)
		}
	}
	return roles
}

// runnerRolesByName reads every Role in the runner's manifest, by "namespace/name".
func runnerRolesByName(t *testing.T) map[string]rbacv1.Role {
	t.Helper()
	raw, err := os.ReadFile("../../../hack/kind/e2e/runner.yaml")
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]rbacv1.Role{}
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			return roles
		} else if err != nil {
			t.Fatal(err)
		}
		if doc["kind"] != "Role" {
			continue
		}
		b, _ := json.Marshal(doc)
		var r rbacv1.Role
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		roles[r.Namespace+"/"+r.Name] = r
	}
}

// grants lists what the Roles allow as "resource:verb" for the group's resources.
func grants(roles []rbacv1.Role) map[string]bool {
	out := map[string]bool{}
	for _, r := range roles {
		for _, rule := range r.Rules {
			for _, res := range rule.Resources {
				for _, v := range rule.Verbs {
					out[res+":"+v] = true
				}
			}
		}
	}
	return out
}

// TestRunnerRoles pins what the runner's service account may do: restart the doubles in e2e, and in
// Agent Kourier only read pods and their logs and patch the Deployment. A pod delete there would bypass strategy:
// Recreate, so the API must refuse it as well as Kill and Restart.
func TestRunnerRoles(t *testing.T) {
	roles := runnerRoles(t)
	if got, want := grants(roles["agent-kourier"]), map[string]bool{"pods:get": true, "pods:list": true, "pods/log:get": true, "deployments:get": true, "deployments:patch": true}; len(got) != len(want) {
		t.Errorf("Role in agent-kourier grants %v, want exactly %v", got, want)
	} else {
		for g := range want {
			if !got[g] {
				t.Errorf("Role in agent-kourier lacks %s", g)
			}
		}
	}
	// Patching a Deployment is limited to Agent Kourier's own: a runner that could patch any would be able to
	// change the test doubles' images or arguments.
	for _, r := range roles["agent-kourier"] {
		for _, rule := range r.Rules {
			named := fmt.Sprint(rule.ResourceNames)
			if isDeployments := slices.Contains(rule.Resources, "deployments"); isDeployments && named != "[agent-kourier]" || !isDeployments && named != "[]" {
				t.Errorf("rule %v in agent-kourier has resourceNames %s; want deployments limited to [agent-kourier], and pods and their logs unlimited", rule.Resources, named)
			}
		}
	}
	if got := grants(roles[Namespace]); !got["pods:delete"] || !got["pods:list"] || len(got) != 2 {
		t.Errorf("Role in %s grants %v, want pods list and delete only", Namespace, got)
	}
	if len(roles) != 2 {
		t.Errorf("the manifest has e2e-runner Roles in %d namespaces, want e2e and agent-kourier only", len(roles))
	}
}

// TestRunnerCRRoles pins what the custom resources scenarios may do, which is all in the team namespace. It makes Bindings and AgentBackends, Roles and
// RoleBindings (the RBAC a team adds) and Secrets. A Secret can be created without a name, and then only the named ones of the scenarios can be listed,
// watched or deleted: it has no get, and a list or watch of the namespace's Secrets is forbidden, so it cannot read the others. Nothing is granted in
// another namespace.
func TestRunnerCRRoles(t *testing.T) {
	roles := runnerRolesByName(t)
	// No rule of any Role is a wildcard: a "*" in resources, verbs or API groups would grant more than the lists the tests above and below pin.
	for name, r := range roles {
		for _, rule := range r.Rules {
			for _, list := range [][]string{rule.Resources, rule.Verbs, rule.APIGroups, rule.ResourceNames} {
				if slices.Contains(list, "*") {
					t.Errorf("Role %s has a rule with a wildcard: %v", name, rule)
				}
			}
		}
	}
	team, ok := roles[TeamNamespace+"/e2e-runner-team"]
	if !ok {
		t.Fatalf("the manifest has no Role e2e-runner-team in %s", TeamNamespace)
	}
	got := grants([]rbacv1.Role{team})
	for _, want := range []string{"bindings:get", "bindings:list", "bindings:create", "bindings:update", "bindings:delete", "agentbackends:list", "agentbackends:create", "agentbackends:delete", "secrets:create", "secrets:delete", "secrets:list", "secrets:watch", "roles:create", "rolebindings:create", "roles:delete", "rolebindings:delete"} {
		if !got[want] {
			t.Errorf("Role e2e-runner-team lacks %s", want)
		}
	}
	for _, never := range []string{"secrets:get", "secrets:update", "secrets:patch", "bindings:patch", "bindings:watch", "roles:get"} {
		if got[never] {
			t.Errorf("Role e2e-runner-team grants %s, which no scenario uses (and a Secret must never be read or changed)", never)
		}
	}
	// The Secrets the scenarios make are the only ones that it may list, watch or delete: every rule on secrets other than create names them.
	names := map[string]bool{}
	for _, rule := range team.Rules {
		if !slices.Contains(rule.Resources, "secrets") {
			continue
		}
		if slices.Equal(rule.Verbs, []string{"create"}) && len(rule.ResourceNames) == 0 {
			continue
		}
		if len(rule.ResourceNames) == 0 {
			t.Errorf("rule %v on secrets names none: it would let the runner list or delete every Secret of the namespace", rule.Verbs)
		}
		for _, n := range rule.ResourceNames {
			names[n] = true
		}
	}
	for _, want := range []string{"newns-token", "invalid-token", "conflict-token", "status-token", "failover-token"} {
		if !names[want] {
			t.Errorf("the Secret %s of a scenario is not named in Role e2e-runner-team", want)
		}
	}
	// Six Roles in all: the two of the doubles and Agent Kourier, this one, and the three of the Slack apps' namespaces (TestRunnerConnectionRoles). A Role in
	// another namespace would let the runner write there.
	if len(roles) != 6 {
		t.Errorf("the manifest has %d Roles, want 6 (e2e, agent-kourier, %s, %s, %s, %s)", len(roles), TeamNamespace, MultiTeamNamespace, ScratchNamespace, ScratchNamespace2)
	}
}

// TestRunnerConnectionRoles pins what the scenarios of several Slack apps may do. In team-a, which the tier fills, the runner reads Bindings and
// ChatConnections and patches the one Secret of the Slack tokens, by name: it can never read a Secret, there or anywhere. In team-b and team-c it plays a
// team as it does in e2e-team, and may also make and delete ChatConnections, with the Secrets of a case (mc-slack and mc-token) the only ones it may list,
// watch or delete.
func TestRunnerConnectionRoles(t *testing.T) {
	roles := runnerRolesByName(t)
	a, ok := roles[MultiTeamNamespace+"/e2e-runner-team-a"]
	if !ok {
		t.Fatalf("the manifest has no Role e2e-runner-team-a in %s", MultiTeamNamespace)
	}
	if got, want := grants([]rbacv1.Role{a}), map[string]bool{"bindings:get": true, "bindings:list": true, "chatconnections:get": true, "chatconnections:list": true, "secrets:patch": true}; len(got) != len(want) {
		t.Errorf("Role e2e-runner-team-a grants %v, want exactly %v", got, want)
	} else {
		for g := range want {
			if !got[g] {
				t.Errorf("Role e2e-runner-team-a lacks %s", g)
			}
		}
	}
	for _, rule := range a.Rules {
		if slices.Contains(rule.Resources, "secrets") && fmt.Sprint(rule.ResourceNames) != "[slack-team-a]" {
			t.Errorf("rule %v on secrets in %s names %v; want the Slack tokens, slack-team-a, and nothing else", rule.Verbs, MultiTeamNamespace, rule.ResourceNames)
		}
	}
	for _, ns := range []string{ScratchNamespace, ScratchNamespace2} {
		r, ok := roles[ns+"/e2e-runner-"+ns]
		if !ok {
			t.Fatalf("the manifest has no Role e2e-runner-%s in %s", ns, ns)
		}
		got := grants([]rbacv1.Role{r})
		for _, want := range []string{"chatconnections:create", "chatconnections:delete", "chatconnections:get", "bindings:create", "bindings:delete", "bindings:get", "agentbackends:create", "agentbackends:delete", "secrets:create", "secrets:delete", "secrets:list", "secrets:watch", "roles:create", "rolebindings:create", "roles:delete", "rolebindings:delete"} {
			if !got[want] {
				t.Errorf("Role %s in %s lacks %s", r.Name, ns, want)
			}
		}
		for _, never := range []string{"secrets:get", "secrets:update", "secrets:patch", "roles:get", "bindings:patch"} {
			if got[never] {
				t.Errorf("Role %s in %s grants %s, which no scenario uses", r.Name, ns, never)
			}
		}
		for _, rule := range r.Rules {
			if !slices.Contains(rule.Resources, "secrets") || slices.Equal(rule.Verbs, []string{"create"}) && len(rule.ResourceNames) == 0 {
				continue
			}
			if fmt.Sprint(rule.ResourceNames) != "[mc-slack mc-token]" {
				t.Errorf("rule %v on secrets in %s names %v; want mc-slack and mc-token, the Secrets of a case", rule.Verbs, ns, rule.ResourceNames)
			}
		}
	}
}
