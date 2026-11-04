package broker

import (
	"bytes"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
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

// crElectionConfig is electionConfig for a replica that reads the custom resources of a fake cluster that takes applies of a status.
func crElectionConfig(t *testing.T, identity string) (RunConfig, *replica, *statusCluster) {
	t.Helper()
	c, rep := electionConfig(t, cluster(), &fakeFence{}, identity)
	c.ConfigSource, c.ConfigDir, c.ChatConnection = SourceCRDs, "", testConnection
	dyn := crCluster(t, nil)
	cl := newStatusClusterOn(dyn)
	c.Dynamic, c.StatusClient = dyn, cl.dyn
	return c, rep, cl
}

// The status writer is behind the lease fence: a leader whose lease is past its renew deadline (the clock makes it a minute old at
// every reading) writes no status, and the process is stopped once, with exit 5, for the reason that a status write was refused.
// Without the fence wired to the writer, the statuses would be written and nothing would stop.
func TestRunALeaderPastItsRenewDeadlineWritesNoStatus(t *testing.T) {
	c, rep, cl := crElectionConfig(t, "pod-a_1")
	c.leaseNow = stepping()
	rep.running = start(t, c)

	select {
	case code := <-rep.exits:
		if code != ExitLeadershipLost {
			t.Errorf("exit code = %d; want %d", code, ExitLeadershipLost)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the process did not stop: a leader past its renew deadline wrote statuses (%d)\n%s", cl.count(), rep.logs.String())
	}
	if n := cl.count(); n != 0 {
		t.Errorf("%d statuses were written; want none: a refused write never leaves the process", n)
	}
	if logs := rep.logs.String(); !strings.Contains(logs, "a status write (") {
		t.Errorf("the log does not say that a status write was refused:\n%s", logs)
	}
}

// The control of the one above: with a lease as young as renewals keep it the same leader writes the statuses and nothing stops it.
func TestRunALeaderWhoseLeaseIsFreshWritesTheStatuses(t *testing.T) {
	c, rep, cl := crElectionConfig(t, "pod-a_1")
	now := time.Now()
	c.leaseNow = func() time.Time { return now }
	rep.running = start(t, c)

	eventually(t, 10*time.Second, "no status was written", func() bool {
		return len(cl.stored(t, "sre/sre-alerts")) > 0 && len(cl.stored(t, "sre/chat-only")) > 0
	})
	select {
	case code := <-rep.exits:
		t.Errorf("the process stopped with exit %d; want it to lead\n%s", code, rep.logs.String())
	default:
	}
}

// The status writer stops after the verification of the Bindings, whose State it reads, and not before it. The order is that of the
// stops' names as the shutdown runs them.
func TestRunStopsTheStatusWriterAfterTheVerification(t *testing.T) {
	var mu sync.Mutex
	var stops []string
	c := crStartConfig(t, nil)
	c.stopTrace = func(name string) { mu.Lock(); stops = append(stops, name); mu.Unlock() }
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	verify, status := slices.Index(stops, "binding-verification"), slices.Index(stops, "binding-status")
	if verify < 0 || status < 0 || status < verify {
		t.Errorf("stops = %v; want binding-status after binding-verification", stops)
	}
}

// The counter of the client's warnings is the writer's: a write that the API server answers with Warnings is said once in the log,
// with the number. (Run builds the counter and gives it to the client and the writer; a test that gives a client feeds the counter
// itself.)
func TestRunTheWritersWarningsAreTheCountersOfTheClient(t *testing.T) {
	c := crStartConfig(t, nil)
	logs := &lockedBuffer{}
	c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	c.leaveKlog = true
	warnings := &warningCounter{}
	c.statusWarnings = warnings
	cluster := newStatusClusterOn(c.Dynamic.(*dynamicfake.FakeDynamicClient))
	cluster.warn, cluster.warnN = warnings, 7
	c.StatusClient = cluster.dyn
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	eventually(t, 10*time.Second, "the warnings were not said", func() bool {
		text := logs.String()
		return strings.Contains(text, "the API server warned") && strings.Contains(text, "warnings_per_write=7")
	})
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}
}

// lockedBuffer is a buffer that the broker's goroutines may write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
