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

// TestRunnerCRRoles pins what the custom resources scenarios may do. In Agent Kourier's namespace the runner only reads the three kinds: no Secret, and no
// write. In the team namespace it makes Bindings and AgentBackends, and Secrets, Roles and RoleBindings (the RBAC a team adds), reads no Secret (it has no
// get), and nothing else, in no other namespace.
func TestRunnerCRRoles(t *testing.T) {
	roles := runnerRolesByName(t)
	crs, ok := roles["agent-kourier/e2e-runner-crs"]
	if !ok {
		t.Fatal("the manifest has no Role e2e-runner-crs in agent-kourier")
	}
	if got := grants([]rbacv1.Role{crs}); len(got) != 6 || !got["bindings:get"] || !got["bindings:list"] || !got["agentbackends:get"] || !got["agentbackends:list"] || !got["chatconnections:get"] || !got["chatconnections:list"] {
		t.Errorf("Role e2e-runner-crs in agent-kourier grants %v, want get and list of the three kinds only", got)
	}
	team, ok := roles[TeamNamespace+"/e2e-runner-team"]
	if !ok {
		t.Fatalf("the manifest has no Role e2e-runner-team in %s", TeamNamespace)
	}
	got := grants([]rbacv1.Role{team})
	if got["secrets:get"] || got["secrets:update"] || got["secrets:patch"] {
		t.Errorf("Role e2e-runner-team grants %v: it must never read or change a Secret, only make, delete, list and watch", got)
	}
	for _, want := range []string{"bindings:create", "bindings:update", "bindings:delete", "agentbackends:create", "secrets:create", "secrets:delete", "secrets:list", "secrets:watch", "roles:create", "rolebindings:create", "roles:delete", "rolebindings:delete"} {
		if !got[want] {
			t.Errorf("Role e2e-runner-team lacks %s", want)
		}
	}
	// Four Roles in all: the two of the doubles and Agent Kourier, and these two. A Role in another namespace would let the runner write there.
	if len(roles) != 4 {
		t.Errorf("the manifest has %d Roles, want 4 (e2e, agent-kourier twice, %s)", len(roles), TeamNamespace)
	}
}
