package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
)

// TeamNamespace is the namespace where the scenarios of the custom resources route play a team (hack/kind/e2e/runner.yaml gives the runner a Role
// there and nowhere else to write). [CRs] refuses a write anywhere but here, and reads the three kinds in any namespace the Role allows.
const TeamNamespace = "e2e-team"

// AgentKourierServiceAccount is the ServiceAccount Agent Kourier runs as, which a team's Role must be bound to: the chart's, in the release
// "agent-kourier" of the tiers, in Agent Kourier's namespace.
const AgentKourierServiceAccount = "agent-kourier"

// The namespaces of the scenarios of a broker that serves several Slack apps (hack/kind/fast/two-apps, hack/kind/e2e/runner.yaml): MultiTeamNamespace holds
// a ChatConnection, a Binding and the Slack tokens that the tier makes and the runner only reads and fixes; ScratchNamespace and ScratchNamespace2 are empty,
// where a case adds a ChatConnection while the broker runs. All three are on the platform's admission list.
const (
	MultiTeamNamespace = "team-a"
	ScratchNamespace   = "team-b"
	ScratchNamespace2  = "team-c"
)

// ErrWriteRefused is returned, wrapped, for a write outside the namespace a [CRs] was made for: [TeamNamespace], or the one [CRs.In] chose.
var ErrWriteRefused = errors.New("kube: refusing to write outside the scenario's team namespace")

// The kinds of group agentkourier.dev the scenarios make and read.
var (
	// ChatConnectionGVR is the ChatConnection resource.
	ChatConnectionGVR = schema.GroupVersionResource{Group: "agentkourier.dev", Version: "v1alpha1", Resource: "chatconnections"}
	// BindingGVR is the Binding resource: `kubectl get akbinding`, never `bindings`, which is the core resource of that name.
	BindingGVR = schema.GroupVersionResource{Group: "agentkourier.dev", Version: "v1alpha1", Resource: "bindings"}
	// AgentBackendGVR is the AgentBackend resource.
	AgentBackendGVR = schema.GroupVersionResource{Group: "agentkourier.dev", Version: "v1alpha1", Resource: "agentbackends"}
)

// CRs plays a team for the scenarios of the custom resources route: it makes and changes Bindings and AgentBackends, a token Secret, and the Role and
// RoleBinding that let Agent Kourier read that Secret, all in [TeamNamespace] (or the namespace [CRs.In] chose); and it reads the three kinds anywhere the
// runner's Roles allow, as the table `kubectl get` prints. [NewCRsInCluster] is the only way to build one outside this package's tests, so the in-cluster
// guard cannot be skipped.
type CRs struct {
	ns   string // the one namespace it writes in
	dyn  dynamic.Interface
	core kubernetes.Interface
	raw  func(ctx context.Context, path, accept string) ([]byte, error) // a GET of an API path with an Accept header, which the typed clients cannot send
}

// NewCRsInCluster builds a [CRs] from rest.InClusterConfig, after the same check as [NewInCluster]: the service account's namespace is [Namespace].
func NewCRsInCluster() (*CRs, error) {
	return newCRsInCluster(rest.InClusterConfig, DefaultNamespaceFile)
}

func newCRsInCluster(inClusterConfig func() (*rest.Config, error), namespaceFile string) (*CRs, error) {
	cfg, err := guardedConfig(inClusterConfig, namespaceFile)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: build dynamic client: %w", err)
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: build clientset: %w", err)
	}
	c := newCRs(dyn, core)
	c.raw = func(ctx context.Context, path, accept string) ([]byte, error) {
		return core.Discovery().RESTClient().Get().AbsPath(path).SetHeader("Accept", accept).DoRaw(ctx)
	}
	return c, nil
}

func newCRs(dyn dynamic.Interface, core kubernetes.Interface) *CRs {
	return &CRs{ns: TeamNamespace, dyn: dyn, core: core}
}

// ErrNoSuchTeamNamespace is returned, wrapped, by [CRs.In] for a namespace the runner has no Role to play a team in.
var ErrNoSuchTeamNamespace = errors.New("kube: not a namespace the scenarios play a team in")

// In returns the same client for writes in another namespace of the scenarios: [TeamNamespace], [MultiTeamNamespace], [ScratchNamespace] or
// [ScratchNamespace2]. What the runner may do there is the Role of that namespace (hack/kind/e2e/runner.yaml); this only keeps a case from asking
// for another namespace by mistake.
func (c *CRs) In(namespace string) (*CRs, error) {
	switch namespace {
	case TeamNamespace, MultiTeamNamespace, ScratchNamespace, ScratchNamespace2:
		in := *c
		in.ns = namespace
		return &in, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrNoSuchTeamNamespace, namespace)
}

// Namespace is the namespace the client writes in.
func (c *CRs) Namespace() string { return c.ns }

// Get returns the object, wherever the runner may read it.
func (c *CRs) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	obj, err := c.dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", gvr.Resource, namespace, name, err)
	}
	return obj, nil
}

// Create makes the object, which must be in the client's namespace, and returns it as the API server stored it.
func (c *CRs) Create(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if obj.GetNamespace() != c.ns {
		return nil, fmt.Errorf("%w: %s", ErrWriteRefused, obj.GetNamespace())
	}
	got, err := c.dyn.Resource(gvr).Namespace(c.ns).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create %s %s/%s: %w", gvr.Resource, obj.GetNamespace(), obj.GetName(), err)
	}
	return got, nil
}

// Replace puts obj in place of the object of its name, which must be in the client's namespace: what a team's `kubectl apply` of an edited file does, and
// like it, against whatever is there now. The status writer updates the same object, so a write that finds it changed under it (a Conflict) reads it
// again and goes on. It returns the object as stored, so its generation says whether the change counted as one.
func (c *CRs) Replace(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if obj.GetNamespace() != c.ns {
		return nil, fmt.Errorf("%w: %s", ErrWriteRefused, obj.GetNamespace())
	}
	var got *unstructured.Unstructured
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := c.dyn.Resource(gvr).Namespace(c.ns).Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		next := obj.DeepCopy()
		next.SetResourceVersion(live.GetResourceVersion())
		got, err = c.dyn.Resource(gvr).Namespace(c.ns).Update(ctx, next, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("replace %s %s/%s: %w", gvr.Resource, obj.GetNamespace(), obj.GetName(), err)
	}
	return got, nil
}

// Purge deletes every object of the resource in the client's namespace, whoever made it: the start of a case that finds what a run killed by its timeout left.
func (c *CRs) Purge(ctx context.Context, gvr schema.GroupVersionResource) error {
	list, err := c.dyn.Resource(gvr).Namespace(c.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list %s in %s: %w", gvr.Resource, c.ns, err)
	}
	for i := range list.Items {
		if err := c.Delete(ctx, gvr, c.ns, list.Items[i].GetName()); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes the object of that name in the client's namespace; one that is not there is not an error, so that a scenario cleans up whatever it got to.
func (c *CRs) Delete(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error {
	if namespace != c.ns {
		return fmt.Errorf("%w: %s", ErrWriteRefused, namespace)
	}
	err := c.dyn.Resource(gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s/%s: %w", gvr.Resource, namespace, name, err)
	}
	return nil
}

// Table is what `kubectl get` prints for a resource: the API server's own table, with the columns the CRD's printer columns define.
type Table struct {
	// Columns are the column names, in order: Name first, then the CRD's.
	Columns []string
	// Rows are the objects, each as its cells by column name.
	Rows []map[string]string
}

// Row returns the cells of the row whose Name is name.
func (t Table) Row(name string) (map[string]string, bool) {
	for _, r := range t.Rows {
		if r["Name"] == name {
			return r, true
		}
	}
	return nil, false
}

// Table asks the API server for the table of the resource in a namespace, as kubectl does (an Accept header of as=Table), and so reads what
// `kubectl get` shows without kubectl: the columns READY and REASON of a Binding are the printer columns of its CRD.
func (c *CRs) Table(ctx context.Context, gvr schema.GroupVersionResource, namespace string) (Table, error) {
	path := fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s", gvr.Group, gvr.Version, namespace, gvr.Resource)
	body, err := c.raw(ctx, path, "application/json;as=Table;g=meta.k8s.io;v=v1")
	if err != nil {
		return Table{}, fmt.Errorf("get the table of %s in %s: %w", gvr.Resource, namespace, err)
	}
	return parseTable(body)
}

func parseTable(body []byte) (Table, error) {
	var in struct {
		Kind    string `json:"kind"`
		Columns []struct {
			Name string `json:"name"`
		} `json:"columnDefinitions"`
		Rows []struct {
			Cells []any `json:"cells"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return Table{}, fmt.Errorf("decode the table: %w", err)
	}
	if in.Kind != "Table" {
		return Table{}, fmt.Errorf("the API server answered a %q, not a Table", in.Kind)
	}
	var t Table
	for _, col := range in.Columns {
		t.Columns = append(t.Columns, col.Name)
	}
	for _, r := range in.Rows {
		row := make(map[string]string, len(t.Columns))
		for i, cell := range r.Cells {
			if i >= len(t.Columns) {
				break
			}
			if cell == nil {
				row[t.Columns[i]] = ""
				continue
			}
			row[t.Columns[i]] = fmt.Sprint(cell)
		}
		t.Rows = append(t.Rows, row)
	}
	return t, nil
}

// CreateSecret makes an Opaque Secret in the client's namespace. Its values are the caller's fake constants.
func (c *CRs) CreateSecret(ctx context.Context, name string, data map[string]string) error {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.ns}, StringData: data, Type: corev1.SecretTypeOpaque}
	if _, err := c.core.CoreV1().Secrets(c.ns).Create(ctx, s, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create secret %s/%s: %w", c.ns, name, err)
	}
	return nil
}

// SetSecretData changes the values of the keys given in a Secret of the client's namespace that exists, as a team does that fixes a token: a merge
// patch, so the other keys stay. The runner's Role allows it for the named Secret only. The values are the caller's fake constants.
func (c *CRs) SetSecretData(ctx context.Context, name string, data map[string]string) error {
	patch, err := json.Marshal(map[string]any{"stringData": data})
	if err != nil {
		return fmt.Errorf("encode the patch of secret %s/%s: %w", c.ns, name, err)
	}
	if _, err := c.core.CoreV1().Secrets(c.ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch secret %s/%s: %w", c.ns, name, err)
	}
	return nil
}

// roleName is the name of the Role and RoleBinding that grant Agent Kourier one Secret.
func roleName(secret string) string { return "agent-kourier-reads-" + secret }

// GrantSecret does what a team must do to let Agent Kourier read a Secret of the client's namespace (chart README, "Custom resources instead of a ConfigMap"):
// a Role that lets list and watch that Secret by name, and a RoleBinding of it to Agent Kourier's ServiceAccount. Without the binding the Role grants
// nothing.
func (c *CRs) GrantSecret(ctx context.Context, secret string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: roleName(secret), Namespace: c.ns},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{secret}, Verbs: []string{"list", "watch"}}},
	}
	if _, err := c.core.RbacV1().Roles(c.ns).Create(ctx, role, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create role %s/%s: %w", c.ns, role.Name, err)
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: roleName(secret), Namespace: c.ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: AgentKourierServiceAccount, Namespace: AgentKourierNamespace}},
	}
	if _, err := c.core.RbacV1().RoleBindings(c.ns).Create(ctx, binding, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create rolebinding %s/%s: %w", c.ns, binding.Name, err)
	}
	return nil
}

// RemoveSecret deletes the Secret of the client's namespace, and the Role and RoleBinding that grant it; whichever is not there is not an error.
func (c *CRs) RemoveSecret(ctx context.Context, secret string) error {
	var errs []error
	for _, del := range []func() error{
		func() error {
			return c.core.CoreV1().Secrets(c.ns).Delete(ctx, secret, metav1.DeleteOptions{})
		},
		func() error {
			return c.core.RbacV1().RoleBindings(c.ns).Delete(ctx, roleName(secret), metav1.DeleteOptions{})
		},
		func() error {
			return c.core.RbacV1().Roles(c.ns).Delete(ctx, roleName(secret), metav1.DeleteOptions{})
		},
	} {
		if err := del(); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("remove secret %s/%s and its grant: %w", c.ns, secret, err)
	}
	return nil
}
