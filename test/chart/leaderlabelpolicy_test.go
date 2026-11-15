package chart_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// admissionKubeconfigEnv names the kubeconfig of a throwaway cluster on this machine that TestLeaderLabelPolicyOnAnAPIServer installs
// the leader label's admission policy in. `make crd-corpus` (hack/kind/crd-corpus.sh) sets it to its kind cluster, which CI's crd-corpus
// jobs run on the pinned Kubernetes and on the chart's minimum. Unset, the test skips.
const admissionKubeconfigEnv = "CHART_ADMISSION_KUBECONFIG"

// policyTestLabel, with policyTestLabelValue, marks every object TestLeaderLabelPolicyOnAnAPIServer creates outside its namespace, and the
// namespace: a run cut short (an interrupt, or the -timeout panic, which skips t.Cleanup) leaves nothing that the next run's
// deleteLeftovers, or hack/kind/crd-corpus.sh's cleanup with CRD_CORPUS_KUBECONFIG, cannot find by it.
const policyTestLabel, policyTestLabelValue = "agentkourier.dev/test", "leader-label-policy"

// leaderLabelPolicy is the leader label's policy and its binding as the chart renders them for the release agent-kourier in namespace,
// with the chat listener on.
func leaderLabelPolicy(t *testing.T, objs []object, namespace string) (admissionregistrationv1.ValidatingAdmissionPolicy, admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	t.Helper()
	name := "agent-kourier-" + namespace + "-leader-label"
	var policy admissionregistrationv1.ValidatingAdmissionPolicy
	find(t, objs, "ValidatingAdmissionPolicy", name, &policy)
	var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	find(t, objs, "ValidatingAdmissionPolicyBinding", name, &binding)
	return policy, binding
}

// The Role's patch on pods reaches every pod of the namespace, so the chart renders, with it, an admission policy that holds the broker's
// ServiceAccount to the leader label of its own pod, bound to the release namespace and denying what it refuses. Without the Role there
// is no policy either.
func TestLeaderLabelPolicyRendersWithTheRole(t *testing.T) {
	t.Parallel()
	helmPath(t)
	objs := mustRender(t, "agent-kourier", "agent-kourier-system", chatOn()...)
	policy, binding := leaderLabelPolicy(t, objs, "agent-kourier-system")
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Errorf("failurePolicy %v; want Fail, so a policy that cannot be evaluated denies", policy.Spec.FailurePolicy)
	}
	rules := policy.Spec.MatchConstraints.ResourceRules
	if len(rules) != 1 || !slices.Equal(rules[0].Resources, []string{"pods"}) || !slices.Equal(rules[0].Operations, []admissionregistrationv1.OperationType{admissionregistrationv1.Update}) {
		t.Errorf("resource rules %+v; want UPDATE of pods, which a patch is", rules)
	}
	if len(policy.Spec.MatchConditions) != 1 || !strings.Contains(policy.Spec.MatchConditions[0].Expression, "'system:serviceaccount:agent-kourier-system:agent-kourier'") {
		t.Errorf("match conditions %+v; want the broker's ServiceAccount alone", policy.Spec.MatchConditions)
	}
	var ownPod bool
	for _, v := range policy.Spec.Validations {
		ownPod = ownPod || strings.Contains(v.Expression, "request.userInfo.extra['authentication.kubernetes.io/pod-name'] == [object.metadata.name]")
	}
	if !ownPod {
		t.Error("no validation holds the patch to the pod the token is bound to")
	}
	if binding.Spec.PolicyName != policy.Name || !slices.Equal(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
		t.Errorf("binding %+v; want the policy bound with Deny", binding.Spec)
	}
	if sel := binding.Spec.MatchResources; sel == nil || sel.NamespaceSelector == nil || sel.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "agent-kourier-system" {
		t.Errorf("binding's match %+v; want the release namespace alone", binding.Spec.MatchResources)
	}

	for _, args := range [][]string{chatOn("--set", "rbac.create=false"), {"--set", "image.repository=agent-kourier,config.source=files"}} {
		for _, o := range mustRender(t, "agent-kourier", "agent-kourier-system", args...) {
			if strings.HasPrefix(o.kind(), "ValidatingAdmissionPolicy") {
				t.Errorf("%v rendered %s %s; want the policy only with the Role", args, o.kind(), o.name())
			}
		}
	}
}

// adminClient is a client of the cluster in the kubeconfig admissionKubeconfigEnv names, and its config; it refuses a cluster that is not
// on this machine, since the test creates a namespace and a cluster-wide policy. Unset, the test skips.
func adminClient(t *testing.T) (*kubernetes.Clientset, *rest.Config) {
	t.Helper()
	path := os.Getenv(admissionKubeconfigEnv)
	if path == "" {
		t.Skipf("%s is not set: the test runs the leader label's admission policy on a real API server (a throwaway kind cluster)", admissionKubeconfigEnv)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("kubeconfig %s: %v", path, err)
	}
	u, err := url.Parse(cfg.Host)
	if err != nil {
		t.Fatalf("API server address %q: %v", cfg.Host, err)
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
		t.Fatalf("API server %s is not on this machine; the test installs a cluster-wide policy and runs only against a local cluster", cfg.Host)
	}
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, cfg
}

// The policy the chart renders, installed in a real API server with the Role, lets the broker take the leader label on and off its own
// pod, the one its token is bound to, and denies it the same patch of another pod, any other change of its own pod, and the patch with a
// token bound to no pod. That the kubelet's tokens carry the pod's name in the user's extra is what the first case relies on; run it on
// the chart's minimum Kubernetes.
func TestLeaderLabelPolicyOnAnAPIServer(t *testing.T) {
	admin, cfg := adminClient(t)
	helmPath(t)
	ctx := t.Context()
	ns := fmt.Sprintf("ak-admission-%d", time.Now().UnixNano()%1_000_000)
	objs := mustRender(t, "agent-kourier", ns, chatOn()...)
	policy, binding := leaderLabelPolicy(t, objs, ns)
	var sa corev1.ServiceAccount
	find(t, objs, "ServiceAccount", "agent-kourier", &sa)
	var role rbacv1.Role
	find(t, objs, "Role", "agent-kourier-leader-label", &role)
	var roleBinding rbacv1.RoleBinding
	find(t, objs, "RoleBinding", "agent-kourier-leader-label", &roleBinding)

	cleanup := context.WithoutCancel(ctx)
	must := func(_ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	deleteLeftovers(ctx, t, admin)
	marked := map[string]string{policyTestLabel: policyTestLabelValue}
	for _, meta := range []*metav1.ObjectMeta{&policy.ObjectMeta, &binding.ObjectMeta} {
		if meta.Labels == nil {
			meta.Labels = map[string]string{}
		}
		meta.Labels[policyTestLabel] = policyTestLabelValue
	}
	must(admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: marked}}, metav1.CreateOptions{}))
	t.Cleanup(func() { _ = admin.CoreV1().Namespaces().Delete(cleanup, ns, metav1.DeleteOptions{}) })
	must(admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, &policy, metav1.CreateOptions{}))
	t.Cleanup(func() {
		_ = admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(cleanup, policy.Name, metav1.DeleteOptions{})
	})
	must(admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, &binding, metav1.CreateOptions{}))
	t.Cleanup(func() {
		_ = admin.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(cleanup, binding.Name, metav1.DeleteOptions{})
	})
	must(admin.CoreV1().ServiceAccounts(ns).Create(ctx, &sa, metav1.CreateOptions{}))
	must(admin.RbacV1().Roles(ns).Create(ctx, &role, metav1.CreateOptions{}))
	must(admin.RbacV1().RoleBindings(ns).Create(ctx, &roleBinding, metav1.CreateOptions{}))
	newPod := func(name string) *corev1.Pod {
		t.Helper()
		p, err := admin.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/name": "agent-kourier"}},
			Spec:       corev1.PodSpec{ServiceAccountName: sa.Name, Containers: []corev1.Container{{Name: "c", Image: "registry.k8s.io/pause:3.10"}}},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	own, other := newPod("broker"), newPod("token-refresh")

	// A client that authenticates as the broker's ServiceAccount, with a token bound to the pod given, as the kubelet's is, or to none.
	as := func(pod *corev1.Pod) kubernetes.Interface {
		t.Helper()
		req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: new(int64(600))}}
		if pod != nil {
			req.Spec.BoundObjectRef = &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID}
		}
		tok, err := admin.CoreV1().ServiceAccounts(ns).CreateToken(ctx, sa.Name, req, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		c := rest.AnonymousClientConfig(cfg)
		c.BearerToken = tok.Status.Token
		client, err := kubernetes.NewForConfig(c)
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	broker, unbound := as(own), as(nil)
	patch := func(c kubernetes.Interface, pod string, pt types.PatchType, body string) error {
		_, err := c.CoreV1().Pods(ns).Patch(ctx, pod, pt, []byte(body), metav1.PatchOptions{FieldManager: "agent-kourier"})
		return err
	}
	const label, unlabel = `{"metadata":{"labels":{"agentkourier.dev/role":"leader"}}}`, `{"metadata":{"labels":{"agentkourier.dev/role":null}}}`
	denied := func(err error) bool {
		return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "ValidatingAdmissionPolicy")
	}

	// The API server loads a new policy and binding a moment after they are created: wait for the first denial.
	deadline := time.Now().Add(30 * time.Second)
	for err := patch(broker, other.Name, types.MergePatchType, label); !denied(err); err = patch(broker, other.Name, types.MergePatchType, label) {
		if time.Now().After(deadline) {
			t.Fatalf("the label on another pod = %v after 30s; want the policy to deny it", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if got, err := admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, policy.Name, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	} else if got.Status.TypeChecking != nil && len(got.Status.TypeChecking.ExpressionWarnings) > 0 {
		t.Errorf("the API server's type check of the policy warns: %+v", got.Status.TypeChecking.ExpressionWarnings)
	}

	for _, tc := range []struct {
		name   string
		client kubernetes.Interface
		pod    string
		pt     types.PatchType
		body   string
		allow  bool
	}{
		{"the label on its own pod", broker, own.Name, types.MergePatchType, label, true},
		{"the label off its own pod", broker, own.Name, types.MergePatchType, unlabel, true},
		{"the label on another pod", broker, other.Name, types.MergePatchType, label, false},
		{"the label off another pod", broker, other.Name, types.MergePatchType, unlabel, false},
		{"the label with a token bound to no pod", unbound, own.Name, types.MergePatchType, label, false},
		{"another value of the label", broker, own.Name, types.MergePatchType, `{"metadata":{"labels":{"agentkourier.dev/role":"standby"}}}`, false},
		{"another label of its own pod", broker, own.Name, types.MergePatchType, `{"metadata":{"labels":{"app.kubernetes.io/name":"other"}}}`, false},
		{"an annotation of its own pod", broker, own.Name, types.MergePatchType, `{"metadata":{"annotations":{"example.test/a":"b"}}}`, false},
		{"a finalizer of its own pod", broker, own.Name, types.MergePatchType, `{"metadata":{"finalizers":["example.test/hold"]}}`, false},
		{"the image of its own pod", broker, own.Name, types.StrategicMergePatchType, `{"spec":{"containers":[{"name":"c","image":"registry.k8s.io/pause:3.9"}]}}`, false},
		{"the image of another pod", broker, other.Name, types.StrategicMergePatchType, `{"spec":{"containers":[{"name":"c","image":"registry.k8s.io/pause:3.9"}]}}`, false},
		{"the label on another pod, by someone else", admin, other.Name, types.MergePatchType, label, true},
	} {
		err := patch(tc.client, tc.pod, tc.pt, tc.body)
		t.Logf("%s: %v", tc.name, err)
		switch {
		case tc.allow && err != nil:
			t.Errorf("%s = %v; want it allowed", tc.name, err)
		case !tc.allow && !denied(err):
			t.Errorf("%s = %v; want the policy to deny it", tc.name, err)
		}
	}
}

// deleteLeftovers deletes the bindings, policies and namespaces that carry policyTestLabel: what an earlier run that was cut short left
// behind. Each run's names are its own, so this is housekeeping, not a precondition.
func deleteLeftovers(ctx context.Context, t *testing.T, admin kubernetes.Interface) {
	t.Helper()
	sel := metav1.ListOptions{LabelSelector: policyTestLabel + "=" + policyTestLabelValue}
	gone := func(err error) {
		t.Helper()
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("delete a leftover of an earlier run: %v", err)
		}
	}
	ar := admin.AdmissionregistrationV1()
	bindings, err := ar.ValidatingAdmissionPolicyBindings().List(ctx, sel)
	gone(err)
	for _, b := range bindings.Items {
		gone(ar.ValidatingAdmissionPolicyBindings().Delete(ctx, b.Name, metav1.DeleteOptions{}))
	}
	policies, err := ar.ValidatingAdmissionPolicies().List(ctx, sel)
	gone(err)
	for _, p := range policies.Items {
		gone(ar.ValidatingAdmissionPolicies().Delete(ctx, p.Name, metav1.DeleteOptions{}))
	}
	namespaces, err := admin.CoreV1().Namespaces().List(ctx, sel)
	gone(err)
	for _, n := range namespaces.Items {
		gone(admin.CoreV1().Namespaces().Delete(ctx, n.Name, metav1.DeleteOptions{}))
	}
}
