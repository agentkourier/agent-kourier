//go:build crdcorpus

package crdcorpus

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// newerCRDNamespace is the namespace of the objects that TestAFreshBrokerServesUnderCRDsThatAreNewerThanItself makes, and removes.
const newerCRDNamespace = "crd-newer"

// withDefaultedField adds to the CRD a field of the spec with a default, under the properties of the object at path, as a later release of the
// CRDs would. It returns the CRD's spec as it was, to put back.
func withDefaultedField(t *testing.T, crd *unstructured.Unstructured, path ...string) (original map[string]any) {
	t.Helper()
	spec, _, _ := unstructured.NestedMap(crd.Object, "spec")
	original = runtime.DeepCopyJSON(spec)
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	version := versions[0].(map[string]any)
	node := version["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	for _, p := range path {
		node = node["properties"].(map[string]any)[p].(map[string]any)
	}
	node["properties"].(map[string]any)["futureField"] = map[string]any{"type": "string", "default": "defaulted-by-a-newer-crd"}
	if err := unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"); err != nil {
		t.Fatal(err)
	}
	return original
}

// TestAFreshBrokerServesUnderCRDsThatAreNewerThanItself holds the source to what an operator meets when CRDs are applied before an image, or an
// image alone is rolled back: a later release gave the Binding's chat and the ChatConnection's spec a field with a default, and the API server adds
// it to every stored object it returns, with no change of generation. A broker that starts then is older than its CRDs. It must read what it read
// before, the Binding and the ChatConnection it serves, and refuse nothing. A decoder that refused a field it has no type for read no Binding here:
// each came back as `unknown field`.
func TestAFreshBrokerServesUnderCRDsThatAreNewerThanItself(t *testing.T) {
	ctx := t.Context()
	c := client(t)
	requireCRDs(ctx, t, c)

	cleanup := context.WithoutCancel(ctx)
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": newerCRDNamespace}}}
	if _, err := c.Resource(namespacesGVR).Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace %s (a run that was killed leaves it): %v", newerCRDNamespace, err)
	}
	t.Cleanup(func() { _ = c.Resource(namespacesGVR).Delete(cleanup, newerCRDNamespace, metav1.DeleteOptions{}) })

	create := func(kind, name string, spec map[string]any) *unstructured.Unstructured {
		t.Helper()
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "agentkourier.dev/v1alpha1", "kind": kind,
			"metadata": map[string]any{"name": name, "namespace": newerCRDNamespace}, "spec": spec,
		}}
		got, err := c.Resource(resources[kind]).Namespace(newerCRDNamespace).Create(ctx, obj, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create %s %s: %v", kind, name, err)
		}
		return got
	}
	create("ChatConnection", "slack-newer", map[string]any{"platform": "slack", "credentialsSecretRef": map[string]any{"name": "slack-newer"}})
	create("AgentBackend", "backend-newer", map[string]any{"dialect": "a2a", "url": "http://backend.example:8083"})
	binding := create("Binding", "binding-newer", map[string]any{
		"agent":    map[string]any{"backendRef": map[string]any{"name": "backend-newer"}, "namespace": "agents", "name": "agent"},
		"identity": map[string]any{"userId": "newer"},
		"chat":     map[string]any{"connectionRef": map[string]any{"name": "slack-newer"}, "channel": "C0NEWERCRD"},
	})

	// The newer CRDs: a field with a default on the Binding's chat and on the ChatConnection's spec.
	for _, upgrade := range []struct {
		resource string
		path     []string
	}{{"bindings.agentkourier.dev", []string{"spec", "chat"}}, {"chatconnections.agentkourier.dev", []string{"spec"}}} {
		crd, err := c.Resource(crds).Get(ctx, upgrade.resource, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		original := withDefaultedField(t, crd, upgrade.path...)
		if _, err := c.Resource(crds).Update(ctx, crd, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("update CRD %s: %v", upgrade.resource, err)
		}
		t.Cleanup(func() { restoreCRD(cleanup, t, c, upgrade.resource, original) })
	}

	// The API server adds the default on the next read of a stored object, as the schema it serves reaches it. The generation does not move.
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := c.Resource(resources["Binding"]).Namespace(newerCRDNamespace).Get(ctx, "binding-newer", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if v, _, _ := unstructured.NestedString(got.Object, "spec", "chat", "futureField"); v != "" {
			if got.GetGeneration() != binding.GetGeneration() {
				t.Fatalf("generation %d after the CRD changed, was %d: the defaulting must not change it", got.GetGeneration(), binding.GetGeneration())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the API server did not add the default to the stored Binding in 30s: the CRD change did not take")
		}
		time.Sleep(250 * time.Millisecond)
	}

	// A broker that starts now, as the pod of an image older than the CRDs does.
	src, err := crsource.New(crsource.Options{Client: c, Connection: v1alpha1.ObjectRef{Namespace: newerCRDNamespace, Name: "slack-newer"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Stop)
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := src.Start(startCtx); err != nil {
		t.Fatal(err)
	}
	set, _, err := src.Set(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Bindings) != 1 || len(set.ChatConnections) != 1 || len(set.AgentBackends) != 1 || len(set.Invalid) != 0 {
		t.Fatalf("the broker reads %d Bindings, %d connections, %d backends, and refuses %+v; want one of each and nothing refused",
			len(set.Bindings), len(set.ChatConnections), len(set.AgentBackends), set.Invalid)
	}
}

// restoreCRD puts the spec of a CRD back as it was, which is what the other tests of the package need.
func restoreCRD(ctx context.Context, t *testing.T, c dynamic.Interface, name string, original map[string]any) {
	t.Helper()
	crd, err := c.Resource(crds).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Errorf("restore CRD %s: %v", name, err)
		return
	}
	if err := unstructured.SetNestedMap(crd.Object, original, "spec"); err != nil {
		t.Errorf("restore CRD %s: %v", name, err)
		return
	}
	if _, err := c.Resource(crds).Update(ctx, crd, metav1.UpdateOptions{}); err != nil {
		t.Errorf("restore CRD %s: %v", name, err)
	}
}
