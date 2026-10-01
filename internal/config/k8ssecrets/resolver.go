// Package k8ssecrets implements config.SecretResolver over the Kubernetes API, served from
// an informer cache so that a config reload never costs a request per reference.
//
// # RBAC
//
// Courier reads Secrets only in the namespaces it is given, never cluster-wide. The Helm
// chart grants this Role and a RoleBinding to Courier's ServiceAccount in each of them.
// The informer needs list and watch; it never issues a get.
//
//	apiVersion: rbac.authorization.k8s.io/v1
//	kind: Role
//	metadata:
//	  name: agent-courier-secrets
//	  namespace: <each watched namespace>
//	rules:
//	  - apiGroups: [""]
//	    resources: ["secrets"]
//	    verbs: ["list", "watch"]
//
// # Values
//
// The cache holds each Secret's name, type and data, and nothing else: the informer's
// transform drops annotations such as last-applied-configuration, which can hold a second
// copy of the values. Only Opaque Secrets (or an empty type) keep their values, so that
// Value costs no API call; every other type (a Helm release, a service account token, TLS
// keys, a docker config) keeps key names only, and Value refuses it. Keys returns names only.
// Value returns a secret.Value, which no formatting path can print, built from a copy of the
// bytes, so a Secret's rotation never changes a Value already read.
//
// Every Opaque value in each watched namespace stays resident for the life of the process,
// whether or not any config refers to it. That has these consequences for the deployment:
//
//   - klog verbosity is 0 and must stay so. Nothing calls klog.InitFlags, and nothing may
//     expose -v. The transform runs after client-go has decoded the response, and client-go
//     logs response bodies, Secret lists included, through klog at verbosity 8 and above.
//   - GOTRACEBACK=crash must never be set (a core dump holds the heap), and no heap-dump
//     endpoint may be added. pprof heap profiles hold allocation sites only, not contents.
//
// After the pilot, narrow the exposure to the Secrets that config refers to: one informer per
// referenced Secret, with a metadata.name field selector, and RBAC resourceNames.
package k8ssecrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	secretval "github.com/agentkourier/agent-kourier/internal/secret"
)

var (
	// ErrNamespaceNotWatched is returned for a namespace the Resolver was not given. It is
	// distinct from config.ErrSecretNotFound: the Secret may exist, but Courier may not read it.
	ErrNamespaceNotWatched = errors.New("namespace not watched")
	// ErrNotSynced is returned while a namespace's cache has not synced, when an empty cache
	// would otherwise be indistinguishable from a missing Secret.
	ErrNotSynced = errors.New("secret cache not synced")
	// ErrStopped is returned after Stop, when the caches no longer update and would serve
	// stale data.
	ErrStopped = errors.New("secret resolver stopped")
	// ErrAccessDenied is wrapped by the error Start returns when the API server answered a list or
	// watch with forbidden or unauthorized. It is a configuration failure, not a cancellation or a
	// timeout, and the error also wraps the API server's own answer.
	ErrAccessDenied = errors.New("the API server denied access to Secrets")
)

var _ config.SecretResolver = (*Resolver)(nil)

// watched is the informer for one namespace.
type watched struct {
	factory informers.SharedInformerFactory
	lister  corelisters.SecretLister
	synced  cache.InformerSynced
}

// Resolver is a config.SecretResolver backed by one Secret informer per watched namespace.
type Resolver struct {
	// ctx bounds the informers' lifetime. Only Stop ends it, so that a caller's timeout on
	// Start cannot silently freeze the caches.
	ctx        context.Context
	cancel     context.CancelFunc
	log        *slog.Logger
	namespaces map[string]watched

	mu     sync.Mutex
	causes map[string]error // the latest list or watch failure of each namespace
	denied chan struct{}    // closed at the first forbidden or unauthorized answer
	once   sync.Once
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithLogger sets where a failed list or watch is reported. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(r *Resolver) { r.log = l } }

// New returns a Resolver that watches Secrets in namespaces through client. It does nothing
// until Start.
func New(client kubernetes.Interface, namespaces []string, opts ...Option) *Resolver {
	r := &Resolver{
		log: slog.Default(), namespaces: make(map[string]watched, len(namespaces)),
		causes: map[string]error{}, denied: make(chan struct{}),
	}
	for _, o := range opts {
		o(r)
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	for _, ns := range namespaces {
		if _, ok := r.namespaces[ns]; ok {
			continue
		}
		f := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(ns), informers.WithTransform(trim))
		secrets := f.Core().V1().Secrets()
		// The handler replaces client-go's, which logs through klog. The only error is an
		// informer that has started, and this one has not.
		_ = secrets.Informer().SetWatchErrorHandler(func(_ *cache.Reflector, err error) { r.failed(ns, err) })
		r.namespaces[ns] = watched{factory: f, lister: secrets.Lister(), synced: secrets.Informer().HasSynced}
	}
	return r
}

// failed records why a namespace's list or watch failed and reports it. A forbidden or
// unauthorized answer will not pass by waiting, so it also ends Start's wait.
func (r *Resolver) failed(namespace string, err error) {
	r.mu.Lock()
	r.causes[namespace] = err
	r.mu.Unlock()
	r.log.Warn("secret informer cannot list or watch", "namespace", namespace, "error", err)
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		r.once.Do(func() { close(r.denied) })
	}
}

// Start runs the informers and waits until every cache has synced, so the first Keys call
// cannot report a false "not found". ctx bounds only the wait: if it ends first, Start
// returns an error naming the namespaces that have not synced, but the informers keep
// running until Stop, and Keys serves each namespace as soon as its cache syncs.
func (r *Resolver) Start(ctx context.Context) error {
	for _, w := range r.namespaces {
		w.factory.Start(r.ctx.Done())
	}
	wait, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer context.AfterFunc(r.ctx, func() { cancel(nil) })() //nolint:contextcheck // r.ctx is the informers' lifetime, deliberately not derived from Start's ctx
	go func() {
		select {
		case <-r.denied:
			cancel(ErrAccessDenied) // so the error is not a Canceled, which a caller may read as a shutdown
		case <-wait.Done():
		}
	}()
	var unsynced []string
	for ns, w := range r.namespaces {
		if !cache.WaitForCacheSync(wait.Done(), w.synced) {
			unsynced = append(unsynced, ns)
		}
	}
	if len(unsynced) > 0 {
		slices.Sort(unsynced)
		err := context.Cause(wait) // the timeout, the caller's cancellation, or ErrAccessDenied
		if causes := r.causesOf(unsynced); len(causes) > 0 {
			err = fmt.Errorf("%w: %w", err, errors.Join(causes...))
		}
		return fmt.Errorf("secret caches not synced for namespaces %q: %w", unsynced, err)
	}
	return nil
}

// causesOf returns the latest failure of each of namespaces that has one, each naming its namespace.
func (r *Resolver) causesOf(namespaces []string) []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []error
	for _, ns := range namespaces {
		if err := r.causes[ns]; err != nil {
			out = append(out, fmt.Errorf("namespace %s: %w", ns, err))
		}
	}
	return out
}

// Stop ends the informers and waits for their goroutines to exit. Keys returns ErrStopped
// afterwards.
func (r *Resolver) Stop() {
	r.cancel()
	for _, w := range r.namespaces {
		w.factory.Shutdown()
	}
}

// get returns the cached Secret. It wraps config.ErrSecretNotFound when the Secret does not
// exist, ErrNamespaceNotWatched when namespace is not one of the watched namespaces,
// ErrNotSynced while that namespace's cache has not synced, and ErrStopped after Stop.
func (r *Resolver) get(namespace, name string) (*corev1.Secret, error) {
	w, ok := r.namespaces[namespace]
	switch {
	case !ok:
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, ErrNamespaceNotWatched)
	case r.ctx.Err() != nil:
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, ErrStopped)
	case !w.synced():
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, ErrNotSynced)
	}
	s, err := w.lister.Secrets(namespace).Get(name)
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, config.ErrSecretNotFound)
	case err != nil:
		return nil, fmt.Errorf("read secret %s/%s from cache: %w", namespace, name, err)
	}
	return s, nil
}

// Keys returns the sorted key names of the Secret from the cache. It fails as get does.
func (r *Resolver) Keys(_ context.Context, namespace, name string) ([]string, error) {
	s, err := r.get(namespace, name)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(s.Data)), nil
}

// Value returns the value of key in the Secret from the cache; an empty key means
// v1alpha1.DefaultSecretKey. It fails as get does, and wraps secret.ErrKeyNotFound when the
// Secret has no such key, secret.ErrEmptyValue when the key holds no bytes, and
// secret.ErrUnsupportedType when the Secret is not Opaque. Error messages name the namespace,
// Secret and key (and the type), never the value.
func (r *Resolver) Value(_ context.Context, namespace, name, key string) (secretval.Value, error) {
	if key == "" {
		key = v1alpha1.DefaultSecretKey
	}
	s, err := r.get(namespace, name)
	if err != nil {
		return secretval.Value{}, fmt.Errorf("read key %q: %w", key, err)
	}
	v, ok := s.Data[key]
	switch {
	case !opaque(s.Type):
		return secretval.Value{}, fmt.Errorf("secret %s/%s key %q of type %q: %w", namespace, name, key, s.Type, secretval.ErrUnsupportedType)
	case !ok:
		return secretval.Value{}, fmt.Errorf("secret %s/%s key %q: %w", namespace, name, key, secretval.ErrKeyNotFound)
	case len(v) == 0:
		return secretval.Value{}, fmt.Errorf("secret %s/%s key %q: %w", namespace, name, key, secretval.ErrEmptyValue)
	}
	return secretval.New(string(v)), nil
}

// opaque reports whether a Secret of type t may have its values read.
func opaque(t corev1.SecretType) bool { return t == "" || t == corev1.SecretTypeOpaque }

// trim is the informer transform. It reduces a Secret to its identity, its type and its data,
// with StringData folded in as the API server does on write (it wins over Data for a key in
// both). Only an Opaque Secret keeps its values; any other type keeps key names, with nil
// values. It returns anything that is not a Secret unchanged.
//
// It must never return an error: when the store rejects an object, client-go's reflector
// logs the untransformed object.
func trim(obj any) (any, error) {
	s, ok := obj.(*corev1.Secret)
	if !ok {
		return obj, nil
	}
	out := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name, ResourceVersion: s.ResourceVersion},
		Type:       s.Type,
		Data:       make(map[string][]byte, len(s.Data)+len(s.StringData)),
	}
	maps.Copy(out.Data, s.Data)
	for k, v := range s.StringData {
		out.Data[k] = []byte(v)
	}
	if !opaque(s.Type) {
		for k := range out.Data {
			out.Data[k] = nil
		}
	}
	return out, nil
}
