package broker

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"

	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// crStartConfig is startConfig that reads the custom resources of a fake cluster, edited by edit, and serves the
// ChatConnection of spec 7's examples, with no config directory.
func crStartConfig(t *testing.T, edit func([]*unstructured.Unstructured)) RunConfig {
	t.Helper()
	c := startConfig(t)
	c.ConfigSource, c.ConfigDir, c.ChatConnection = SourceCRDs, "", testConnection
	dyn := crCluster(t, edit)
	c.Dynamic, c.StatusClient = dyn, newStatusClusterOn(dyn).dyn // a cluster that takes the applies of a status, which a leader writes
	return c
}

// With the custom resources as the source the broker reads them, serves, and is ready as it is on files; the config it
// reads is the cluster's, and its directory is not looked at.
func TestRunServesTheCustomResources(t *testing.T) {
	c := crStartConfig(t, nil)
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	code, body := r.get("/metrics")
	if code != http.StatusOK || !strings.Contains(body, `agentkourier_config_invalid_objects{kind="Binding"} 0`) {
		t.Errorf("/metrics = %d; want the gauge of invalid Bindings at 0", code)
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Errorf("Run = %v after a signal; want nil", err)
	}
}

// One object that does not pass validation does not stop the broker or make it unready, and is counted: a Binding that
// is invalid when the broker starts is left out, and the others are served.
func TestRunDoesNotStopForOneInvalidObject(t *testing.T) {
	c := crStartConfig(t, func(objs []*unstructured.Unstructured) {
		_ = unstructured.SetNestedField(objs[2].Object, "#general", "spec", "chat", "channel") // sre-alerts
	})
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	_, body := r.get("/metrics")
	if !strings.Contains(body, `agentkourier_config_invalid_objects{kind="Binding"} 1`) {
		t.Errorf("/metrics does not count the invalid Binding:\n%s", grep(body, "agentkourier_config_"))
	}
	if !strings.Contains(body, `agentkourier_config_binding_rejections{reason="InvalidSpec"} 1`) {
		t.Errorf("/metrics does not count the Binding as rejected for its spec:\n%s", grep(body, "agentkourier_config_"))
	}
}

// With no CRDs installed the broker does not start, and says it is the wiring that is wrong, which exits 2.
func TestRunWithoutTheCRDsIsAWiringError(t *testing.T) {
	c := crStartConfig(t, nil)
	c.Dynamic.(interface {
		PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
	}).PrependReactor("list", "chatconnections", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(crConnections.GroupResource(), "")
	})
	err := start(t, c).wait()
	if !errors.Is(err, ErrWiring) || !errors.Is(err, crsource.ErrCRDsAbsent) {
		t.Errorf("Run = %v; want ErrWiring wrapping ErrCRDsAbsent", err)
	}
}

// The broker's own connection, if missing at start, stops it with a wiring error that names it.
func TestRunWithoutItsOwnConnectionIsAWiringError(t *testing.T) {
	c := crStartConfig(t, nil)
	c.ChatConnection.Name = "another"
	err := start(t, c).wait()
	if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "agent-kourier-system/another was not found") {
		t.Errorf("Run = %v; want ErrWiring saying the connection was not found", err)
	}
}

// grep returns the lines of text that hold sub.
func grep(text, sub string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// The retention skip is wired through Run: the sessions of a Binding that exists in the cluster and is not served (its spec
// is invalid and it has no good generation) outlive a retention tick that deletes those of a Binding that was deleted, which
// are the same age. Without the skip both fall to the same horizon and both go.
func TestRunKeepsTheSessionsOfAnUnservedBinding(t *testing.T) {
	c := crStartConfig(t, func(objs []*unstructured.Unstructured) {
		_ = unstructured.SetNestedField(objs[2].Object, "#general", "spec", "chat", "channel") // sre-alerts is invalid, and new
	})
	c.retentionEvery = 10 * time.Millisecond
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	unserved := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "1.1"}
	deleted := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "2.2"}
	for k, binding := range map[store.ConversationKey]string{unserved: "sre/sre-alerts", deleted: "sre/long-gone"} {
		if err := seed.UpsertSession(t.Context(), store.Session{Key: k, Binding: binding, LastActivity: time.Now().Add(-400 * day)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) { // the store's own metric says the sweep ran
		if _, body := r.get("/metrics"); strings.Contains(body, `operation="agentkourier.store.PurgeExpiredSessions"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session purge never ran")
		}
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for k, want := range map[store.ConversationKey]bool{unserved: true, deleted: false} {
		_, err := st.GetSession(t.Context(), k, 100*365*day, time.Now())
		if present := err == nil; present != want {
			t.Errorf("session %s present = %v (%v); want %v", k.Thread, present, err, want)
		}
	}
}
