package broker

import (
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// With the custom resources as the source, the broker that leads writes each Binding's status: it is a SQLite broker, so it leads
// at once, and its own writer is the one that Run builds. The status of a Binding it serves says it is accepted, and the one of a
// Binding that is invalid says why, and neither is probed yet or has a reason to be ready.
func TestRunWritesTheStatusOfTheBindingsItServes(t *testing.T) {
	c := crStartConfig(t, func(objs []*unstructured.Unstructured) {
		_ = unstructured.SetNestedField(objs[3].Object, "#general", "spec", "chat", "channel") // chat-only is invalid
	})
	cluster := newStatusClusterOn(c.Dynamic.(*dynamicfake.FakeDynamicClient))
	c.StatusClient = cluster.dyn
	r := start(t, c)
	r.until("/readyz", http.StatusOK)

	conds := func(key string) map[string]metav1.Condition { return cluster.stored(t, key) }
	timeout := time.After(30 * time.Second)
	for {
		if _, served := conds("sre/sre-alerts")["Accepted"]; served {
			if _, rejected := conds("sre/chat-only")["Accepted"]; rejected {
				break
			}
		}
		select {
		case <-cluster.applied:
		case <-timeout:
			t.Fatalf("the statuses were not written: applies = %d", cluster.count())
		}
	}
	if a := conds("sre/sre-alerts")["Accepted"]; a.Status != metav1.ConditionTrue || a.ObservedGeneration != 1 {
		t.Errorf("served Accepted = %+v, want True at generation 1", a)
	}
	if a := conds("sre/chat-only")["Accepted"]; a.Status != metav1.ConditionFalse || a.Reason != "InvalidSpec" {
		t.Errorf("invalid Accepted = %+v, want False/InvalidSpec", a)
	}
	if rd := conds("sre/chat-only")["Ready"]; rd.Reason != "Accepted:InvalidSpec" {
		t.Errorf("invalid Ready = %+v, want the reason Accepted:InvalidSpec", rd)
	}
	for _, ap := range cluster.snapshotApplies() {
		if ap.Options.FieldManager != statusFieldManager || ap.Subresource != "status" {
			t.Errorf("an apply was made as %q on %q, want %q on status", ap.Options.FieldManager, ap.Subresource, statusFieldManager)
		}
	}
	if _, body := r.get("/metrics"); !strings.Contains(body, `agentkourier_binding_status_writes_total{result="written"}`) {
		t.Errorf("/metrics does not count the writes:\n%s", grep(body, "status_writes"))
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Errorf("Run = %v after a signal; want nil", err)
	}
}

// On the files route nothing of the status writer exists: the client that a test gives is never used, no series of the metric is
// made, and the broker runs as it did.
func TestRunWithFilesMakesNoStatusWriter(t *testing.T) {
	c := startConfig(t)
	cluster := newStatusClusterOn(crClusterOf())
	c.StatusClient = cluster.dyn
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	_, body := r.get("/metrics")
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}
	if acts := cluster.dyn.Actions(); len(acts) != 0 {
		t.Errorf("the status client was used %d times with files: %v", len(acts), acts)
	}
	if strings.Contains(body, "status_writes") {
		t.Errorf("/metrics has a series of the status writer with files:\n%s", grep(body, "status_writes"))
	}
}
