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
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
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

// sn is a Secret's name.
func sn(namespace, name string) config.SecretName {
	return config.SecretName{Namespace: namespace, Name: name}
}

// api is a fake clientset that answers a Secrets list or watch as the API server answers one with a metadata.name
// field selector (the fake tracker ignores field selectors), and fails the test for a request that has none, or that has
// more than the one name. It counts the requests of each Secret.
type api struct {
	*fake.Clientset

	mu      sync.Mutex
	lists   map[config.SecretName]int
	watches map[config.SecretName]int
}

var secretKind = corev1.SchemeGroupVersion.WithKind("Secret")

// selected returns the Secret that a list or watch asks for, and fails the test if it asks for anything else.
func (a *api) selected(t *testing.T, act k8stesting.Action, sel fields.Selector) (config.SecretName, bool) {
	t.Helper()
	name, ok := sel.RequiresExactMatch("metadata.name")
	if !ok || sel.String() != "metadata.name="+name {
		t.Errorf("a Secrets %s in namespace %q had field selector %q; want exactly one metadata.name", act.GetVerb(), act.GetNamespace(), sel)
		return config.SecretName{}, false
	}
	return sn(act.GetNamespace(), name), true
}

func newAPI(t *testing.T, objects ...runtime.Object) *api {
	t.Helper()
	a := &api{Clientset: fake.NewClientset(objects...), lists: map[config.SecretName]int{}, watches: map[config.SecretName]int{}}
	a.PrependReactor("list", "secrets", func(act k8stesting.Action) (bool, runtime.Object, error) {
		n, ok := a.selected(t, act, act.(k8stesting.ListAction).GetListRestrictions().Fields)
		if !ok {
			return false, nil, nil
		}
		a.mu.Lock()
		a.lists[n]++
		a.mu.Unlock()
		all, err := a.Tracker().List(act.GetResource(), secretKind, act.GetNamespace())
		if err != nil {
			return true, nil, err
		}
		out := &corev1.SecretList{}
		for _, s := range all.(*corev1.SecretList).Items {
			if s.Name == n.Name {
				out.Items = append(out.Items, s)
			}
		}
		return true, out, nil
	})
	a.PrependWatchReactor("secrets", func(act k8stesting.Action) (bool, watch.Interface, error) {
		n, ok := a.selected(t, act, act.(k8stesting.WatchAction).GetWatchRestrictions().Fields)
		if !ok {
			return false, nil, nil
		}
		w, err := a.Tracker().Watch(act.GetResource(), act.GetNamespace())
		if err != nil {
			return true, nil, err
		}
		a.mu.Lock()
		a.watches[n]++
		a.mu.Unlock()
		return true, watch.Filter(w, func(e watch.Event) (watch.Event, bool) {
			o, isObj := e.Object.(metav1.Object)
			return e, isObj && o.GetName() == n.Name
		}), nil
	})
	return a
}

// count returns how many lists and watches of n the API has answered.
func (a *api) count(n config.SecretName) (lists, watches int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lists[n], a.watches[n]
}

// waitFor polls until done holds, or fails the test after 10 s.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !done(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened", what)
		}
	}
}

// watching waits until the API has a watch for each of names. The fake tracker drops events for a watch that does not
// exist yet, and a Watch returns once the list has synced.
func (a *api) watching(t *testing.T, names ...config.SecretName) {
	t.Helper()
	for _, n := range names {
		waitFor(t, "the watch of "+n.String(), func() bool { _, w := a.count(n); return w > 0 })
	}
}

// follow returns a Resolver that follows names, synced, and stopped at cleanup.
func follow(t *testing.T, client *api, names ...config.SecretName) *Resolver {
	t.Helper()
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	if err := r.Watch(t.Context(), names); err != nil {
		t.Fatal(err)
	}
	return r
}

// unsynced returns a Resolver that follows sre/creds whose list fails every time, so that the Secret never syncs.
func unsynced(t *testing.T) *Resolver {
	t.Helper()
	client := fake.NewClientset(secret("sre", "creds"))
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	r.wait = 50 * time.Millisecond // a Value waits this long for the cache that never syncs
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := r.Watch(ctx, []config.SecretName{sn("sre", "creds")}); err == nil {
		t.Fatal("a Secret whose list fails synced")
	}
	return r
}

func TestKeys(t *testing.T) {
	r := follow(t, newAPI(t, secret("sre", "creds"), secret("payments", "creds")), sn("sre", "creds"), sn("payments", "creds"))
	got, err := r.Keys(context.Background(), "sre", "creds")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "middle", "token"}; !slices.Equal(got, want) {
		t.Errorf("Keys = %v, want %v (sorted, Data and StringData)", got, want)
	}
}

func TestKeysErrors(t *testing.T) {
	r := follow(t, newAPI(t, secret("sre", "creds"), secret("sre", "other"), secret("kube-system", "creds")), sn("sre", "creds"), sn("sre", "absent"))
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
		{"a namespace that is not followed", r, "kube-system", "creds", ErrNotWatched, config.ErrSecretNotFound},
		{"cache not synced", unsynced(t), "sre", "creds", ErrNotSynced, config.ErrSecretNotFound},
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

// Each followed Secret has an informer of its own, and its list and watch ask for that Secret by name: the cache of a
// Secret holds that Secret alone, even when the namespace has others, which are neither read nor kept.
func TestEachSecretIsFollowedByName(t *testing.T) {
	a := newAPI(t, secret("sre", "creds"), secret("sre", "other"), secret("sre", "third"))
	r := follow(t, a, sn("sre", "creds"), sn("sre", "third"))
	a.watching(t, sn("sre", "creds"), sn("sre", "third"))

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) != 2 {
		t.Fatalf("%d informers, want one per followed Secret (2)", len(r.entries))
	}
	for n, e := range r.entries {
		cached, err := e.lister.Secrets(n.Namespace).List(labels.Everything())
		if err != nil || len(cached) != 1 || cached[0].Name != n.Name {
			t.Errorf("the cache of %s holds %d Secrets (%v), want that Secret alone", n, len(cached), err)
		}
	}
	if lists, _ := a.count(sn("sre", "other")); lists != 0 {
		t.Errorf("the Secret that is not followed was listed %d times", lists)
	}
}

// A relist, which a watch that has expired makes the informer do, keeps the selector, or it would list the namespace.
func TestRelistKeepsTheNameSelector(t *testing.T) {
	a := newAPI(t, secret("sre", "creds"), secret("sre", "other"))
	var first atomic.Bool
	a.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) {
		if first.CompareAndSwap(false, true) {
			return true, nil, apierrors.NewResourceExpired("too old resource version") // newAPI's reactor checks the selector of the others
		}
		return false, nil, nil
	})
	r := follow(t, a, sn("sre", "creds"))

	waitFor(t, "the relist", func() bool { lists, _ := a.count(sn("sre", "creds")); return lists >= 2 })
	// api.selected fails the test for a request without the selector, so the relist was checked as it was made.
	if _, err := r.Keys(t.Context(), "sre", "creds"); err != nil {
		t.Errorf("Keys after the relist: %v", err)
	}
	if _, err := r.Keys(t.Context(), "sre", "other"); !errors.Is(err, ErrNotWatched) {
		t.Errorf("Keys of a Secret that is not followed = %v, want ErrNotWatched", err)
	}
}

// TestCacheKeepsOnlyIdentityAndData: the values are cached for Value, but nothing else of the
// Secret is, least of all the annotation that can hold a copy of them.
func TestCacheKeepsOnlyIdentityAndData(t *testing.T) {
	r := follow(t, newAPI(t, secret("sre", "creds")), sn("sre", "creds"))
	s, err := r.entries[sn("sre", "creds")].lister.Secrets("sre").Get("creds")
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
	client := newAPI(t, s)
	r := follow(t, client, sn("sre", "creds"))
	client.watching(t, sn("sre", "creds"))
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
	waitFor(t, "the rotated value in the cache", func() bool {
		v, err := r.Value(ctx, "sre", "creds", "")
		return err == nil && v.Reveal() == "rotated-value"
	})
	if got := before.Reveal(); got != canary {
		t.Errorf("a Value read earlier changed to %q; it must be a snapshot", got)
	}
}

// TestValueOnlyForOpaque: Agent Kourier's own tokens live in Opaque Secrets. Any other type (a Helm
// release, a service account token, TLS keys) keeps its key names for Keys, but its values
// never enter the cache and Value refuses it.
func TestValueOnlyForOpaque(t *testing.T) {
	typed := func(name string, typ corev1.SecretType) *corev1.Secret {
		s := secret("sre", name)
		s.Type = typ
		return s
	}
	r := follow(t, newAPI(t,
		typed("opaque", corev1.SecretTypeOpaque),
		typed("tls", corev1.SecretTypeTLS),
		typed("release", "helm.sh/release.v1"),
	), sn("sre", "opaque"), sn("sre", "tls"), sn("sre", "release"))
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
		cached, err := r.entries[sn("sre", name)].lister.Secrets("sre").Get(name)
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
	r := follow(t, newAPI(t, s), sn("sre", "creds"), sn("payments", "creds"))
	stopped := follow(t, newAPI(t, s), sn("sre", "creds"))
	stopped.Stop()
	all := []error{config.ErrSecretNotFound, secretval.ErrKeyNotFound, secretval.ErrEmptyValue, ErrNotWatched, ErrNotSynced, ErrStopped} // each case matches exactly one
	tests := []struct {
		name              string
		resolver          *Resolver
		namespace, secret string
		key, wantKey      string // wantKey is the key the message must name; "" defaults to "token"
		want              error
	}{
		{"missing Secret", r, "payments", "creds", "token", "token", config.ErrSecretNotFound},
		{"missing key", r, "sre", "creds", "nokey", "nokey", secretval.ErrKeyNotFound},
		{"missing Secret, default key", r, "payments", "creds", "", "token", config.ErrSecretNotFound},
		{"empty value", r, "sre", "creds", "empty", "empty", secretval.ErrEmptyValue},
		{"not followed, so followed now, and not there", r, "kube-system", "creds", "token", "token", config.ErrSecretNotFound},
		{"cache not synced", unsynced(t), "sre", "creds", "token", "token", ErrNotSynced},
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

// TestPicksUpSecretCreatedAfterWatch also proves the informers outlive the context passed to
// Watch, which a caller bounds with a timeout.
func TestPicksUpSecretCreatedAfterWatch(t *testing.T) {
	client := newAPI(t)
	r := New(client)
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithCancel(t.Context())
	err := r.Watch(ctx, []config.SecretName{sn("sre", "late")})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	client.watching(t, sn("sre", "late"))
	if _, err := r.Keys(context.Background(), "sre", "late"); !errors.Is(err, config.ErrSecretNotFound) {
		t.Fatalf("before create: err = %v", err)
	}
	if _, err := client.CoreV1().Secrets("sre").Create(context.Background(), secret("sre", "late"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the Secret in the cache", func() bool {
		keys, err := r.Keys(context.Background(), "sre", "late")
		return err == nil && len(keys) == 3
	})
}

// Asking again for a Secret that is followed starts nothing.
func TestWatchOfAFollowedSecretStartsNoSecondInformer(t *testing.T) {
	client := newAPI(t, secret("sre", "creds"))
	r := follow(t, client, sn("sre", "creds"))
	for range 3 {
		if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "creds")}); err != nil {
			t.Fatal(err)
		}
	}
	if lists, _ := client.count(sn("sre", "creds")); lists != 1 {
		t.Errorf("sre/creds was listed %d times, want once", lists)
	}
}

// A reload that refers to a Secret in a namespace nothing has read before is served by a new informer, with no restart.
func TestWatchAddsASecretInANewNamespace(t *testing.T) {
	client := newAPI(t, secret("sre", "creds"), secret("payments", "token"))
	r := follow(t, client, sn("sre", "creds"))
	if _, err := r.Keys(t.Context(), "payments", "token"); !errors.Is(err, ErrNotWatched) {
		t.Fatalf("before: Keys = %v, want ErrNotWatched", err)
	}
	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "creds"), sn("payments", "token")}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Keys(t.Context(), "payments", "token"); err != nil {
		t.Errorf("after: Keys = %v", err)
	}
}

// Retain lets go of the Secrets that a published snapshot no longer refers to: they are no longer served, and their
// informers end, each of them, whether it had synced or not.
func TestRetainStopsTheInformersOfSecretsThatAreGone(t *testing.T) {
	before := goleak.IgnoreCurrent()
	client := newAPI(t, secret("sre", "a"), secret("sre", "b"))
	r := follow(t, client, sn("sre", "a"), sn("sre", "b"))

	r.Retain([]config.SecretName{sn("sre", "b"), sn("sre", "never-followed")})
	if _, err := r.Keys(t.Context(), "sre", "a"); !errors.Is(err, ErrNotWatched) {
		t.Errorf("Keys of a Secret that is no longer retained = %v, want ErrNotWatched", err)
	}
	if _, err := r.Keys(t.Context(), "sre", "b"); err != nil {
		t.Errorf("Keys of a retained Secret = %v", err)
	}
	if _, err := r.Keys(t.Context(), "sre", "never-followed"); !errors.Is(err, ErrNotWatched) {
		t.Errorf("Retain started an informer it was not asked to: Keys = %v", err)
	}

	r.Retain(nil)
	goleak.VerifyNone(t, before) // the resolver is still running, and nothing of it is
}

func TestRetainStopsAnInformerThatNeverSynced(t *testing.T) {
	before := goleak.IgnoreCurrent()
	r := unsynced(t)
	r.Retain(nil)
	goleak.VerifyNone(t, before)
}

// The pinned Secrets are the ones Agent Kourier reads for itself, such as the Postgres DSN's, which no config names: the
// config's Retain must not end them.
func TestPinnedSecretsOutliveRetain(t *testing.T) {
	client := newAPI(t, secret("agent-kourier", "pg"), secret("sre", "creds"))
	r := New(client, WithPinned(sn("agent-kourier", "pg")))
	t.Cleanup(r.Stop)
	// Readable as soon as Start returns, before any config has named a Secret.
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Value(t.Context(), "agent-kourier", "pg", "token"); err != nil {
		t.Fatalf("Value of the pinned Secret after Start: %v", err)
	}
	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "creds")}); err != nil {
		t.Fatal(err)
	}

	r.Retain(nil)

	if _, err := r.Value(t.Context(), "agent-kourier", "pg", "token"); err != nil {
		t.Errorf("Value of the pinned Secret after Retain: %v", err)
	}
	if _, err := r.Keys(t.Context(), "sre", "creds"); !errors.Is(err, ErrNotWatched) {
		t.Errorf("Keys of the config's Secret after Retain(nil) = %v, want ErrNotWatched", err)
	}
	r.Stop()
	if _, err := r.Value(t.Context(), "agent-kourier", "pg", "token"); !errors.Is(err, ErrStopped) {
		t.Errorf("Value after Stop = %v, want ErrStopped", err)
	}
}

func TestReadsAfterStopFail(t *testing.T) {
	r := follow(t, newAPI(t, secret("sre", "creds")), sn("sre", "creds"))
	r.Stop()
	r.Stop() // twice is as good as once
	if _, err := r.Keys(context.Background(), "sre", "creds"); !errors.Is(err, ErrStopped) {
		t.Errorf("Keys after Stop = %v, want ErrStopped", err)
	}
	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "creds")}); !errors.Is(err, ErrStopped) {
		t.Errorf("Watch after Stop = %v, want ErrStopped", err)
	}
	if err := r.Start(t.Context()); !errors.Is(err, ErrStopped) {
		t.Errorf("Start after Stop = %v, want ErrStopped", err)
	}
}

// Ready is about the Secrets that the broker depends on: not the ones that a reload is still waiting for.
func TestReady(t *testing.T) {
	client := newAPI(t, secret("agent-kourier", "pg"), secret("sre", "creds"), secret("sre", "stuck"))
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.ListAction).GetListRestrictions().Fields.String() == "metadata.name=stuck" {
			return true, nil, errors.New("connection refused")
		}
		return false, nil, nil
	})
	r := New(client, WithPinned(sn("agent-kourier", "pg")), WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	if err := r.Ready(); !errors.Is(err, ErrNotSynced) {
		t.Errorf("Ready before Start = %v, want ErrNotSynced", err)
	}
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.Ready(); err != nil {
		t.Errorf("Ready after Start = %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := r.Watch(ctx, []config.SecretName{sn("sre", "creds"), sn("sre", "stuck")}); err == nil {
		t.Fatal("a Secret whose list fails synced")
	}
	if err := r.Ready(); err != nil {
		t.Errorf("Ready while a reload waits for sre/stuck = %v; a Secret no snapshot refers to must not count", err)
	}

	r.Retain([]config.SecretName{sn("sre", "creds"), sn("sre", "stuck")}) // as if a snapshot referred to it
	if err := r.Ready(); !errors.Is(err, ErrNotSynced) || !strings.Contains(err.Error(), "sre/stuck") {
		t.Errorf("Ready with a retained Secret that is not synced = %v, want ErrNotSynced naming it", err)
	}
	r.Retain(nil)
	if err := r.Ready(); err != nil {
		t.Errorf("Ready after Retain(nil) = %v", err)
	}
	r.Stop()
	if err := r.Ready(); !errors.Is(err, ErrStopped) {
		t.Errorf("Ready after Stop = %v, want ErrStopped", err)
	}
}

// A retained Secret that the API server denies, and that never synced, does not make the broker unready: with the custom
// resource source it is a Binding's, and that Binding says its credentials are unreadable, where the rest keep serving. A
// pinned one still does, and so does one that is merely not synced.
func TestReadyIgnoresARetainedSecretThatIsDenied(t *testing.T) {
	client := newAPI(t, secret("agent-kourier", "pg"), secret("sre", "creds"), secret("sre", "private"), secret("sre", "slow"))
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		switch a.(k8stesting.ListAction).GetListRestrictions().Fields.String() {
		case "metadata.name=private":
			return true, nil, denied()
		case "metadata.name=slow":
			return true, nil, errors.New("connection refused")
		}
		return false, nil, nil
	})
	r := New(client, WithPinned(sn("agent-kourier", "pg")), WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	names := []config.SecretName{sn("sre", "creds"), sn("sre", "private")}
	if err := r.Watch(t.Context(), names); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Watch = %v, want ErrAccessDenied", err)
	}
	r.Retain(names)
	if err := r.Ready(); err != nil {
		t.Errorf("Ready with a retained Secret that is denied = %v, want nil", err)
	}
	if _, err := r.Keys(t.Context(), "sre", "private"); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("Keys of the denied Secret = %v; it must still say it is denied", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_ = r.Watch(ctx, []config.SecretName{sn("sre", "slow")})
	r.Retain([]config.SecretName{sn("sre", "private"), sn("sre", "slow")})
	if err := r.Ready(); !errors.Is(err, ErrNotSynced) || !strings.Contains(err.Error(), "sre/slow") || strings.Contains(err.Error(), "sre/private") {
		t.Errorf("Ready with a retained Secret that is only slow = %v, want ErrNotSynced naming sre/slow alone", err)
	}
}

// A pinned Secret that the API server denies still makes the broker unready, where a retained one does not: it is Agent
// Kourier's own (the Postgres DSN), and nothing reports it for anyone else.
func TestReadyCountsAPinnedSecretThatIsDenied(t *testing.T) {
	client := newAPI(t, secret("agent-kourier", "pg"))
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, denied() })
	r := New(client, WithPinned(sn("agent-kourier", "pg")), WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	if err := r.Start(t.Context()); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Start = %v, want ErrAccessDenied", err)
	}
	if err := r.Ready(); !errors.Is(err, ErrNotSynced) || !strings.Contains(err.Error(), "agent-kourier/pg") {
		t.Errorf("Ready with a pinned Secret that is denied = %v, want ErrNotSynced naming it", err)
	}
}

// A Secret that does not sync in time is named, with the cause, and stays followed: the next Watch finds it synced once
// the cause passes.
func TestWatchNamesTheUnsyncedSecretAndItsCause(t *testing.T) {
	client := newAPI(t, secret("sre", "creds"), secret("sre", "stuck"))
	var failing atomic.Bool
	failing.Store(true)
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if failing.Load() && a.(k8stesting.ListAction).GetListRestrictions().Fields.String() == "metadata.name=stuck" {
			return true, nil, errors.New("connection refused")
		}
		return false, nil, nil
	})
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := r.Watch(ctx, []config.SecretName{sn("sre", "creds"), sn("sre", "stuck")})

	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrAccessDenied) {
		t.Errorf("Watch = %v, want the deadline and not a denial", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "[sre/stuck]") || strings.Contains(msg, "sre/creds") || !strings.Contains(msg, "connection refused") {
		t.Errorf("Watch = %q, want it to name sre/stuck alone, and the cause", msg)
	}
	if _, err := r.Keys(t.Context(), "sre", "creds"); err != nil {
		t.Errorf("the Secret that synced: %v", err)
	}
	if _, err := r.Keys(t.Context(), "sre", "stuck"); !errors.Is(err, ErrNotSynced) {
		t.Errorf("the Secret that did not: Keys = %v, want ErrNotSynced", err)
	}

	failing.Store(false)
	if err := r.Watch(t.Context(), []config.SecretName{sn("sre", "creds"), sn("sre", "stuck")}); err != nil {
		t.Errorf("Watch once the cause passed = %v", err)
	}
}

// TestStopUnblocksWatch is the shutdown case: Watch waits on a Secret that never syncs, with a context that never ends, and
// Stop must still release it, and the informers must be gone.
func TestStopUnblocksWatch(t *testing.T) {
	before := goleak.IgnoreCurrent()
	client := fake.NewClientset()
	listed := make(chan struct{}, 1)
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		select {
		case listed <- struct{}{}:
		default:
		}
		return true, nil, errors.New("connection refused")
	})
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	waiting := make(chan error, 1)
	go func() { waiting <- r.Watch(context.Background(), []config.SecretName{sn("payments", "creds")}) }()
	<-listed // the wait is under way

	r.Stop()

	select {
	case err := <-waiting:
		if !errors.Is(err, ErrStopped) {
			t.Errorf("Watch = %v, want ErrStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch still blocked after Stop")
	}
	goleak.VerifyNone(t, before)
}

// denied is what the API server answers a ServiceAccount that has no Role for the Secret.
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

// A denied list never syncs, so waiting out the timeout would only hide the reason: Start and Watch fail as soon as the API
// server says forbidden, and the error names the Secret and the cause.
func TestWaitFailsFastAndNamesADeniedSecret(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		is    func(error) bool
		words string
	}{
		{"forbidden", denied(), apierrors.IsForbidden, "forbidden"},
		{"unauthorized", apierrors.NewUnauthorized("token expired"), apierrors.IsUnauthorized, "token expired"},
	} {
		for _, via := range []string{"Start", "Watch"} {
			t.Run(tc.name+" "+via, func(t *testing.T) {
				client := fake.NewClientset()
				client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, tc.err })
				r := New(client, WithPinned(sn("sre", "creds")), WithLogger(slog.New(slog.DiscardHandler)))
				t.Cleanup(r.Stop)
				ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
				defer cancel()

				begin := time.Now()
				var err error
				if via == "Start" {
					err = r.Start(ctx)
				} else {
					err = r.Watch(ctx, []config.SecretName{sn("sre", "creds")})
				}

				if !tc.is(err) || !strings.Contains(err.Error(), tc.words) || !strings.Contains(err.Error(), "[sre/creds]") {
					t.Errorf("%s = %v, want an error naming sre/creds and wrapping the cause", via, err)
				}
				if took := time.Since(begin); took > 2*time.Second {
					t.Errorf("%s took %s, want it to fail at the first denial, well before its 4s timeout", via, took)
				}
				// A denial must not look like a shutdown or a timeout to a caller that decides its exit from it.
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrAccessDenied) {
					t.Errorf("%s = %v; want ErrAccessDenied, and neither Canceled nor DeadlineExceeded", via, err)
				}
			})
		}
	}
}

// Forbidden is reported for the Secret it was said of: the others are read as usual, and Keys of the denied one says
// forbidden, not that its cache has yet to sync, so that the caller can tell a denial from a wait.
func TestDenialIsPerSecret(t *testing.T) {
	client := newAPI(t, secret("sre", "creds"), secret("sre", "private"))
	client.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.ListAction).GetListRestrictions().Fields.String() == "metadata.name=private" {
			return true, nil, denied()
		}
		return false, nil, nil
	})
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)

	err := r.Watch(t.Context(), []config.SecretName{sn("sre", "creds"), sn("sre", "private")})

	if !errors.Is(err, ErrAccessDenied) || !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "sre/private") {
		t.Errorf("Watch = %v, want ErrAccessDenied wrapping the answer and naming sre/private", err)
	}
	if _, err := r.Keys(t.Context(), "sre", "creds"); err != nil {
		t.Errorf("a Secret that was not denied: %v", err)
	}
	_, err = r.Keys(t.Context(), "sre", "private")
	if !errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrNotSynced) || !apierrors.IsForbidden(err) {
		t.Errorf("Keys of the denied Secret = %v, want ErrAccessDenied wrapping the answer, and not ErrNotSynced", err)
	}
	if _, err := r.SecretVersion(t.Context(), "sre", "private"); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("SecretVersion of the denied Secret = %v, want ErrAccessDenied", err)
	}
	if _, err := r.Value(t.Context(), "sre", "private", ""); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("Value of the denied Secret = %v, want ErrAccessDenied", err)
	}
}

// A transient failure is not worth giving up on: Start waits out its timeout, and the error
// still names the cause.
func TestStartNamesATransientCauseAtTheTimeout(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	r := New(client, WithPinned(sn("sre", "creds")), WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := r.Start(ctx); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Start = %v, want the deadline and the cause", err)
	}
}

// A watch that is denied after the list was allowed is not fatal: the cache has synced, though it
// will not update, so Start succeeds and the denial is reported in the log. The watch is held
// until Start has returned, so the order is fixed.
func TestDeniedWatchIsReported(t *testing.T) {
	client := newAPI(t, secret("sre", "creds"))
	started := make(chan struct{})
	client.PrependWatchReactor("secrets", func(k8stesting.Action) (bool, watch.Interface, error) {
		<-started
		return true, nil, denied()
	})
	logs := &logBuffer{}
	r := New(client, WithPinned(sn("sre", "creds")), WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(started) }); r.Stop() })

	err := r.Start(t.Context())
	release.Do(func() { close(started) })

	if err != nil {
		t.Fatalf("Start = %v; a denied watch after an allowed list must not fail it", err)
	}
	waitFor(t, "the forbidden watch in the log", func() bool { return strings.Contains(logs.String(), "forbidden") })
	if !strings.Contains(logs.String(), "sre") || !strings.Contains(logs.String(), "creds") || strings.Contains(logs.String(), canary) {
		t.Errorf("log %q should name Secret sre/creds and hold no Secret value", logs.String())
	}
	if _, err := r.Keys(t.Context(), "sre", "creds"); err != nil {
		t.Errorf("the synced cache is still served: %v", err)
	}
}
