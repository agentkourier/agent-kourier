package k8ssecrets

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/testutil"
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
	r := follow(t, newAPI(t, versioned("sre", "creds", "41", "token")), sn("sre", "creds"))
	got, err := r.SecretVersion(context.Background(), "sre", "creds")
	if err != nil || got != "41" {
		t.Fatalf("SecretVersion = %q, %v; want the cached resourceVersion", got, err)
	}
}

func TestSecretVersionErrors(t *testing.T) {
	client := newAPI(t, versioned("sre", "creds", "41", "token"), versioned("sre", "other", "7", "token"))
	r := follow(t, client, sn("sre", "creds"), sn("sre", "absent"))
	stopped := follow(t, client, sn("sre", "creds"))
	stopped.Stop()
	tests := []struct {
		name      string
		resolver  *Resolver
		namespace string
		secret    string
		want      error
		notWant   error
	}{
		{"missing Secret", r, "sre", "absent", config.ErrSecretNotFound, ErrNotWatched},
		{"a Secret that exists but is not followed", r, "sre", "other", ErrNotWatched, config.ErrSecretNotFound},
		{"cache not synced", unsynced(t), "sre", "creds", ErrNotSynced, config.ErrSecretNotFound},
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

// While the list and watch fail, the cache keeps its last contents: a Secret rotated meanwhile
// moves neither SecretVersion nor Value, and the failure is in the log. This is what the
// Revision and SecretVersion docs say; if the cache ever starts reporting the failure, they and
// this test change together.
func TestSecretVersionStaysWhileTheWatchFails(t *testing.T) {
	client := newAPI(t, versioned("sre", "creds", "1", "token"))
	var failing atomic.Bool
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing.Load() {
			return true, nil, errors.New("connection refused")
		}
		return false, nil, nil
	})
	held := watch.NewFake() // the one watch the informer gets before the failure; the test ends it
	client.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) {
		if failing.Load() {
			return true, nil, errors.New("connection refused")
		}
		return true, held, nil
	})
	logs := &logBuffer{}
	r := New(client, WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	t.Cleanup(r.Stop)
	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "creds")}); err != nil {
		t.Fatal(err)
	}
	failures := func() int { return strings.Count(logs.String(), "secret informer cannot list or watch") }
	waitFor := func(what string, done func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !done(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s never happened; log: %q", what, logs.String())
			}
		}
	}

	failing.Store(true)
	held.Stop()
	waitFor("the first failure in the log", func() bool { return failures() > 0 })
	if _, err := client.CoreV1().Secrets("sre").Update(t.Context(), versioned("sre", "creds", "2", "token"), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Every later list and watch fails too, so nothing can carry the update into the cache.
	if got, err := r.SecretVersion(t.Context(), "sre", "creds"); err != nil || got != "1" {
		t.Errorf("SecretVersion = %q, %v; want the version cached before the failure", got, err)
	}
	if v, err := r.Value(t.Context(), "sre", "creds", "token"); err != nil || v.Reveal() != canary+"1" {
		t.Errorf("Value = %v; want the value cached before the failure", err)
	}
	if strings.Contains(logs.String(), canary) {
		t.Errorf("log %q holds a Secret value", logs.String())
	}
}

// TestReloaderRevisionFollowsSecrets runs a Reloader over the informer cache. A rotation that
// keeps the keys, a deletion and a restore each change the revision without a reload, the
// revision depends on the version and never on a value, and reading it makes no API call.
func TestReloaderRevisionFollowsSecrets(t *testing.T) {
	const system, sre = "agent-kourier-system", "sre"
	client := newAPI(t,
		versioned(system, "slack-main", "1", "botToken", "appToken"),
		versioned(sre, "agent-kourier-sre-alerts-oidc", "1", "token"),
	)
	secrets := New(client, WithLogger(testutil.DiscardLogger()))
	t.Cleanup(secrets.Stop)

	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../../api/v1alpha1/testdata")); err != nil {
		t.Fatal(err)
	}
	rl := config.NewReloader(config.ReloaderConfig{Dir: dir, Secrets: secrets, PollInterval: time.Hour, Logger: testutil.DiscardLogger()})
	if err := rl.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Wait)
	// The Reloader asked for the Secrets of the config, and the fake tracker drops events for a watch that does not exist yet.
	client.watching(t, sn(system, "slack-main"), sn(sre, "agent-kourier-sre-alerts-oidc"))

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
