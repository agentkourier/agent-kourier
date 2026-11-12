package broker

import (
	"errors"
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

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
	"github.com/agentkourier/agent-kourier/internal/testutil"
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
	)
}

func options(dir string) BootstrapOptions {
	return BootstrapOptions{
		Dir: dir, SyncTimeout: 30 * time.Second, PollInterval: 5 * time.Millisecond, SecretsInterval: time.Hour,
		Logger: testutil.DiscardLogger(),
	}
}

// The example config refers to a Secret in agent-kourier-system and one in sre. Starting with those, and only those,
// loads it: a Secret that was not followed would fail the first load with ErrNotWatched.
func TestBootstrapFollowsTheSecretsTheConfigNames(t *testing.T) {
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

// addPayments writes the Binding of a team whose Secret is in a namespace that the config did not refer to at start. The
// file appears whole. A plain WriteFile truncates first, and the reloader, polling every 5 ms, can load the empty file (a
// good reload, which replaces the snapshot with an equal one, so "the snapshot is unchanged" would be false) or see the file
// change twice (two failures). A ConfigMap mount, which is how the config arrives, is replaced atomically too.
func addPayments(t *testing.T, dir string) {
	t.Helper()
	added := `apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: { name: pay, namespace: payments }
spec:
  agent: { backendRef: { namespace: agent-kourier-system, name: kagent-pilot }, name: k8s-agent, namespace: kagent }
  identity: { tokenSecretRef: { name: pay-token } }
  chat: { connectionRef: { namespace: agent-kourier-system, name: slack-main }, channel: C0PAYMENTS01 }
`
	staging := filepath.Join(dir, ".payments.yaml.tmp") // a dot name is not a config file
	if err := os.WriteFile(staging, []byte(added), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, filepath.Join(dir, "payments.yaml")); err != nil {
		t.Fatal(err)
	}
}

// A reload that refers to a Secret in a namespace that the config did not name at start is served: the resolver follows the
// new Secret, and the Binding is live with no restart. The Secret that the reload no longer refers to is let go, and
// readiness holds throughout.
func TestReloadThatAddsASecretInANewNamespaceNeedsNoRestart(t *testing.T) {
	dir := configDir(t)
	client := cluster()
	if _, err := client.CoreV1().Secrets("payments").Create(t.Context(), secretOf("payments", "pay-token", "token"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	opts := options(dir)
	opts.OnReloadError = func(err error) { t.Errorf("a reload failed: %v", err) }
	c, err := Bootstrap(t.Context(), client, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if _, err := c.Secrets.Keys(t.Context(), "payments", "pay-token"); !errors.Is(err, k8ssecrets.ErrNotWatched) {
		t.Fatalf("before the reload: Keys of the new namespace's Secret = %v, want ErrNotWatched", err)
	}

	addPayments(t, dir)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := c.Reloader.Current().Binding("payments", "pay"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Binding of the new namespace never went live; stale %t", c.Stale())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.Stale() {
		t.Error("the reloader is stale after a good reload")
	}
	if keys, err := c.Secrets.Keys(t.Context(), "payments", "pay-token"); err != nil || len(keys) != 1 {
		t.Errorf("Keys of the new Secret = %v, %v; want its key", keys, err)
	}
	for _, check := range c.Checks() {
		if err := check.Fn(t.Context()); err != nil {
			t.Errorf("%s is not ready after the reload: %v", check.Name, err)
		}
	}
}

// A reload that refers to a Secret that Agent Kourier may not read fails: the last good snapshot stays in force, the
// reloader is marked stale for the health endpoint, the hook counts the error once, and the error says that access was
// denied. Readiness is not affected: the broker still serves the config it has.
func TestReloadThatAddsAForbiddenSecretKeepsTheSnapshot(t *testing.T) {
	dir := configDir(t)
	client := cluster()
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.ListAction).GetListRestrictions().Fields.String() != "metadata.name=pay-token" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "pay-token", errors.New("RBAC: no list"))
	})
	failures := make(chan error, 8)
	opts := options(dir)
	opts.OnReloadError = func(err error) { failures <- err }
	c, err := Bootstrap(t.Context(), client, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	before := c.Reloader.Current()

	addPayments(t, dir)

	select {
	case err := <-failures:
		if !errors.Is(err, k8ssecrets.ErrAccessDenied) || !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "payments/pay-token") {
			t.Errorf("reload failed with %v, want ErrAccessDenied wrapping the answer and naming payments/pay-token", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reload never failed")
	}
	if c.Reloader.Current() != before {
		t.Error("the snapshot changed although the reload failed")
	}
	if !c.Stale() {
		t.Error("the reloader is not stale after a failed reload")
	}
	for _, check := range c.Checks() {
		if err := check.Fn(t.Context()); err != nil {
			t.Errorf("%s is not ready after a failed reload: %v", check.Name, err)
		}
	}
	if _, err := c.Secrets.Keys(t.Context(), "sre", "agent-kourier-sre-alerts-oidc"); err != nil {
		t.Errorf("a Secret of the last good snapshot is no longer readable: %v", err)
	}
	select {
	case err := <-failures:
		t.Errorf("a second failure was counted: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// A Secret that no resource of the config refers to, such as the Postgres DSN's, is read through the same caches once it is
// asked for; otherwise it is not followed.
func TestBootstrapFollowsTheExtraSecrets(t *testing.T) {
	client := cluster()
	if _, err := client.CoreV1().Secrets("agent-kourier").Create(t.Context(), secretOf("agent-kourier", "agent-kourier-pg", "dsn"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for name, extra := range map[string][]config.SecretName{"asked": {{Namespace: "agent-kourier", Name: "agent-kourier-pg"}}, "not asked": nil} {
		t.Run(name, func(t *testing.T) {
			o := options(configDir(t))
			o.ExtraSecrets = extra
			c, err := Bootstrap(t.Context(), client, o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(c.Stop)
			// Keys never follows a Secret, so it tells whether the Secret was followed from the start.
			_, err = c.Secrets.Keys(t.Context(), "agent-kourier", "agent-kourier-pg")
			if (extra != nil) != (err == nil) || (extra == nil && !errors.Is(err, k8ssecrets.ErrNotWatched)) {
				t.Errorf("Keys of the extra Secret = %v, with extra Secrets %v", err, extra)
			}
		})
	}
}

// The Secret of the Postgres DSN stays readable through every reload, though no config names it.
func TestExtraSecretsSurviveAReload(t *testing.T) {
	dir := configDir(t)
	client := cluster()
	for _, s := range []*corev1.Secret{secretOf("agent-kourier", "agent-kourier-pg", "dsn"), secretOf("payments", "pay-token", "token")} {
		if _, err := client.CoreV1().Secrets(s.Namespace).Create(t.Context(), s, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	o := options(dir)
	o.ExtraSecrets = []config.SecretName{{Namespace: "agent-kourier", Name: "agent-kourier-pg"}}
	c, err := Bootstrap(t.Context(), client, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)

	addPayments(t, dir)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, ok := c.Reloader.Current().Binding("payments", "pay"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reload never published the Binding")
		}
	}

	if _, err := c.Secrets.Value(t.Context(), "agent-kourier", "agent-kourier-pg", "dsn"); err != nil {
		t.Errorf("the DSN's Secret after a reload: %v", err)
	}
}
