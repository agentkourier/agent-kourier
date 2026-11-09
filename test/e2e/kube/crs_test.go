package kube

import (
	"errors"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k2ktesting "k8s.io/client-go/testing"
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
	return newCRs(testDynamic(objs...), fake.NewSimpleClientset())
}

func testDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	kinds := map[schema.GroupVersionResource]string{BindingGVR: "BindingList", AgentBackendGVR: "AgentBackendList", ChatConnectionGVR: "ChatConnectionList"}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objs...)
}

// TestCRsReplaceReadsAgainOnAConflict: the status writer updates a Binding too, so an update can find the object changed under it. Replace goes on with
// what is there then, and gives up with the Conflict only when it never stops.
func TestCRsReplaceReadsAgainOnAConflict(t *testing.T) {
	dyn := testDynamic()
	c := newCRs(dyn, fake.NewSimpleClientset())
	ctx := t.Context()
	if _, err := c.Create(ctx, BindingGVR, testBinding(TeamNamespace, "x")); err != nil {
		t.Fatal(err)
	}
	conflicts := 1
	dyn.PrependReactor("update", "bindings", func(k2ktesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(BindingGVR.GroupResource(), "x", errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	if _, err := c.Replace(ctx, BindingGVR, testBinding(TeamNamespace, "x")); err != nil {
		t.Errorf("Replace after one conflict = %v, want it to succeed", err)
	}
	conflicts = 1000
	if _, err := c.Replace(ctx, BindingGVR, testBinding(TeamNamespace, "x")); !apierrors.IsConflict(errors.Unwrap(err)) && !apierrors.IsConflict(err) {
		t.Errorf("Replace under endless conflicts = %v, want the Conflict", err)
	}
}

// TestCRsPurgeDeletesWhatIsThere: every object of the resource in the team namespace goes, whoever made it, and none in another.
func TestCRsPurgeDeletesWhatIsThere(t *testing.T) {
	c := testCRs(testBinding(TeamNamespace, "a"), testBinding(TeamNamespace, "b"), testBinding("agent-kourier", "keep"))
	ctx := t.Context()
	if err := c.Purge(ctx, BindingGVR); err != nil {
		t.Fatal(err)
	}
	left, err := c.dyn.Resource(BindingGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil || len(left.Items) != 1 || left.Items[0].GetName() != "keep" {
		t.Errorf("left after Purge = %v, %v; want only agent-kourier/keep", left.Items, err)
	}
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

// TestCRsInWritesInTheNamespaceItChose: the scenarios of several Slack apps play a team in the namespaces named in In, and in no other: a client made for
// one refuses a write in the next, and In refuses a namespace the runner has no Role in. Its Secrets and grants are that namespace's too.
func TestCRsInWritesInTheNamespaceItChose(t *testing.T) {
	c := testCRs()
	ctx := t.Context()
	for _, ns := range []string{TeamNamespace, MultiTeamNamespace, ScratchNamespace, ScratchNamespace2} {
		in, err := c.In(ns)
		if err != nil {
			t.Fatalf("In(%s) = %v, want a client", ns, err)
		}
		if in.Namespace() != ns {
			t.Errorf("In(%s).Namespace() = %s", ns, in.Namespace())
		}
		if _, err := in.Create(ctx, ChatConnectionGVR, testBinding(ns, "x")); err != nil {
			t.Errorf("Create in %s through In(%s) = %v, want it made", ns, ns, err)
		}
		other := TeamNamespace
		if ns == TeamNamespace {
			other = ScratchNamespace
		}
		if _, err := in.Create(ctx, BindingGVR, testBinding(other, "x")); !errors.Is(err, ErrWriteRefused) {
			t.Errorf("Create in %s through In(%s) = %v, want ErrWriteRefused", other, ns, err)
		}
		if err := in.Delete(ctx, ChatConnectionGVR, other, "x"); !errors.Is(err, ErrWriteRefused) {
			t.Errorf("Delete in %s through In(%s) = %v, want ErrWriteRefused", other, ns, err)
		}
		if err := in.Delete(ctx, ChatConnectionGVR, ns, "x"); err != nil {
			t.Errorf("Delete in %s through In(%s) = %v, want it gone", ns, ns, err)
		}
	}
	for _, ns := range []string{"agent-kourier", "kube-system", "", "team-d"} {
		if _, err := c.In(ns); !errors.Is(err, ErrNoSuchTeamNamespace) {
			t.Errorf("In(%q) = %v, want ErrNoSuchTeamNamespace", ns, err)
		}
	}
	if c.Namespace() != TeamNamespace {
		t.Errorf("a client made for no namespace writes in %s, want %s", c.Namespace(), TeamNamespace)
	}

	in, err := c.In(ScratchNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.CreateSecret(ctx, "mc-slack", map[string]string{"botToken": "xoxb-fake"}); err != nil {
		t.Fatal(err)
	}
	if err := in.GrantSecret(ctx, "mc-slack"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.core.RbacV1().RoleBindings(ScratchNamespace).Get(ctx, roleName("mc-slack"), metav1.GetOptions{}); err != nil {
		t.Errorf("the grant is not in %s: %v", ScratchNamespace, err)
	}
	if _, err := in.core.RbacV1().Roles(TeamNamespace).Get(ctx, roleName("mc-slack"), metav1.GetOptions{}); err == nil {
		t.Errorf("the grant was made in %s, not in the namespace the client was made for", TeamNamespace)
	}
	if err := in.RemoveSecret(ctx, "mc-slack"); err != nil {
		t.Errorf("RemoveSecret = %v", err)
	}
}

// TestSetSecretDataMergesTheKeysGiven: a team that fixes a token changes the one key and leaves the others, and the Secret must be there.
func TestSetSecretDataMergesTheKeysGiven(t *testing.T) {
	c, err := testCRs().In(MultiTeamNamespace)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := c.SetSecretData(ctx, "slack-team-a", map[string]string{"botToken": "xoxb-fake-new"}); err == nil {
		t.Fatal("SetSecretData of a Secret that is not there succeeded; want the not-found error")
	}
	if err := c.CreateSecret(ctx, "slack-team-a", map[string]string{"botToken": "xoxb-fake-old", "appToken": "xapp-fake-old"}); err != nil {
		t.Fatal(err)
	}
	// The fake clientset does not turn stringData into data, as the API server does: it keeps what the patch said, which is what is read back.
	if err := c.SetSecretData(ctx, "slack-team-a", map[string]string{"botToken": "xoxb-fake-new"}); err != nil {
		t.Fatalf("SetSecretData = %v", err)
	}
	got, err := c.core.CoreV1().Secrets(MultiTeamNamespace).Get(ctx, "slack-team-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.StringData["botToken"] != "xoxb-fake-new" || got.StringData["appToken"] != "xapp-fake-old" {
		t.Errorf("the Secret holds %v after the patch; want the new botToken and the old appToken", got.StringData)
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
