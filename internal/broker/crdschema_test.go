package broker

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// schemaStateOf reads the state that the gauge holds for a CRD: the one state whose series is 1; it fails unless exactly one is.
func schemaStateOf(t *testing.T, m *Metrics, crd string) string {
	t.Helper()
	got, ok := schemaStateNow(t, m, crd)
	if !ok {
		t.Fatalf("CRD %s has no one state set", crd)
	}
	return got
}

// schemaStateNow is schemaStateOf that says false, instead of failing, when no state or more than one is set.
func schemaStateNow(t *testing.T, m *Metrics, crd string) (string, bool) {
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
		return "", false
	}
	return set[0], true
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

// schemaSoon polls cond every few milliseconds, and fails the test if it is not true within 10 s.
func schemaSoon(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(3 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
	}
}

// runWatch runs watchCRDSchemas against client with a short period until the gauge holds a state for every CRD and several passes have been
// made, then stops it and waits for it to return. It returns the metrics and what was logged.
func runWatch(t *testing.T, client *dynamicfake.FakeDynamicClient, during func()) (*Metrics, string) {
	t.Helper()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	m, err := NewMetrics(nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); watchCRDSchemas(ctx, client, log, m, 4*time.Millisecond) }()
	schemaSoon(t, "the gauge holds a state for every CRD", func() bool {
		for _, n := range v1alpha1.CRDNames() {
			if _, ok := schemaStateNow(t, m, n); !ok {
				return false
			}
		}
		return true
	})
	if during != nil {
		during()
	}
	time.Sleep(60 * time.Millisecond) // many passes: the log must not repeat itself
	cancel()
	<-done
	return m, logs.String()
}

func countLines(logs, level string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level="+level) {
			n++
		}
	}
	return n
}

// TestWatchCRDSchemas: each CRD's annotation is compared with the hash the broker was built with. A match is quiet; a mismatch warns, naming the
// CRD and both hashes; a CRD with no annotation is told, not warned of, since a CRD from an older chart has none; one that cannot be read
// warns and is unknown. However many passes are made, each CRD is logged once. Nothing stops the broker.
func TestWatchCRDSchemas(t *testing.T) {
	names := v1alpha1.CRDNames()
	built := func(i int) string { h, _ := v1alpha1.SchemaSHA256(names[i]); return h }
	other := strings.Repeat("0", 64)
	annotated := func(i int, hash string) *unstructured.Unstructured {
		return crdObject(names[i], map[string]string{v1alpha1.SchemaHashAnnotation: hash})
	}
	tests := []struct {
		name     string
		client   func() *dynamicfake.FakeDynamicClient
		want     [3]string
		warnings int
		infos    int
		contains []string
	}{
		{"all three match", func() *dynamicfake.FakeDynamicClient {
			return crdCluster(annotated(0, built(0)), annotated(1, built(1)), annotated(2, built(2)))
		}, [3]string{SchemaMatch, SchemaMatch, SchemaMatch}, 0, 0, nil},
		{"one is of another schema", func() *dynamicfake.FakeDynamicClient {
			return crdCluster(annotated(0, built(0)), annotated(1, other), annotated(2, built(2)))
		}, [3]string{SchemaMatch, SchemaMismatch, SchemaMatch}, 1, 0, []string{names[1], other, built(1), "not what the API server serves"}},
		{"CRDs from an older chart carry no hash", func() *dynamicfake.FakeDynamicClient {
			return crdCluster(crdObject(names[0], nil), crdObject(names[1], map[string]string{"other": "x"}), annotated(2, ""))
		}, [3]string{SchemaUnannotated, SchemaUnannotated, SchemaUnannotated}, 0, 3, []string{"has no schema hash"}},
		{"a CRD that is not there", func() *dynamicfake.FakeDynamicClient {
			return crdCluster(annotated(0, built(0)), annotated(2, built(2)))
		}, [3]string{SchemaMatch, SchemaUnknown, SchemaMatch}, 1, 0, []string{names[1], "not there"}},
		{"a get that is forbidden", func() *dynamicfake.FakeDynamicClient {
			c := crdCluster(annotated(0, built(0)), annotated(1, built(1)), annotated(2, built(2)))
			c.PrependReactor("get", "customresourcedefinitions", func(a k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(crdResource.GroupResource(), a.(k8stesting.GetAction).GetName(), nil)
			})
			return c
		}, [3]string{SchemaUnknown, SchemaUnknown, SchemaUnknown}, 3, 0, []string{"may not get the CRD", names[0], names[1], names[2]}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, logs := runWatch(t, tc.client(), nil)
			for i, want := range tc.want {
				if got := schemaStateOf(t, m, names[i]); got != want {
					t.Errorf("%s state %q; want %q", names[i], got, want)
				}
			}
			if got := countLines(logs, "WARN"); got != tc.warnings {
				t.Errorf("%d warnings; want %d:\n%s", got, tc.warnings, logs)
			}
			if got := countLines(logs, "INFO"); got != tc.infos {
				t.Errorf("%d info lines; want %d:\n%s", got, tc.infos, logs)
			}
			for _, w := range tc.contains {
				if !strings.Contains(logs, w) {
					t.Errorf("the log lacks %q:\n%s", w, logs)
				}
			}
		})
	}
}

// TestWatchFollowsAChangeOfTheCRDs: a chart upgrade that changes the CRDs under a pinned image rolls no pod, so the comparison is made again: a
// CRD that falls out of step is warned of once, and one that is brought back is said to match, once, and the gauge follows each.
func TestWatchFollowsAChangeOfTheCRDs(t *testing.T) {
	names := v1alpha1.CRDNames()
	built, _ := v1alpha1.SchemaSHA256(names[1])
	other := strings.Repeat("f", 64)
	set := func(c *dynamicfake.FakeDynamicClient, hash string) {
		t.Helper()
		u, err := c.Resource(crdResource).Get(t.Context(), names[1], metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		u.SetAnnotations(map[string]string{v1alpha1.SchemaHashAnnotation: hash})
		if _, err := c.Resource(crdResource).Update(t.Context(), u, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	c := crdCluster(crdObject(names[0], map[string]string{v1alpha1.SchemaHashAnnotation: mustHash(names[0])}),
		crdObject(names[1], map[string]string{v1alpha1.SchemaHashAnnotation: built}), crdObject(names[2], nil))

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	m, err := NewMetrics(nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); watchCRDSchemas(ctx, c, log, m, 4*time.Millisecond) }()
	state := func() string { s, _ := schemaStateNow(t, m, names[1]); return s }
	schemaSoon(t, "a match", func() bool { return state() == SchemaMatch })
	set(c, other)
	schemaSoon(t, "a mismatch after the CRD changed", func() bool { return state() == SchemaMismatch })
	time.Sleep(40 * time.Millisecond)
	set(c, built)
	schemaSoon(t, "a match again", func() bool { return state() == SchemaMatch })
	time.Sleep(40 * time.Millisecond)
	cancel()
	<-done
	out := logs.String()
	if got := countLines(out, "WARN"); got != 1 || !strings.Contains(out, other) {
		t.Errorf("%d warnings; want one that names the hash found, %s:\n%s", got, other, out)
	}
	if !strings.Contains(out, "now matches") || strings.Count(out, "now matches") != 1 {
		t.Errorf("want one line saying the CRD now matches:\n%s", out)
	}
}

func mustHash(name string) string { h, _ := v1alpha1.SchemaSHA256(name); return h }

// TestWatchStopsWithItsContext: the watch returns when its context ends, in the middle of a pass or between two, and a stop is not a finding.
func TestWatchStopsWithItsContext(t *testing.T) {
	m, err := NewMetrics(nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchCRDSchemas(ctx, crdCluster(), slog.New(slog.NewTextHandler(&logs, nil)), m, time.Hour)
	}()
	schemaSoon(t, "the watch returns", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	if logs.Len() != 0 || gatherNames(t, m)["agentkourier_crd_schema_state"] {
		t.Errorf("a stopped watch logged %q or set the gauge", logs.String())
	}
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
// comparison; with the custom resources it reads each CRD after Bootstrap, and a CRD it may not read, or that is of another schema, does not stop it.
func TestRunComparesTheSchemasOnlyOnTheCustomResources(t *testing.T) {
	t.Run("files", func(t *testing.T) {
		c := startConfig(t)
		dyn := crdCluster()
		c.Dynamic = dyn // given and never used: the config source is files
		r := start(t, c)
		r.until("/readyz", http.StatusOK)
		time.Sleep(100 * time.Millisecond)
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
		good := mustHash(names[0])
		c := crStartConfig(t, nil)
		dyn := c.Dynamic.(*dynamicfake.FakeDynamicClient)
		dyn.PrependReactor("get", "customresourcedefinitions", func(a k8stesting.Action) (bool, runtime.Object, error) {
			name := a.(k8stesting.GetAction).GetName()
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
		for _, want := range []string{
			`agentkourier_crd_schema_state{crd="` + names[0] + `",state="match"} 1`,
			`agentkourier_crd_schema_state{crd="` + names[1] + `",state="mismatch"} 1`,
			`agentkourier_crd_schema_state{crd="` + names[2] + `",state="unknown"} 1`,
			`agentkourier_crd_schema_state{crd="` + names[2] + `",state="match"} 0`,
		} {
			r.waitMetric(want)
		}
		r.cancel()
		if err := r.wait(); err != nil {
			t.Errorf("Run = %v after a signal; want nil (the watch must stop with the broker)", err)
		}
	})
}
