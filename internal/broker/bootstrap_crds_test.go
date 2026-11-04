package broker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
)

var (
	crBindings    = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "bindings"}
	crBackends    = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "agentbackends"}
	crConnections = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "chatconnections"}

	// testConnection is the ChatConnection of spec 7's examples, which the broker in these tests serves.
	testConnection = v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "slack-main"}
)

// crObject returns spec 7's example file as the object a cluster holds: with a uid and a generation.
func crObject(t *testing.T, file string) *unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "api", "v1alpha1", "testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(b, &u.Object); err != nil {
		t.Fatal(err)
	}
	u.SetUID(types.UID("uid-" + u.GetName()))
	u.SetGeneration(1)
	return u
}

// crCluster returns a fake API server holding spec 7's example objects, edited by edit, which may be nil.
func crCluster(t *testing.T, edit func(objs []*unstructured.Unstructured)) *dynamicfake.FakeDynamicClient {
	t.Helper()
	objs := []*unstructured.Unstructured{
		crObject(t, "agentbackend.yaml"), crObject(t, "chatconnection.yaml"), crObject(t, "binding.yaml"), crObject(t, "binding-minimal.yaml"),
	}
	if edit != nil {
		edit(objs)
	}
	rt := make([]runtime.Object, len(objs))
	for i, o := range objs {
		rt[i] = o
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crBindings: "BindingList", crBackends: "AgentBackendList", crConnections: "ChatConnectionList",
	}, rt...)
}

func crOptions(dyn *dynamicfake.FakeDynamicClient) BootstrapOptions {
	o := options("")
	o.CRDs = &CRDSource{Client: dyn, Connection: testConnection}
	return o
}

func gvrResource(gvr schema.GroupVersionResource) schema.GroupResource { return gvr.GroupResource() }

// The custom resources of a cluster are read, and what the Secrets of the served objects need is followed, as with files.
func TestBootstrapCRDsServesTheObjectsOfTheCluster(t *testing.T) {
	before := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, before) })
	c, err := Bootstrap(t.Context(), cluster(), crOptions(crCluster(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if _, ok := c.Reloader.Current().Binding("sre", "sre-alerts"); !ok || c.Stale() {
		t.Errorf("after bootstrap: Binding found %t, stale %t; want the Binding and fresh", ok, c.Stale())
	}
	if rep := c.Reloader.Report(); len(rep.Rejections) != 0 || len(rep.Invalid) != 0 {
		t.Errorf("report = %+v; want nothing refused", rep)
	}
	if err := c.Secrets.Ready(); err != nil {
		t.Errorf("the Secrets are not ready: %v", err)
	}
}

// The failures of start that are the operator's to fix are said so by their code, and leave nothing running.
func TestBootstrapCRDsFailuresAtStart(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(dyn *dynamicfake.FakeDynamicClient, kube *fake.Clientset)
		edit    func([]*unstructured.Unstructured)
		is      error
		wiring  bool
		says    string
	}{
		"the CRDs are not installed": {
			prepare: func(dyn *dynamicfake.FakeDynamicClient, _ *fake.Clientset) {
				dyn.PrependReactor("list", "agentbackends", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewNotFound(gvrResource(crBackends), "")
				})
			},
			is: crsource.ErrCRDsAbsent, wiring: true, says: "agentbackends.agentkourier.dev",
		},
		"the custom resources are forbidden": {
			prepare: func(dyn *dynamicfake.FakeDynamicClient, _ *fake.Clientset) {
				dyn.PrependReactor("list", "bindings", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(gvrResource(crBindings), "", errors.New("RBAC"))
				})
			},
			is: crsource.ErrAccessDenied, says: "bindings.agentkourier.dev",
		},
		"the own connection does not exist": {
			edit:   func(objs []*unstructured.Unstructured) { objs[1].SetName("slack-other") },
			wiring: true, says: "ChatConnection agent-kourier-system/slack-main was not found",
		},
		"the own connection is invalid": {
			edit: func(objs []*unstructured.Unstructured) {
				_ = unstructured.SetNestedField(objs[1].Object, "teams", "spec", "platform")
			},
			wiring: true, says: "ChatConnection agent-kourier-system/slack-main is invalid",
		},
		"the own connection's Secret is denied": {
			prepare: func(_ *dynamicfake.FakeDynamicClient, kube *fake.Clientset) {
				kube.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
					if listOf(a) == "metadata.name=slack-main" {
						return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("RBAC"))
					}
					return false, nil, nil
				})
			},
			is: k8ssecrets.ErrAccessDenied, says: "agent-kourier-system/slack-main",
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := goleak.IgnoreCurrent()
			dyn, kube := crCluster(t, tc.edit), cluster()
			if tc.prepare != nil {
				tc.prepare(dyn, kube)
			}
			opts := crOptions(dyn)
			opts.SyncTimeout = 4 * time.Second

			c, err := Bootstrap(t.Context(), kube, opts)

			if c != nil {
				c.Stop()
				t.Fatal("Bootstrap succeeded")
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("Bootstrap = %v, want it to wrap %v", err, tc.is)
			}
			if got := errors.Is(err, ErrWiring); got != tc.wiring {
				t.Errorf("Bootstrap = %v; ErrWiring is %t, want %t", err, got, tc.wiring)
			}
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("Bootstrap = %v, want it to say %q", err, tc.says)
			}
			goleak.VerifyNone(t, before)
		})
	}
}

// One bad object is not a bad config: a Binding whose Secret is missing is routed, where the files would not load, and the
// Secret of a Binding that the API server denies makes the broker neither unready nor stuck at start.
func TestBootstrapCRDsServesWhatAFileConfigWouldRefuse(t *testing.T) {
	kube := fake.NewClientset(secretOf("agent-kourier-system", "slack-main", "botToken", "appToken")) // sre's token Secret is missing
	c, err := Bootstrap(t.Context(), kube, crOptions(crCluster(t, nil)))
	if err != nil {
		t.Fatalf("a Binding with a missing Secret stopped the broker: %v", err)
	}
	t.Cleanup(c.Stop)
	if _, ok := c.Reloader.Current().Binding("sre", "sre-alerts"); !ok {
		t.Error("the Binding whose Secret is missing is not routed")
	}

	denied := cluster()
	denied.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "sre" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("RBAC"))
		}
		return false, nil, nil
	})
	opts := crOptions(crCluster(t, nil))
	opts.SyncTimeout = 4 * time.Second
	begin := time.Now()
	d, err := Bootstrap(t.Context(), denied, opts)
	if err != nil {
		t.Fatalf("a Binding's Secret that is denied stopped the broker: %v", err)
	}
	t.Cleanup(d.Stop)
	if _, ok := d.Reloader.Current().Binding("sre", "sre-alerts"); !ok {
		t.Error("the Binding whose Secret is denied is not routed")
	}
	if err := d.Secrets.Ready(); err != nil {
		t.Errorf("a denied Secret of a Binding made the broker unready: %v", err)
	}
	if took := time.Since(begin); took > 3*time.Second {
		t.Errorf("Bootstrap took %s; a denial must end the wait at once", took)
	}
}

// A running broker keeps the last good generation of an object that is edited into an invalid one, says so in its report,
// and serves the fix when it comes. A Binding that appears in a namespace nobody had before, with a Secret in it, is served
// with no restart.
func TestBootstrapCRDsFollowsTheClusterWhileRunning(t *testing.T) {
	dyn, kube := crCluster(t, nil), cluster()
	c, err := Bootstrap(t.Context(), kube, crOptions(dyn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s did not happen in 10s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	update := func(gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
		t.Helper()
		if _, err := dyn.Resource(gvr).Namespace(u.GetNamespace()).Update(t.Context(), u, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	channelOf := func() string {
		rb, ok := c.Reloader.Current().Binding("sre", "sre-alerts")
		if !ok {
			return ""
		}
		return fmt.Sprintf("%s@%d", rb.Binding.Spec.Chat.Channel, rb.Binding.Generation)
	}

	// The informers' watches are open once the first sync is done, but the fake delivers to a watch only after it is
	// registered: repeat the update until the Reloader has seen it.
	bad := crObject(t, "binding.yaml")
	bad.SetGeneration(2)
	_ = unstructured.SetNestedField(bad.Object, "#general", "spec", "chat", "channel")
	waitFor("the invalid update to be reported", func() bool {
		update(crBindings, bad)
		return len(c.Reloader.Report().Invalid) == 1
	})
	rep := c.Reloader.Report()
	if got := channelOf(); got != "C0123456789@1" {
		t.Errorf("the Binding served is %s; want the last good generation, C0123456789@1", got)
	}
	if len(rep.Rejections) != 1 || rep.Rejections[0].Reason != "InvalidSpec" || rep.Rejections[0].Served != 1 || rep.Rejections[0].Generation != 2 {
		t.Errorf("rejections = %+v; want InvalidSpec for generation 2, serving 1", rep.Rejections)
	}

	fixed := crObject(t, "binding.yaml")
	fixed.SetGeneration(3)
	_ = unstructured.SetNestedField(fixed.Object, "C0123456780", "spec", "chat", "channel")
	waitFor("the fix to be served", func() bool { update(crBindings, fixed); return channelOf() == "C0123456780@3" })
	if rep := c.Reloader.Report(); len(rep.Invalid) != 0 || len(rep.Rejections) != 0 {
		t.Errorf("report after the fix = %+v; want nothing refused", rep)
	}

	// A new namespace with a Binding and the Secret it names.
	if _, err := kube.CoreV1().Secrets("payments").Create(t.Context(), secretOf("payments", "tok", "token"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	fresh := crObject(t, "binding.yaml")
	fresh.SetNamespace("payments")
	fresh.SetName("late")
	fresh.SetUID("uid-late")
	_ = unstructured.SetNestedField(fresh.Object, "C0999999999", "spec", "chat", "channel")
	_ = unstructured.SetNestedField(fresh.Object, "tok", "spec", "identity", "tokenSecretRef", "name")
	_ = unstructured.SetNestedField(fresh.Object, "payments-token", "spec", "identity", "userId")
	waitFor("the Binding in a new namespace to be served", func() bool {
		_, _ = dyn.Resource(crBindings).Namespace("payments").Create(t.Context(), fresh.DeepCopy(), metav1.CreateOptions{})
		_, ok := c.Reloader.Current().Binding("payments", "late")
		return ok
	})
	if _, err := c.Secrets.Keys(context.Background(), "payments", "tok"); err != nil {
		t.Errorf("the Secret of the Binding in the new namespace is not followed: %v", err)
	}
	if c.Stale() {
		t.Error("the config is stale")
	}
}
