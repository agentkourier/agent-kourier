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

// runnerRoles reads the Roles in the runner's manifest, by namespace.
func runnerRoles(t *testing.T) map[string][]rbacv1.Role {
	t.Helper()
	raw, err := os.ReadFile("../../../hack/kind/e2e/runner.yaml")
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string][]rbacv1.Role{}
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
		roles[r.Namespace] = append(roles[r.Namespace], r)
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
// courier only read pods and patch the Deployment. A pod delete there would bypass strategy:
// Recreate, so the API must refuse it as well as Kill and Restart.
func TestRunnerRoles(t *testing.T) {
	roles := runnerRoles(t)
	if got, want := grants(roles["courier"]), map[string]bool{"pods:get": true, "pods:list": true, "deployments:get": true, "deployments:patch": true}; len(got) != len(want) {
		t.Errorf("Role in courier grants %v, want exactly %v", got, want)
	} else {
		for g := range want {
			if !got[g] {
				t.Errorf("Role in courier lacks %s", g)
			}
		}
	}
	// Patching a Deployment is limited to courier's own: a runner that could patch any would be able to
	// change the test doubles' images or arguments.
	for _, r := range roles["courier"] {
		for _, rule := range r.Rules {
			named := fmt.Sprint(rule.ResourceNames)
			if isDeployments := slices.Contains(rule.Resources, "deployments"); isDeployments && named != "[courier]" || !isDeployments && named != "[]" {
				t.Errorf("rule %v in courier has resourceNames %s; want deployments limited to [courier], and pods unlimited", rule.Resources, named)
			}
		}
	}
	if got := grants(roles[Namespace]); !got["pods:delete"] || !got["pods:list"] || len(got) != 2 {
		t.Errorf("Role in %s grants %v, want pods list and delete only", Namespace, got)
	}
	if len(roles) != 2 {
		t.Errorf("the manifest has Roles in %d namespaces, want e2e and courier only", len(roles))
	}
}
