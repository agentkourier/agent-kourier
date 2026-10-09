package broker

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
)

// configDir copies spec 7's example config, which refers to Secrets in agent-kourier-system and sre.
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
		secretOf("agent-kourier-system", "slack-main", "botToken", "appToken"),
		secretOf("sre", "agent-kourier-sre-alerts-oidc", "token"),
		secretOf("sre", "agent-kourier-webhook-token", "token"),
	)
}

func options(dir string) BootstrapOptions {
	return BootstrapOptions{
		Dir: dir, SyncTimeout: 30 * time.Second, PollInterval: 5 * time.Millisecond, SecretsInterval: time.Hour,
		Logger: slog.New(slog.DiscardHandler),
	}
}

// The example config needs agent-kourier-system and sre. Starting with those derived, and only those,
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

// When the Reloader cannot load the config, the resolver Bootstrap started must be stopped, or its
// informers outlive the failed start.
func TestBootstrapStopsTheResolverWhenTheConfigDoesNotResolve(t *testing.T) {
	before := goleak.IgnoreCurrent()
	client := fake.NewClientset( // the Binding's token Secret, sre/agent-kourier-sre-alerts-oidc, is missing
		secretOf("agent-kourier-system", "slack-main", "botToken", "appToken"),
		secretOf("sre", "agent-kourier-webhook-token", "token"),
	)

	c, err := Bootstrap(t.Context(), client, options(configDir(t)))

	if err == nil || c != nil {
		t.Fatalf("Bootstrap = %v, %v; want an error and no Config", c, err)
	}
	if !strings.Contains(err.Error(), "agent-kourier-sre-alerts-oidc") {
		t.Errorf("Bootstrap = %v, want it to name the missing Secret", err)
	}
	goleak.VerifyNone(t, before)
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

	added := `apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: { name: pay, namespace: payments }
spec:
  agent: { backendRef: { namespace: agent-kourier-system, name: kagent-pilot }, name: k8s-agent, namespace: kagent }
  identity: { tokenSecretRef: { name: pay-token } }
  chat: { connectionRef: { namespace: agent-kourier-system, name: slack-main }, channel: C0PAYMENTS01 }
`
	// The file appears whole. A plain WriteFile truncates first, and the reloader, polling every
	// 5 ms, can load the empty file (a good reload, which replaces the snapshot with an equal one,
	// so "the snapshot is unchanged" would be false) or see the file change twice (two failures).
	// A ConfigMap mount, which is how the config arrives, is replaced atomically too.
	staging := filepath.Join(dir, ".payments.yaml.tmp") // a dot name is not a config file
	if err := os.WriteFile(staging, []byte(added), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, filepath.Join(dir, "payments.yaml")); err != nil {
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

// A Secret that no resource of the config refers to, such as the Postgres DSN's, is read through the same caches once its
// namespace is asked for; otherwise the namespace is not watched.
func TestBootstrapWatchesTheExtraNamespaces(t *testing.T) {
	client := cluster()
	if _, err := client.CoreV1().Secrets("agent-kourier").Create(t.Context(), secretOf("agent-kourier", "agent-kourier-pg", "dsn"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for name, extra := range map[string][]string{"asked": {"agent-kourier", "sre"}, "not asked": nil} {
		t.Run(name, func(t *testing.T) {
			o := options(configDir(t))
			o.ExtraNamespaces = extra
			c, err := Bootstrap(t.Context(), client, o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(c.Stop)
			_, err = c.Secrets.Value(t.Context(), "agent-kourier", "agent-kourier-pg", "dsn")
			if (extra != nil) != (err == nil) || (extra == nil && !errors.Is(err, k8ssecrets.ErrNamespaceNotWatched)) {
				t.Errorf("Value of the extra namespace's Secret = %v, with extra namespaces %v", err, extra)
			}
			if extra != nil && len(c.namespaces) != 3 { // agent-kourier-system and sre from the config, Agent Kourier; sre once
				t.Errorf("namespaces = %v, want the config's two and the extra one, each once", c.namespaces)
			}
		})
	}
}
