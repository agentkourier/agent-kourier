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
// The cache holds each Secret's name and data, values included, so that Value costs no API
// call, and nothing else: the informer's transform drops annotations such as
// last-applied-configuration, which can hold a second copy of the values. Keys returns names
// only. Value returns a secret.Value, which no formatting path can print, built from a copy
// of the bytes, so a Secret's rotation never changes a Value already read.
//
// The transform runs after client-go has decoded the response, and client-go logs response
// bodies, Secret lists included, through klog at verbosity 8 and above. klog verbosity must
// stay below 8 wherever this package runs.
package k8ssecrets

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

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
	namespaces map[string]watched
}

// New returns a Resolver that watches Secrets in namespaces through client. It does nothing
// until Start.
func New(client kubernetes.Interface, namespaces []string) *Resolver {
	r := &Resolver{namespaces: make(map[string]watched, len(namespaces))}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	for _, ns := range namespaces {
		if _, ok := r.namespaces[ns]; ok {
			continue
		}
		f := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(ns), informers.WithTransform(trim))
		secrets := f.Core().V1().Secrets()
		r.namespaces[ns] = watched{factory: f, lister: secrets.Lister(), synced: secrets.Informer().HasSynced}
	}
	return r
}

// Start runs the informers and waits until every cache has synced, so the first Keys call
// cannot report a false "not found". ctx bounds only the wait: if it ends first, Start
// returns an error naming the namespaces that have not synced, but the informers keep
// running until Stop, and Keys serves each namespace as soon as its cache syncs.
func (r *Resolver) Start(ctx context.Context) error {
	for _, w := range r.namespaces {
		w.factory.Start(r.ctx.Done())
	}
	wait, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(r.ctx, cancel)() //nolint:contextcheck // r.ctx is the informers' lifetime, deliberately not derived from Start's ctx
	var unsynced []string
	for ns, w := range r.namespaces {
		if !cache.WaitForCacheSync(wait.Done(), w.synced) {
			unsynced = append(unsynced, ns)
		}
	}
	if len(unsynced) > 0 {
		slices.Sort(unsynced)
		return fmt.Errorf("secret caches not synced for namespaces %q: %w", unsynced, wait.Err())
	}
	return nil
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
// Secret has no such key and secret.ErrEmptyValue when the key holds no bytes. Error
// messages name the namespace, Secret and key, never the value.
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
	case !ok:
		return secretval.Value{}, fmt.Errorf("secret %s/%s key %q: %w", namespace, name, key, secretval.ErrKeyNotFound)
	case len(v) == 0:
		return secretval.Value{}, fmt.Errorf("secret %s/%s key %q: %w", namespace, name, key, secretval.ErrEmptyValue)
	}
	return secretval.New(string(v)), nil
}

// trim is the informer transform. It reduces a Secret to its identity and its data, with
// StringData folded in as the API server does on write (it wins over Data for a key in both).
// It returns anything that is not a Secret unchanged.
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
		Data:       make(map[string][]byte, len(s.Data)+len(s.StringData)),
	}
	maps.Copy(out.Data, s.Data)
	for k, v := range s.StringData {
		out.Data[k] = []byte(v)
	}
	return out, nil
}
