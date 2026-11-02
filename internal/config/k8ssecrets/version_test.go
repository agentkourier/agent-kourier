package k8ssecrets

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// versioned returns a Secret with the given resourceVersion, keys and a value of the canary's.
func versioned(namespace, name, version string, keys ...string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, ResourceVersion: version},
		Data:       map[string][]byte{},
	}
	for _, k := range keys {
		s.Data[k] = []byte(canary + version)
	}
	return s
}

func TestSecretVersion(t *testing.T) {
	r := start(t, fake.NewClientset(versioned("sre", "creds", "41", "token")))
	got, err := r.SecretVersion(context.Background(), "sre", "creds")
	if err != nil || got != "41" {
		t.Fatalf("SecretVersion = %q, %v; want the cached resourceVersion", got, err)
	}
}

func TestSecretVersionErrors(t *testing.T) {
	r := start(t, fake.NewClientset(versioned("sre", "creds", "41", "token")))
	unstarted := New(fake.NewClientset(versioned("sre", "creds", "41", "token")), []string{"sre"})
	stopped := New(fake.NewClientset(versioned("sre", "creds", "41", "token")), []string{"sre"})
	if err := stopped.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	stopped.Stop()
	tests := []struct {
		name      string
		resolver  *Resolver
		namespace string
		secret    string
		want      error
		notWant   error
	}{
		{"missing Secret", r, "sre", "absent", config.ErrSecretNotFound, ErrNamespaceNotWatched},
		{"unwatched namespace", r, "kube-system", "creds", ErrNamespaceNotWatched, config.ErrSecretNotFound},
		{"cache not synced", unstarted, "sre", "creds", ErrNotSynced, config.ErrSecretNotFound},
		{"stopped", stopped, "sre", "creds", ErrStopped, config.ErrSecretNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.resolver.SecretVersion(context.Background(), tc.namespace, tc.secret)
			if !errors.Is(err, tc.want) || errors.Is(err, tc.notWant) || got != "" {
				t.Fatalf("SecretVersion = %q, %v; want no version and %v, not %v", got, err, tc.want, tc.notWant)
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("error contains a Secret value: %v", err)
			}
		})
	}
}

// watchesRegistered closes the returned channel once the fake clientset has a watch on Secrets
// in every one of namespaces. The fake tracker drops events for a watch that does not exist yet.
func watchesRegistered(client *fake.Clientset, namespaces ...string) <-chan struct{} {
	registered, mu, pending := make(chan struct{}), sync.Mutex{}, map[string]bool{}
	for _, ns := range namespaces {
		pending[ns] = true
	}
	client.PrependWatchReactor("secrets", func(a k8stesting.Action) (bool, watch.Interface, error) {
		w, err := client.Tracker().Watch(a.GetResource(), a.GetNamespace())
		mu.Lock()
		defer mu.Unlock()
		if pending[a.GetNamespace()] {
			delete(pending, a.GetNamespace())
			if len(pending) == 0 {
				close(registered)
			}
		}
		return true, w, err
	})
	return registered
}

// TestReloaderRevisionFollowsSecrets runs a Reloader over the informer cache. A rotation that
// keeps the keys, a deletion and a restore each change the revision without a reload, the
// revision depends on the version and never on a value, and reading it makes no API call.
func TestReloaderRevisionFollowsSecrets(t *testing.T) {
	const system, sre = "agent-kourier-system", "sre"
	client := fake.NewClientset(
		versioned(system, "slack-main", "1", "botToken", "appToken"),
		versioned(sre, "agent-kourier-sre-alerts-oidc", "1", "token"),
	)
	registered := watchesRegistered(client, system, sre)
	secrets := New(client, []string{system, sre}, WithLogger(slog.New(slog.DiscardHandler)))
	if err := secrets.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(secrets.Stop)
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("the informers never started their watches")
	}

	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../../api/v1alpha1/testdata")); err != nil {
		t.Fatal(err)
	}
	rl := config.NewReloader(config.ReloaderConfig{Dir: dir, Secrets: secrets, PollInterval: time.Hour, Logger: slog.New(slog.DiscardHandler)})
	if err := rl.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Wait)

	secretsAPI := client.CoreV1().Secrets(sre)
	actions := len(client.Actions())
	rev := rl.Revision(t.Context())
	if got := rl.Revision(t.Context()); got != rev || len(client.Actions()) != actions {
		t.Fatalf("revision = %q then %q, with %d new API calls; want a stable digest and none", rev, got, len(client.Actions())-actions)
	}

	// next waits for the cache to move the revision off prev and returns the new one.
	next := func(what, prev string) string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			got := rl.Revision(t.Context())
			if got != prev {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the revision never changed", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// The same key with a new value and a new resourceVersion, as a real update does.
	if _, err := secretsAPI.Update(t.Context(), versioned(sre, "agent-kourier-sre-alerts-oidc", "2", "token"), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	rotated := next("rotation", rev)
	if err := secretsAPI.Delete(t.Context(), "agent-kourier-sre-alerts-oidc", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	deleted := next("deletion", rotated)
	if _, err := secretsAPI.Create(t.Context(), versioned(sre, "agent-kourier-sre-alerts-oidc", "3", "token"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	restored := next("restore", deleted)
	if restored == rev || restored == rotated {
		t.Errorf("a restored Secret repeated an earlier revision")
	}

	// A value that changes under the same resourceVersion (which the API server does not do)
	// must not move the revision: it is built from the version, so no value can be in it.
	changed := versioned(sre, "agent-kourier-sre-alerts-oidc", "3", "token")
	changed.Data["token"] = []byte("another-" + canary)
	if _, err := secretsAPI.Update(t.Context(), changed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if v, err := secrets.Value(t.Context(), sre, "agent-kourier-sre-alerts-oidc", "token"); err == nil && strings.HasPrefix(v.Reveal(), "another-") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cache never took the new value")
		}
	}
	if got := rl.Revision(t.Context()); got != restored {
		t.Errorf("revision moved with a value under the same version: %q, want %q", got, restored)
	}
}
