package broker

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// crdObject is a CustomResourceDefinition of the given name that carries the annotations given (none if annotations is nil).
func crdObject(name string, annotations map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": name},
	}}
	u.SetAnnotations(annotations)
	return u
}

// crdCluster is a fake cluster that holds the given CRDs.
func crdCluster(objs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	rt := make([]runtime.Object, len(objs))
	for i, o := range objs {
		rt[i] = o
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crdResource: "CustomResourceDefinitionList",
	}, rt...)
}

// schemaStateOf reads the state that the gauge holds for a CRD: the one state whose series is 1, and whether exactly one is.
func schemaStateOf(t *testing.T, m *Metrics, crd string) string {
	t.Helper()
	families, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var set []string
	for _, f := range families {
		if f.GetName() != "agentkourier_crd_schema_state" {
			continue
		}
		for _, metric := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range metric.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["crd"] == crd && metric.GetGauge().GetValue() == 1 {
				set = append(set, labels["state"])
			}
		}
	}
	if len(set) != 1 {
		t.Fatalf("CRD %s has states %v set; want exactly one", crd, set)
	}
	return set[0]
}

func gatherNames(t *testing.T, m *Metrics) map[string]bool {
	t.Helper()
	families, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range families {
		names[f.GetName()] = true
	}
	return names
}

// TestCheckCRDSchemas: each CRD's annotation is compared with the hash the broker was built with. A match is quiet; a mismatch warns, naming the
// CRD and both hashes; a CRD with no annotation is told, not warned of, since a perfectly good install made by hand has none; one that cannot be
// read warns and is unknown. Nothing stops the broker: the function returns nothing.
func TestCheckCRDSchemas(t *testing.T) {
	names := v1alpha1.CRDNames()
	built := func(i int) string { h, _ := v1alpha1.SchemaSHA256(names[i]); return h }
	other := strings.Repeat("0", 64)
	annotated := func(i int, hash string) *unstructured.Unstructured {
		return crdObject(names[i], map[string]string{v1alpha1.SchemaHashAnnotation: hash})
	}
	tests := []struct {
		name      string
		client    func() *dynamicfake.FakeDynamicClient
		want      [3]string
		warns     []string // substrings of the warnings, in any order
		infoWords string   // a substring of an info line that must be there, if not empty
	}{
		{"all three match", func() *dynamicfake.FakeDynamicClient {
			return crdCluster(annotated(0, built(0)), annotated(1, built(1)), annotated(2, built(2)))
		}, [3]string{SchemaMatch, SchemaMatch, SchemaMatch}, nil, ""},
		{"one is of another schema", func() *dynamicfake.FakeDynamicClient {
			return crdCluster(annotated(0, built(0)), annotated(1, other), annotated(2, built(2)))
		}, [3]string{SchemaMatch, SchemaMismatch, SchemaMatch}, []string{names[1], other, built(1)}, ""},
		{"CRDs made by hand carry no hash", func() *dynamicfake.FakeDynamicClient {
			return crdCluster(crdObject(names[0], nil), crdObject(names[1], map[string]string{"other": "x"}), annotated(2, ""))
		}, [3]string{SchemaUnannotated, SchemaUnannotated, SchemaUnannotated}, nil, "no schema hash"},
		{"a CRD that is not there", func() *dynamicfake.FakeDynamicClient {
			return crdCluster(annotated(0, built(0)), annotated(2, built(2)))
		}, [3]string{SchemaMatch, SchemaUnknown, SchemaMatch}, []string{names[1], "not there"}, ""},
		{"a get that is forbidden", func() *dynamicfake.FakeDynamicClient {
			c := crdCluster(annotated(0, built(0)), annotated(1, built(1)), annotated(2, built(2)))
			c.PrependReactor("get", "customresourcedefinitions", func(a k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(crdResource.GroupResource(), a.(k8stesting.GetAction).GetName(), nil)
			})
			return c
		}, [3]string{SchemaUnknown, SchemaUnknown, SchemaUnknown}, []string{"may not get the CRD", names[0], names[1], names[2]}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
			m, err := NewMetrics(nil)
			if err != nil {
				t.Fatal(err)
			}
			checkCRDSchemas(context.Background(), tc.client(), log, m)
			for i, want := range tc.want {
				if got := schemaStateOf(t, m, names[i]); got != want {
					t.Errorf("%s state %q; want %q", names[i], got, want)
				}
			}
			var warnings []string
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "level=WARN") {
					warnings = append(warnings, line)
				}
			}
			if len(warnings) != countWarnings(tc.want) {
				t.Errorf("%d warnings; want %d:\n%s", len(warnings), countWarnings(tc.want), logs.String())
			}
			for _, w := range tc.warns {
				if !strings.Contains(logs.String(), w) {
					t.Errorf("the log lacks %q:\n%s", w, logs.String())
				}
			}
			if tc.infoWords != "" && !strings.Contains(logs.String(), `level=INFO msg="a CRD has `+tc.infoWords) {
				t.Errorf("no info line with %q:\n%s", tc.infoWords, logs.String())
			}
		})
	}
}

// countWarnings is how many warnings a result calls for: one for each CRD that mismatches or is unknown, none for a match or a CRD without a hash.
func countWarnings(states [3]string) int {
	n := 0
	for _, s := range states {
		if s == SchemaMismatch || s == SchemaUnknown {
			n++
		}
	}
	return n
}

// TestCRDSchemaGaugeHasNoSeriesUntilSet: the files route never sets it, so /metrics there holds no series of it and does not move.
func TestCRDSchemaGaugeHasNoSeriesUntilSet(t *testing.T) {
	m, err := NewMetrics(nil)
	if err != nil {
		t.Fatal(err)
	}
	if gatherNames(t, m)["agentkourier_crd_schema_state"] {
		t.Error("agentkourier_crd_schema_state has series before anything set it")
	}
	m.CRDSchema("bindings.agentkourier.dev", "no such state")
	if got := schemaStateOf(t, m, "bindings.agentkourier.dev"); got != SchemaUnknown {
		t.Errorf("a state outside the set reads as %q; want %q", got, SchemaUnknown)
	}
}

// TestRunComparesTheSchemasOnlyOnTheCustomResources: with files the broker makes no call to read a CRD and /metrics holds no series of the
// comparison; with the custom resources it reads each CRD once at start, and a CRD it may not read, or that is of another schema, does not stop it.
func TestRunComparesTheSchemasOnlyOnTheCustomResources(t *testing.T) {
	t.Run("files", func(t *testing.T) {
		c := startConfig(t)
		dyn := crdCluster()
		c.Dynamic = dyn // given and never used: the config source is files
		r := start(t, c)
		r.until("/readyz", http.StatusOK)
		_, body := r.get("/metrics")
		if strings.Contains(body, "agentkourier_crd_schema_state") {
			t.Errorf("/metrics on files holds the schema gauge:\n%s", grep(body, "crd_schema"))
		}
		for _, a := range dyn.Actions() {
			t.Errorf("the files route called the API server: %v", a)
		}
	})
	t.Run("custom resources", func(t *testing.T) {
		names := v1alpha1.CRDNames()
		good, _ := v1alpha1.SchemaSHA256(names[0])
		c := crStartConfig(t, nil)
		dyn := c.Dynamic.(*dynamicfake.FakeDynamicClient)
		gets := map[string]int{}
		dyn.PrependReactor("get", "customresourcedefinitions", func(a k8stesting.Action) (bool, runtime.Object, error) {
			name := a.(k8stesting.GetAction).GetName()
			gets[name]++
			switch name {
			case names[0]:
				return true, crdObject(name, map[string]string{v1alpha1.SchemaHashAnnotation: good}), nil
			case names[1]:
				return true, crdObject(name, map[string]string{v1alpha1.SchemaHashAnnotation: strings.Repeat("0", 64)}), nil
			}
			return true, nil, apierrors.NewForbidden(crdResource.GroupResource(), name, nil)
		})
		r := start(t, c)
		r.until("/readyz", http.StatusOK)
		_, body := r.get("/metrics")
		for _, want := range []string{
			`agentkourier_crd_schema_state{crd="` + names[0] + `",state="match"} 1`,
			`agentkourier_crd_schema_state{crd="` + names[1] + `",state="mismatch"} 1`,
			`agentkourier_crd_schema_state{crd="` + names[2] + `",state="unknown"} 1`,
			`agentkourier_crd_schema_state{crd="` + names[2] + `",state="match"} 0`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("/metrics lacks %s:\n%s", want, grep(body, "crd_schema"))
			}
		}
		for _, n := range names {
			if gets[n] != 1 {
				t.Errorf("CRD %s was got %d times; want once, at start", n, gets[n])
			}
		}
	})
}
