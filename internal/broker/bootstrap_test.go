package broker

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
)

// configDir copies spec 7's example config, which refers to Secrets in courier-system and sre.
func configDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../api/v1alpha1/testdata")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func secretOf(namespace, name string, keys ...string) *corev1.Secret {
	data := map[string][]byte{}
	for _, k := range keys {
		data[k] = []byte("fake-value")
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Data: data}
}

// cluster holds the Secrets that the example config refers to.
func cluster() *fake.Clientset {
	return fake.NewClientset(
		secretOf("courier-system", "slack-main", "botToken", "appToken"),
		secretOf("sre", "courier-sre-alerts-oidc", "token"),
		secretOf("sre", "courier-webhook-token", "token"),
	)
}

func options(dir string) BootstrapOptions {
	return BootstrapOptions{
		Dir: dir, SyncTimeout: 30 * time.Second, PollInterval: 5 * time.Millisecond, SecretsInterval: time.Hour,
		Logger: slog.New(slog.DiscardHandler),
	}
}

// The example config needs courier-system and sre. Starting with those derived, and only those,
// loads it: a missing namespace would fail the first load with ErrNamespaceNotWatched.
func TestBootstrapWatchesTheNamespacesTheConfigNames(t *testing.T) {
	c, err := Bootstrap(t.Context(), cluster(), options(configDir(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if _, ok := c.Reloader.Current().Binding("sre", "sre-alerts"); !ok || c.Stale() {
		t.Errorf("after bootstrap: Binding found %t, stale %t; want the Binding and fresh", ok, c.Stale())
	}
}

func TestBootstrapFailsAtTheFirstDenialAndNamesIt(t *testing.T) {
	client := cluster()
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() != "sre" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("RBAC: no list"))
	})
	opts := options(configDir(t))
	opts.SyncTimeout = 4 * time.Second

	begin := time.Now()
	c, err := Bootstrap(t.Context(), client, opts)

	if c != nil {
		c.Stop()
		t.Fatal("Bootstrap succeeded without the sre namespace")
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("Bootstrap = %v, want an error wrapping the forbidden cause", err)
	}
	if took := time.Since(begin); took > 2*time.Second {
		t.Errorf("Bootstrap took %s, want it to fail at the first denial, well before its 4s timeout", took)
	}
}

// A reload cannot watch a namespace it did not start with (no hot rebuild, WP-35 ruling 6), so a
// Binding added in a new namespace fails the reload. The last good snapshot stays in force, the
// reloader is marked stale for the health endpoint, and the hook counts the error once.
func TestReloadThatAddsAnUnwatchedNamespaceKeepsTheSnapshot(t *testing.T) {
	dir := configDir(t)
	failures := make(chan error, 8)
	opts := options(dir)
	opts.OnReloadError = func(err error) { failures <- err }
	c, err := Bootstrap(t.Context(), cluster(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	before := c.Reloader.Current()

	added := `apiVersion: agentcourier.dev/v1alpha1
kind: Binding
metadata: { name: pay, namespace: payments }
spec:
  agent: { backendRef: { namespace: courier-system, name: kagent-pilot }, name: k8s-agent, namespace: kagent }
  identity: { tokenSecretRef: { name: pay-token } }
  chat: { connectionRef: { namespace: courier-system, name: slack-main }, channel: C0PAYMENTS01 }
`
	if err := os.WriteFile(filepath.Join(dir, "payments.yaml"), []byte(added), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-failures:
		if !errors.Is(err, k8ssecrets.ErrNamespaceNotWatched) {
			t.Errorf("reload failed with %v, want ErrNamespaceNotWatched", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reload never failed")
	}
	if c.Reloader.Current() != before {
		t.Error("the snapshot changed although the reload failed")
	}
	if !c.Stale() {
		t.Error("the reloader is not stale after a failed reload")
	}
	select {
	case err := <-failures:
		t.Errorf("a second failure was counted: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}
