//go:build crdcorpus

package crdcorpus

import (
	"context"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// kubeconfigEnv names the kubeconfig of the cluster to test against. `make crd-corpus` sets it, to a cluster that has
// the CRDs and nothing else.
const kubeconfigEnv = "CRD_CORPUS_KUBECONFIG"

var (
	resources = map[string]schema.GroupVersionResource{
		"AgentBackend":   {Group: "agentkourier.dev", Version: "v1alpha1", Resource: "agentbackends"},
		"ChatConnection": {Group: "agentkourier.dev", Version: "v1alpha1", Resource: "chatconnections"},
		"Binding":        {Group: "agentkourier.dev", Version: "v1alpha1", Resource: "bindings"},
	}
	crds          = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	namespacesGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
)

// client returns a dynamic client for the cluster in the kubeconfig that kubeconfigEnv names, and refuses a cluster
// that is not on this machine: the test creates Namespaces (the dry-run creates alone change nothing), so it must never
// be pointed at a shared cluster by a stray kubeconfig.
func client(t *testing.T) dynamic.Interface {
	t.Helper()
	path := os.Getenv(kubeconfigEnv)
	if path == "" {
		t.Fatalf("%s is not set: run `make crd-corpus`, which brings up a kind cluster with the CRDs and runs this test", kubeconfigEnv)
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
		t.Fatalf("API server %s is not on this machine; the corpus test creates Namespaces and runs only against a local kind cluster", cfg.Host)
	}
	// A few hundred requests to a cluster on this machine: the client's default of 5 a second would make it minutes.
	cfg.QPS, cfg.Burst = 200, 400
	c, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// requireCRDs fails unless all three CRDs are Established: the dry-runs below mean nothing against a cluster whose CRDs
// are missing or still coming up.
func requireCRDs(ctx context.Context, t *testing.T, c dynamic.Interface) {
	t.Helper()
	for _, gvr := range resources {
		name := gvr.Resource + "." + gvr.Group
		var established bool
		for deadline := time.Now().Add(30 * time.Second); !established && time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			crd, err := c.Resource(crds).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("CRD %s: %v: apply charts/agent-kourier/files/crds first", name, err)
			}
			conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
			for _, cond := range conds {
				m, _ := cond.(map[string]any)
				established = established || (m["type"] == "Established" && m["status"] == "True")
			}
		}
		if !established {
			t.Fatalf("CRD %s is not Established after 30s", name)
		}
	}
}

// ensureNamespaces creates the namespaces the entries name, for the dry-run create of a namespaced object finds the
// namespace first. A name that is not a namespace is left to the entry that tests it.
func ensureNamespaces(ctx context.Context, t *testing.T, c dynamic.Interface, entries []Entry) {
	t.Helper()
	seen := map[string]bool{}
	for _, e := range entries {
		ns := e.Namespace
		if ns == "" || seen[ns] || len(validation.IsDNS1123Label(ns)) > 0 {
			continue
		}
		seen[ns] = true
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": ns},
		}}
		if _, err := c.Resource(namespacesGVR).Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create namespace %s: %v", ns, err)
		}
	}
}

// schemaRefuses sends the entry to the apiserver as a server-side dry-run create, with strict field validation as
// `kubectl apply` has it, and says whether the apiserver refuses it. Any other failure is returned as an error, since
// it says nothing about the schema.
func schemaRefuses(ctx context.Context, c dynamic.Interface, e Entry) (refused bool, reason string, err error) {
	gvr, ok := resources[e.Kind]
	if !ok {
		return false, "", apierrors.NewBadRequest("unknown kind " + e.Kind)
	}
	ns := e.Namespace
	if ns == "" {
		ns = "default"
	}
	obj := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(e.Object)}
	_, err = c.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldValidation: "Strict"})
	switch {
	case err == nil:
		return false, "", nil
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		return true, err.Error(), nil
	}
	return false, "", err
}

// mapKey matches a map key in an apiserver's field path, extract[state], which the corpus writes as extract.state.
var mapKey = regexp.MustCompile(`\[([^\]0-9][^\]]*)\]`)

// namesField says whether the apiserver's refusal names the field, as the corpus writes it, or the list item it is in:
// a list that is a set or a map is refused at the item, as approverGroups[1], and the item's own field is not named.
func namesField(refusal, field string) bool {
	if field == "" {
		return false
	}
	refusal = mapKey.ReplaceAllString(refusal, ".$1")
	if strings.Contains(refusal, field) {
		return true
	}
	item, _, ok := strings.Cut(field, "].")
	return ok && strings.Contains(refusal, item+"]")
}

// TestTheSchemaNeverRefusesWhatTheBrokerAccepts is the invariant of D50(2), run against an apiserver that has the
// CRDs: every object the schema refuses, the broker refuses too, and every valid object passes both. The valid half is
// what the broker accepts (TestEveryValidEntryIsAcceptedByTheBroker), so the schema must accept all of it; the
// invalid half is what the broker refuses (TestEveryInvalidEntryIsRefusedByTheBroker), so whatever the schema refuses
// of it is within the invariant, and what it accepts is the broker's alone. Each invalid entry says which of the two it
// is, so a marker that stops refusing, or a rule that starts to, fails here until the corpus follows it.
func TestTheSchemaNeverRefusesWhatTheBrokerAccepts(t *testing.T) {
	ctx := t.Context()
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	valid, err := Valid(root)
	if err != nil {
		t.Fatal(err)
	}
	invalid, err := Invalid(root)
	if err != nil {
		t.Fatal(err)
	}
	// The job runs the unit tests of this package too, but this test holds the corpus to its size itself, so that it
	// cannot pass on a half that was emptied: every loop below would succeed on nothing.
	if err := CheckSize(valid, invalid); err != nil {
		t.Fatal(err)
	}
	c := client(t)
	requireCRDs(ctx, t, c)
	ensureNamespaces(ctx, t, c, append(slices.Clone(valid), invalid...))

	var validRefused int
	t.Run("valid", func(t *testing.T) {
		for _, e := range valid {
			t.Run(e.Name, func(t *testing.T) {
				refused, reason, err := schemaRefuses(ctx, c, e)
				switch {
				case err != nil:
					t.Fatalf("dry-run create failed for a reason that is not the schema's: %v", err)
				case refused:
					validRefused++
					t.Errorf("the schema refuses an object the broker accepts: %s", reason)
				}
			})
		}
	})

	var brokerOnly []string
	t.Run("invalid", func(t *testing.T) {
		for _, e := range invalid {
			if e.SkipCluster != "" {
				continue
			}
			t.Run(e.Name, func(t *testing.T) {
				refused, reason, err := schemaRefuses(ctx, c, e)
				if err != nil {
					t.Fatalf("dry-run create failed for a reason that is not the schema's: %v", err)
				}
				if refused && Broker(ctx, e) == nil {
					t.Errorf("the schema refuses an object the broker accepts: %s", reason)
				}
				switch {
				case refused && !e.SchemaRefuses:
					t.Errorf("the schema refuses this entry, which is marked 'schema: accepts': %s", reason)
				case refused && !namesField(reason, e.Field):
					t.Errorf("the schema refuses this entry, but not at %q, the field of its rule (add a '# field:' line if the apiserver names another): %s", e.Field, reason)
				case !refused && e.SchemaRefuses:
					t.Errorf("the schema accepts this entry, which is marked 'schema: refuses'")
				}
				if !refused {
					brokerOnly = append(brokerOnly, e.Name)
				}
			})
		}
	})

	var skipped int
	for _, e := range invalid {
		if e.SkipCluster != "" {
			skipped++
		}
	}
	t.Logf("%d of %d valid entries pass the schema. Of %d invalid entries, the schema refuses %d, the broker alone refuses %d, and %d are not sent to an apiserver.",
		len(valid)-validRefused, len(valid), len(invalid), len(invalid)-len(brokerOnly)-skipped, len(brokerOnly), skipped)
	t.Logf("what only the broker refuses, the work of the CEL rules to come:\n  %s", strings.Join(brokerOnly, "\n  "))
}
