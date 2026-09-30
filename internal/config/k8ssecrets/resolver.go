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
// Only key names are kept. The informer's transform strips each Secret to its name and key
// names before it enters the cache, so values are in memory only while a list or watch
// event is in flight, and annotations such as last-applied-configuration (which can hold a
// copy of the values) are dropped too.
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

	"github.com/agentkourier/agent-kourier/internal/config"
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
		f := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(ns), informers.WithTransform(keysOnly))
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

// Keys returns the sorted key names of the Secret from the cache. It wraps
// config.ErrSecretNotFound when the Secret does not exist, ErrNamespaceNotWatched when
// namespace is not one of the watched namespaces, ErrNotSynced while that namespace's cache
// has not synced, and ErrStopped after Stop.
func (r *Resolver) Keys(_ context.Context, namespace, name string) ([]string, error) {
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
	return slices.Sorted(maps.Keys(s.Data)), nil
}

// keysOnly is the informer transform. It reduces a Secret to its identity and its key
// names (in Data, with nil values; StringData keys are folded in). It returns anything that
// is not a Secret unchanged.
//
// It must never return an error: when the store rejects an object, client-go's reflector
// logs the untransformed object.
func keysOnly(obj any) (any, error) {
	s, ok := obj.(*corev1.Secret)
	if !ok {
		return obj, nil
	}
	out := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name, ResourceVersion: s.ResourceVersion},
		Data:       make(map[string][]byte, len(s.Data)+len(s.StringData)),
	}
	for k := range s.Data {
		out.Data[k] = nil
	}
	for k := range s.StringData {
		out.Data[k] = nil
	}
	return out, nil
}
