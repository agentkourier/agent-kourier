package kube

import (
	"errors"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func testBinding(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentkourier.dev/v1alpha1",
		"kind":       "Binding",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{"chat": map[string]any{"channel": "C1"}},
	}}
}

func testCRs(objs ...runtime.Object) *CRs {
	scheme := runtime.NewScheme()
	return newCRs(dynamicfake.NewSimpleDynamicClient(scheme, objs...), fake.NewSimpleClientset())
}

// TestCRsWriteOnlyInTheTeamNamespace: the runner's Role allows writes in one namespace, and the helper refuses the others before it asks, so that a
// scenario that names another fails with the reason and not with a Forbidden.
func TestCRsWriteOnlyInTheTeamNamespace(t *testing.T) {
	c := testCRs()
	ctx := t.Context()
	if _, err := c.Create(ctx, BindingGVR, testBinding("agent-kourier", "x")); !errors.Is(err, ErrWriteRefused) {
		t.Errorf("Create in agent-kourier = %v, want ErrWriteRefused", err)
	}
	if _, err := c.Replace(ctx, BindingGVR, testBinding("agent-kourier", "x")); !errors.Is(err, ErrWriteRefused) {
		t.Errorf("Replace in agent-kourier = %v, want ErrWriteRefused", err)
	}
	if err := c.Delete(ctx, BindingGVR, "agent-kourier", "x"); !errors.Is(err, ErrWriteRefused) {
		t.Errorf("Delete in agent-kourier = %v, want ErrWriteRefused", err)
	}
	if _, err := c.Create(ctx, BindingGVR, testBinding(TeamNamespace, "x")); err != nil {
		t.Errorf("Create in %s = %v, want it made", TeamNamespace, err)
	}
}

// TestCRsReplacePutsTheObjectInPlaceOfTheLiveOne: Replace works against whatever is there, as a team's apply of an edited file does, so a caller need not
// carry the resourceVersion of what it read; one that is not there is an error, and Delete of one that is not there is not.
func TestCRsReplacePutsTheObjectInPlaceOfTheLiveOne(t *testing.T) {
	c := testCRs()
	ctx := t.Context()
	if _, err := c.Replace(ctx, BindingGVR, testBinding(TeamNamespace, "x")); err == nil {
		t.Error("Replace of an object that is not there succeeded")
	}
	if _, err := c.Create(ctx, BindingGVR, testBinding(TeamNamespace, "x")); err != nil {
		t.Fatal(err)
	}
	next := testBinding(TeamNamespace, "x")
	next.Object["spec"] = map[string]any{"chat": map[string]any{"channel": "C2"}}
	if _, err := c.Replace(ctx, BindingGVR, next); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, BindingGVR, TeamNamespace, "x")
	if err != nil {
		t.Fatal(err)
	}
	if ch, _, _ := unstructured.NestedString(got.Object, "spec", "chat", "channel"); ch != "C2" {
		t.Errorf("channel = %q after Replace, want C2", ch)
	}
	if err := c.Delete(ctx, BindingGVR, TeamNamespace, "x"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, BindingGVR, TeamNamespace, "x"); err != nil {
		t.Errorf("Delete of an object that is gone = %v, want nil", err)
	}
}

// TestParseTable: the API server's table is read into rows by column name, a cell that is null is empty, and an answer that is not a Table is an error.
func TestParseTable(t *testing.T) {
	const body = `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[{"name":"Name"},{"name":"Ready"},{"name":"Reason"},{"name":"Channel"}],
"rows":[{"cells":["a","True","OK","C1"]},{"cells":["b",null,null,"C2"]}]}`
	tab, err := parseTable([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tab.Columns, []string{"Name", "Ready", "Reason", "Channel"}) {
		t.Errorf("columns = %v", tab.Columns)
	}
	a, ok := tab.Row("a")
	if !ok || a["Ready"] != "True" || a["Reason"] != "OK" || a["Channel"] != "C1" {
		t.Errorf("row a = %v, %v", a, ok)
	}
	if b, _ := tab.Row("b"); b["Ready"] != "" || b["Channel"] != "C2" {
		t.Errorf("row b = %v, want an empty Ready and its channel", b)
	}
	if _, ok := tab.Row("c"); ok {
		t.Error("Row found a row that is not there")
	}
	if _, err := parseTable([]byte(`{"kind":"Status","status":"Failure"}`)); err == nil {
		t.Error("an answer that is not a Table was read as one")
	}
}

// TestGrantSecretIsARoleByNameAndABindingToAgentKourier: what a team must add: list and watch of the one Secret by name, bound to Agent Kourier's
// ServiceAccount, and removed with it.
func TestGrantSecretIsARoleByNameAndABindingToAgentKourier(t *testing.T) {
	c := testCRs()
	ctx := t.Context()
	if err := c.CreateSecret(ctx, "tok", map[string]string{"token": "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := c.GrantSecret(ctx, "tok"); err != nil {
		t.Fatal(err)
	}
	role, err := c.core.RbacV1().Roles(TeamNamespace).Get(ctx, roleName("tok"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 1 || !slices.Equal(role.Rules[0].ResourceNames, []string{"tok"}) || !slices.Equal(role.Rules[0].Verbs, []string{"list", "watch"}) {
		t.Errorf("role rules = %+v, want list and watch of the Secret tok by name", role.Rules)
	}
	rb, err := c.core.RbacV1().RoleBindings(TeamNamespace).Get(ctx, roleName("tok"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != "agent-kourier" || rb.Subjects[0].Namespace != "agent-kourier" || rb.RoleRef.Name != role.Name {
		t.Errorf("rolebinding = %+v, want the Role bound to agent-kourier/agent-kourier", rb)
	}
	if err := c.RemoveSecret(ctx, "tok"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveSecret(ctx, "tok"); err != nil {
		t.Errorf("RemoveSecret of what is gone = %v, want nil", err)
	}
	if _, err := c.core.RbacV1().Roles(TeamNamespace).Get(ctx, roleName("tok"), metav1.GetOptions{}); err == nil {
		t.Error("the Role is still there after RemoveSecret")
	}
}

func TestNewCRsInCluster_Guard(t *testing.T) {
	inCluster := func() (*rest.Config, error) { return &rest.Config{Host: "https://127.0.0.1:1"}, nil }
	if _, err := newCRsInCluster(inCluster, nsFile(t, "agent-kourier\n")); !errors.Is(err, ErrWrongNamespace) {
		t.Errorf("wrong namespace = %v, want ErrWrongNamespace", err)
	}
	if _, err := newCRsInCluster(inCluster, nsFile(t, "e2e\n")); err != nil {
		t.Errorf("e2e namespace = %v, want it built", err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := NewCRsInCluster(); !errors.Is(err, ErrNotInCluster) {
		t.Errorf("outside a cluster = %v, want ErrNotInCluster", err)
	}
}
