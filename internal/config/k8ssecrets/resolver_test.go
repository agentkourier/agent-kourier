package k8ssecrets

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config"
	secretval "github.com/agentkourier/agent-kourier/internal/secret"
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

// TestCacheKeepsOnlyIdentityAndData: the values are cached for Value, but nothing else of the
// Secret is, least of all the annotation that can hold a copy of them.
func TestCacheKeepsOnlyIdentityAndData(t *testing.T) {
	r := start(t, fake.NewClientset(secret("sre", "creds")))
	s, err := r.namespaces["sre"].lister.Secrets("sre").Get("creds")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Data) != 3 {
		t.Errorf("cached Data has %d keys, want 3", len(s.Data))
	}
	for k, v := range s.Data {
		if string(v) != canary {
			t.Errorf("cached Data[%q] = %q, want the value", k, v)
		}
	}
	if s.StringData != nil {
		t.Errorf("cached StringData = %v, want nil", s.StringData)
	}
	if want := (metav1.ObjectMeta{Namespace: "sre", Name: "creds", ResourceVersion: s.ResourceVersion}); !reflect.DeepEqual(s.ObjectMeta, want) {
		t.Errorf("cached ObjectMeta = %+v, want only name, namespace and resourceVersion", s.ObjectMeta)
	}
}

// TestValue reads through the watch: a rotated value shows on the next read, and reads make
// no API call.
func TestValue(t *testing.T) {
	s := secret("sre", "creds")
	s.Data["alpha"] = []byte("alpha-value")
	client := fake.NewClientset(s)
	registered := watchRegistered(client)
	r := New(client, []string{"sre"})
	t.Cleanup(r.Stop)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-registered
	ctx := context.Background()
	calls := len(client.Actions())
	for _, tc := range []struct{ key, want string }{
		{"", canary}, // an empty key is the default, "token"
		{"token", canary},
		{"alpha", "alpha-value"},
		{"middle", canary}, // from StringData
	} {
		v, err := r.Value(ctx, "sre", "creds", tc.key)
		if err != nil {
			t.Fatalf("Value(%q): %v", tc.key, err)
		}
		if got := v.Reveal(); got != tc.want {
			t.Errorf("Value(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
	if got := len(client.Actions()); got != calls {
		t.Errorf("reads made %d API calls, want 0", got-calls)
	}

	before, err := r.Value(ctx, "sre", "creds", "")
	if err != nil {
		t.Fatal(err)
	}
	rotated := secret("sre", "creds")
	rotated.Data["token"] = []byte("rotated-value")
	if _, err := client.CoreV1().Secrets("sre").Update(ctx, rotated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, err := r.Value(ctx, "sre", "creds", "")
		if err != nil {
			t.Fatal(err)
		}
		if v.Reveal() == "rotated-value" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rotated value never reached the cache")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := before.Reveal(); got != canary {
		t.Errorf("a Value read earlier changed to %q; it must be a snapshot", got)
	}
}

// TestValueOnlyForOpaque: Courier's own tokens live in Opaque Secrets. Any other type (a Helm
// release, a service account token, TLS keys) keeps its key names for Keys, but its values
// never enter the cache and Value refuses it.
func TestValueOnlyForOpaque(t *testing.T) {
	typed := func(name string, typ corev1.SecretType) *corev1.Secret {
		s := secret("sre", name)
		s.Type = typ
		return s
	}
	r := start(t, fake.NewClientset(
		typed("opaque", corev1.SecretTypeOpaque),
		typed("tls", corev1.SecretTypeTLS),
		typed("release", "helm.sh/release.v1"),
	))
	if v, err := r.Value(context.Background(), "sre", "opaque", ""); err != nil || v.Reveal() != canary {
		t.Errorf("Opaque Value = %v, %v; want the value", v, err)
	}
	for name, typ := range map[string]string{"tls": "kubernetes.io/tls", "release": "helm.sh/release.v1"} {
		if keys, err := r.Keys(context.Background(), "sre", name); err != nil || !slices.Equal(keys, []string{"alpha", "middle", "token"}) {
			t.Errorf("%s Keys = %v, %v; want the key names", name, keys, err)
		}
		v, err := r.Value(context.Background(), "sre", name, "")
		if !errors.Is(err, secretval.ErrUnsupportedType) || !strings.Contains(err.Error(), typ) || v.Reveal() != "" {
			t.Errorf("%s Value = %v, %v; want ErrUnsupportedType naming %q", name, v, err, typ)
		}
		cached, err := r.namespaces["sre"].lister.Secrets("sre").Get(name)
		if err != nil {
			t.Fatal(err)
		}
		for k, val := range cached.Data {
			if val != nil {
				t.Errorf("%s: cached Data[%q] holds a value", name, k)
			}
		}
	}
}

func TestValueErrors(t *testing.T) {
	s := secret("sre", "creds")
	s.Data["empty"] = []byte{}
	r := start(t, fake.NewClientset(s))
	unstarted := New(fake.NewClientset(s), []string{"sre"})
	stopped := New(fake.NewClientset(s), []string{"sre"})
	if err := stopped.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	stopped.Stop()
	all := []error{config.ErrSecretNotFound, secretval.ErrKeyNotFound, secretval.ErrEmptyValue, ErrNamespaceNotWatched, ErrNotSynced, ErrStopped} // each case matches exactly one
	tests := []struct {
		name              string
		resolver          *Resolver
		namespace, secret string
		key, wantKey      string // wantKey is the key the message must name; "" defaults to "token"
		want              error
	}{
		{"missing Secret", r, "sre", "absent", "token", "token", config.ErrSecretNotFound},
		{"missing key", r, "sre", "creds", "nokey", "nokey", secretval.ErrKeyNotFound},
		{"missing Secret, default key", r, "payments", "creds", "", "token", config.ErrSecretNotFound},
		{"empty value", r, "sre", "creds", "empty", "empty", secretval.ErrEmptyValue},
		{"unwatched namespace", r, "kube-system", "creds", "token", "token", ErrNamespaceNotWatched},
		{"cache not synced", unstarted, "sre", "creds", "token", "token", ErrNotSynced},
		{"stopped", stopped, "sre", "creds", "token", "token", ErrStopped},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := tc.resolver.Value(context.Background(), tc.namespace, tc.secret, tc.key)
			if err == nil {
				t.Fatalf("Value = %v, want an error", v)
			}
			for _, e := range all {
				if got, want := errors.Is(err, e), errors.Is(tc.want, e); got != want {
					t.Errorf("errors.Is(err, %v) = %v, want %v; err = %v", e, got, want, err)
				}
			}
			msg := err.Error()
			for _, part := range []string{tc.namespace, tc.secret, strconv.Quote(tc.wantKey)} {
				if !strings.Contains(msg, part) {
					t.Errorf("error %q does not name %q", msg, part)
				}
			}
			// The Secret holds the canary; no error path may echo it.
			if strings.Contains(msg, canary) || v.Reveal() != "" {
				t.Errorf("error %q or the returned Value carries a Secret value", msg)
			}
		})
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

// denied is what the API server answers a ServiceAccount that has no Role in the namespace.
func denied() error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("RBAC: no list"))
}

// logBuffer is a log destination that the informers' goroutines and the test share.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A denied list never syncs, so waiting out the timeout would only hide the reason: Start fails
// as soon as the API server says forbidden, and the error names the namespace and the cause.
func TestStartFailsFastAndNamesAForbiddenList(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, denied() })
	r := New(client, []string{"sre"})
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()

	begin := time.Now()
	err := r.Start(ctx)

	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "forbidden") || !strings.Contains(err.Error(), `["sre"]`) {
		t.Errorf("Start = %v, want an error naming namespace sre and wrapping the forbidden cause", err)
	}
	if took := time.Since(begin); took > 2*time.Second {
		t.Errorf("Start took %s, want it to fail at the first denial, well before its 4s timeout", took)
	}
}

// A transient failure is not worth giving up on: Start waits out its timeout, and the error
// still names the cause.
func TestStartNamesATransientCauseAtTheTimeout(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	r := New(client, []string{"sre"})
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := r.Start(ctx); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Start = %v, want the deadline and the cause", err)
	}
}

// A watch that is denied leaves a cache that never updates. Whether the denial arrives before
// Start has seen the first list sync is a race, so either outcome must name the cause: Start
// fails with it, or it succeeds and the cause is in the log.
func TestDeniedWatchIsReported(t *testing.T) {
	client := fake.NewClientset(secret("sre", "creds"))
	client.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) { return true, nil, denied() })
	logs := &logBuffer{}
	r := New(client, []string{"sre"}, WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	t.Cleanup(r.Stop)
	if err := r.Start(t.Context()); err != nil && !apierrors.IsForbidden(err) {
		t.Fatalf("Start = %v, want success or the forbidden cause", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "forbidden") {
		if time.Now().After(deadline) {
			t.Fatalf("no forbidden watch in the log: %q", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "sre") || strings.Contains(logs.String(), canary) {
		t.Errorf("log %q should name namespace sre and hold no Secret value", logs.String())
	}
}
