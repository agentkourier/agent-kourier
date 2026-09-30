package k8ssecrets

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// canary is planted in every place a Secret holds a value; it must never come back out.
const canary = "s3cr3t-value-canary"

// TestMain fails the run if any informer goroutine outlives Stop.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func secret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        name,
			Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": canary},
		},
		Data:       map[string][]byte{"token": []byte(canary), "alpha": []byte(canary)},
		StringData: map[string]string{"middle": canary},
	}
}

// start returns a synced Resolver over namespaces "sre" and "payments", stopped at cleanup.
func start(t *testing.T, client *fake.Clientset) *Resolver {
	t.Helper()
	r := New(client, []string{"sre", "payments"})
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	return r
}

func TestKeys(t *testing.T) {
	r := start(t, fake.NewClientset(secret("sre", "creds"), secret("payments", "creds")))
	got, err := r.Keys(context.Background(), "sre", "creds")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "middle", "token"}; !slices.Equal(got, want) {
		t.Errorf("Keys = %v, want %v (sorted, Data and StringData)", got, want)
	}
}

func TestKeysErrors(t *testing.T) {
	r := start(t, fake.NewClientset(secret("sre", "creds")))
	unstarted := New(fake.NewClientset(secret("sre", "creds")), []string{"sre"})
	tests := []struct {
		name      string
		resolver  *Resolver
		namespace string
		secret    string
		want      error
		notWant   error
	}{
		{"missing Secret", r, "sre", "absent", config.ErrSecretNotFound, ErrNamespaceNotWatched},
		{"missing Secret in another watched namespace", r, "payments", "creds", config.ErrSecretNotFound, ErrNamespaceNotWatched},
		{"unwatched namespace", r, "kube-system", "creds", ErrNamespaceNotWatched, config.ErrSecretNotFound},
		{"cache not synced", unstarted, "sre", "creds", ErrNotSynced, config.ErrSecretNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.resolver.Keys(context.Background(), tc.namespace, tc.secret)
			if !errors.Is(err, tc.want) || errors.Is(err, tc.notWant) {
				t.Fatalf("err = %v, want %v and not %v", err, tc.want, tc.notWant)
			}
			// Every Secret in these clients holds the canary; no error path may echo it.
			if strings.Contains(err.Error(), canary) {
				t.Errorf("error contains a Secret value: %v", err)
			}
		})
	}
}

// watchRegistered makes the fake clientset close the returned channel once the "sre" watch
// exists. The fake tracker drops events for a watch that is not registered yet, and Start
// returns once the list has synced.
func watchRegistered(client *fake.Clientset) <-chan struct{} {
	registered, once := make(chan struct{}), sync.Once{}
	client.PrependWatchReactor("secrets", func(a k8stesting.Action) (bool, watch.Interface, error) {
		w, err := client.Tracker().Watch(a.GetResource(), a.GetNamespace())
		if a.GetNamespace() == "sre" {
			once.Do(func() { close(registered) })
		}
		return true, w, err
	})
	return registered
}

func TestCacheHoldsKeysOnly(t *testing.T) {
	r := start(t, fake.NewClientset(secret("sre", "creds")))
	s, err := r.namespaces["sre"].lister.Secrets("sre").Get("creds")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Data) != 3 {
		t.Errorf("cached Data has %d keys, want 3", len(s.Data))
	}
	for k, v := range s.Data {
		if v != nil {
			t.Errorf("cached Data[%q] holds a value", k)
		}
	}
	if s.StringData != nil {
		t.Errorf("cached StringData = %v, want nil", s.StringData)
	}
	if want := (metav1.ObjectMeta{Namespace: "sre", Name: "creds", ResourceVersion: s.ResourceVersion}); !reflect.DeepEqual(s.ObjectMeta, want) {
		t.Errorf("cached ObjectMeta = %+v, want only name, namespace and resourceVersion", s.ObjectMeta)
	}
}

// TestPicksUpSecretCreatedAfterStart also proves the informers outlive the context passed to
// Start, which a caller will bound with a timeout.
func TestPicksUpSecretCreatedAfterStart(t *testing.T) {
	client := fake.NewClientset()
	registered := watchRegistered(client)
	r := New(client, []string{"sre"})
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithCancel(t.Context())
	err := r.Start(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("the informer never started its watch")
	}
	if _, err := r.Keys(context.Background(), "sre", "late"); !errors.Is(err, config.ErrSecretNotFound) {
		t.Fatalf("before create: err = %v", err)
	}
	if _, err := client.CoreV1().Secrets("sre").Create(context.Background(), secret("sre", "late"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		keys, err := r.Keys(context.Background(), "sre", "late")
		if err == nil {
			if len(keys) != 3 {
				t.Errorf("keys = %v", keys)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Secret never appeared in the cache: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestKeysAfterStopFails(t *testing.T) {
	r := New(fake.NewClientset(secret("sre", "creds")), []string{"sre"})
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.Stop()
	if _, err := r.Keys(context.Background(), "sre", "creds"); !errors.Is(err, ErrStopped) {
		t.Errorf("Keys after Stop = %v, want ErrStopped", err)
	}
}

func TestNamespacesSyncIndependently(t *testing.T) {
	client := fake.NewClientset(secret("sre", "creds"))
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "payments" {
			return true, nil, errors.New("forbidden")
		}
		return false, nil, nil
	})
	r := New(client, []string{"sre", "payments"})
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := r.Start(ctx); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `["payments"]`) {
		t.Errorf("Start = %v, want a deadline error naming only payments", err)
	}
	if _, err := r.Keys(context.Background(), "sre", "creds"); err != nil {
		t.Errorf("synced namespace: %v", err)
	}
	if _, err := r.Keys(context.Background(), "payments", "creds"); !errors.Is(err, ErrNotSynced) {
		t.Errorf("unsynced namespace: err = %v, want ErrNotSynced", err)
	}
}

// TestStopUnblocksStart is the shutdown case: Start waits on a namespace that never syncs,
// with a context that never ends, and Stop must still release it.
func TestStopUnblocksStart(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	r := New(client, []string{"payments"})
	started := make(chan error, 1)
	go func() { started <- r.Start(context.Background()) }()
	r.Stop()
	select {
	case err := <-started:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Start = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start still blocked after Stop")
	}
}
