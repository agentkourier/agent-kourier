// Package k8ssecrets implements config.SecretResolver over the Kubernetes API, served from
// informer caches so that a config reload never costs a request per reference.
//
// # What is followed
//
// Each Secret that the config refers to has an informer of its own, whose list and watch carry a
// metadata.name field selector, so the API server sends that one Secret and nothing else. The
// Reloader asks for the Secrets of each config it loads (Watch) and lets go of the ones that a
// published snapshot no longer refers to (Retain), so a reload that adds a Secret, in any
// namespace that Agent Kourier may read, needs no restart. Pinned Secrets (WithPinned), which
// belong to Agent Kourier and not to a resource of the config, such as the Postgres DSN's, stay
// followed until Stop.
//
// A turn holds the Binding of the snapshot it began under, for as long as it runs, and reads that
// Binding's Secrets through Value. A reload that drops one of them from the config does not cut
// the turn off: Value follows a Secret that is not followed, waits for its cache (at most
// the on-demand wait, 10 s) and reads it, and a Value that finds
// the cache of a Secret that another read is waiting for waits for it too. That informer lasts
// until the next Watch that does not name the Secret, or the next Retain that does not, so a
// holder that still reads it later follows it again. This needs a live API: if the Secret was
// dropped and the Role is then revoked, or the API server is out, a turn that still reads it gets
// an error, where an informer per namespace would have served its cache. Keys and SecretVersion
// do not follow: they answer ErrNotWatched and make no API call, because the Reloader asks only
// about Secrets it has just named.
//
// # RBAC
//
// The informer needs list and watch on a Secret; it never issues a get. Both are authorized by a
// Role on the whole namespace, which is what the Helm chart grants in each namespace it is told
// of, and by a Role that names only the Secrets it may read:
//
//	apiVersion: rbac.authorization.k8s.io/v1
//	kind: Role
//	metadata:
//	  name: agent-kourier-secrets
//	  namespace: <the Secret's namespace>
//	rules:
//	  - apiGroups: [""]
//	    resources: ["secrets"]
//	    resourceNames: ["<the Secret's name>"] # leave out to allow every Secret of the namespace
//	    verbs: ["list", "watch"]
//
// A rule with resourceNames covers a list or watch only because the informer's requests carry the
// field selector. An answer of forbidden for one Secret ends the wait for that Secret (see
// ErrAccessDenied) and is reported for that Secret alone; what it means is the caller's to decide.
//
// # Values
//
// The cache holds each Secret's name, type, data and resourceVersion (which SecretVersion
// returns), and nothing else: the informer's transform drops annotations such as
// last-applied-configuration, which can hold a second copy of the values. Only Opaque
// Secrets (or an empty type) keep their values, so that Value of a followed Secret costs no API
// call; every other type (a Helm release, a service account token, TLS keys, a docker config) keeps key names
// only, and Value refuses it. Keys returns names only.
// Value returns a secret.Value, which no formatting path can print, built from a copy of the
// bytes, so a Secret's rotation never changes a Value already read.
//
// The Opaque values of the followed Secrets stay resident for the life of the process, or until
// the Secret is no longer followed. That has these consequences for the deployment:
//
//   - klog verbosity is 0 and must stay so. Nothing calls klog.InitFlags, and nothing may
//     expose -v. The transform runs after client-go has decoded the response, and client-go
//     logs response bodies, Secret lists included, through klog at verbosity 8 and above.
//   - GOTRACEBACK=crash must never be set (a core dump holds the heap), and no heap-dump
//     endpoint may be added. pprof heap profiles hold allocation sites only, not contents.
package k8ssecrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
	secretval "github.com/agentkourier/agent-kourier/internal/secret"
)

// defaultOnDemandWait bounds the wait of a Value for the cache of a Secret that it had to follow, or that another
// read was waiting for.
const defaultOnDemandWait = 10 * time.Second

// syncPoll is how often a wait looks at whether its caches have synced, as client-go's own wait does.
const syncPoll = 20 * time.Millisecond

var (
	// ErrNotWatched is returned for a Secret the Resolver does not follow: it was never asked to
	// (Watch, WithPinned), or no longer is (Retain). It is distinct from config.ErrSecretNotFound:
	// the Secret may exist, but nothing here reads it.
	ErrNotWatched = errors.New("secret not watched")
	// ErrNotSynced is returned while a Secret's cache has not synced, when an empty cache would
	// otherwise be indistinguishable from a missing Secret.
	ErrNotSynced = errors.New("secret cache not synced")
	// ErrStopped is returned after Stop, when the caches no longer update and would serve
	// stale data.
	ErrStopped = errors.New("secret resolver stopped")
	// ErrAccessDenied is wrapped by the error that Start or Watch returns, and by the error that
	// Keys, SecretVersion and Value return for a Secret whose cache has not synced, when the API
	// server answered that Secret's list or watch with forbidden or unauthorized. It is a
	// configuration failure, not a cancellation or a timeout, and the error also wraps the API
	// server's own answer. A Secret that synced and was denied afterwards is served from its
	// cache as it was, and the denial is logged.
	ErrAccessDenied = errors.New("the API server denied access to Secrets")
)

var (
	_ config.SecretResolver  = (*Resolver)(nil)
	_ config.SecretVersioner = (*Resolver)(nil)
	_ config.SecretWatcher   = (*Resolver)(nil)
)

// entry is the informer of one Secret. Its fields from pinned on are guarded by Resolver.mu.
type entry struct {
	name     config.SecretName
	factory  informers.SharedInformerFactory
	stop     context.CancelFunc // ends the informer; Shutdown then waits for its goroutines
	ended    <-chan struct{}    // closed once the informer is told to end: by Retain, by Watch, or by Stop
	lister   corelisters.SecretLister
	synced   cache.InformerSynced
	pinned   bool  // followed until Stop, whatever the config says
	retained bool  // named by the latest published snapshot, which is what readiness is about
	cause    error // the latest list or watch failure
}

// Resolver is a config.SecretResolver backed by one Secret informer per followed Secret.
type Resolver struct {
	// ctx bounds the informers' lifetime. Only Stop ends it, so that a caller's timeout on
	// Start cannot silently freeze the caches.
	ctx    context.Context
	cancel context.CancelFunc
	client kubernetes.Interface
	log    *slog.Logger
	pins   []config.SecretName
	wait   time.Duration // the bound of a Value's wait for a cache

	mu      sync.Mutex
	started bool
	entries map[config.SecretName]*entry
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithLogger sets where a failed list or watch is reported. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(r *Resolver) { r.log = l } }

// WithPinned names Secrets that Start follows and that stay followed until Stop, whatever Retain
// is given: the ones that Agent Kourier itself reads and no resource of the config refers to,
// such as the Postgres DSN's.
func WithPinned(names ...config.SecretName) Option {
	return func(r *Resolver) { r.pins = append(r.pins, names...) }
}

// New returns a Resolver that follows Secrets through client. It follows none until Start (the
// pinned ones) and Watch (the config's).
func New(client kubernetes.Interface, opts ...Option) *Resolver {
	r := &Resolver{client: client, log: slog.Default(), wait: defaultOnDemandWait, entries: map[config.SecretName]*entry{}}
	for _, o := range opts {
		o(r)
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r
}

// follow returns the entry of each of names, starting the informer of each that has none. A pinned
// entry stays pinned. It fails with ErrStopped once the Resolver is stopped.
func (r *Resolver) follow(names []config.SecretName, pin bool) ([]*entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return nil, ErrStopped
	}
	out := make([]*entry, 0, len(names))
	for _, n := range names {
		e, ok := r.entries[n]
		if !ok {
			e = r.newEntry(n)
			r.entries[n] = e
		}
		e.pinned = e.pinned || pin
		out = append(out, e)
	}
	return out, nil
}

// newEntry starts the informer of one Secret. The caller holds r.mu and has checked that r is not stopped.
func (r *Resolver) newEntry(n config.SecretName) *entry {
	f := informers.NewSharedInformerFactoryWithOptions(r.client, 0,
		informers.WithNamespace(n.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", n.Name).String()
		}),
		informers.WithTransform(trim))
	secrets := f.Core().V1().Secrets()
	ctx, stop := context.WithCancel(r.ctx)
	e := &entry{name: n, factory: f, stop: stop, ended: ctx.Done(), lister: secrets.Lister(), synced: secrets.Informer().HasSynced}
	// The handler replaces client-go's, which logs through klog. The only error is an
	// informer that has started, and this one has not.
	_ = secrets.Informer().SetWatchErrorHandler(func(_ *cache.Reflector, err error) { r.failed(e, err) })
	f.Start(ctx.Done())
	return e
}

// failed records why an entry's list or watch failed and reports it.
func (r *Resolver) failed(e *entry, err error) {
	r.mu.Lock()
	e.cause = err
	r.mu.Unlock()
	r.log.Warn("secret informer cannot list or watch", "namespace", e.name.Namespace, "secret", e.name.Name, "error", err)
}

// denial returns the latest failure of e if the API server answered it with forbidden or unauthorized, and nil otherwise.
func (r *Resolver) denial(e *entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if forbidden(e.cause) {
		return e.cause
	}
	return nil
}

// forbidden reports whether err is the API server's answer of forbidden or unauthorized.
func forbidden(err error) bool {
	return err != nil && (apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err))
}

// Start follows the pinned Secrets and waits until their caches have synced, so a read of one
// cannot report a false "not found". ctx bounds only the wait: if it ends first, Start returns an
// error naming the Secrets that have not synced, but the informers keep running until Stop, and a
// Secret is served as soon as its cache syncs. A Secret that the API server denied ends the wait
// at once, with an error wrapping ErrAccessDenied.
func (r *Resolver) Start(ctx context.Context) error {
	es, err := r.follow(r.pins, true)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	return r.await(ctx, es)
}

// Watch follows each of names that is not followed yet and waits as Start does, for all of them,
// so it is safe to call again with the same names. It first stops every informer that is not pinned,
// not retained by the latest Retain and not in names: what an earlier Watch started for a config
// that never became a snapshot, and what Value followed for a holder of a dropped Secret. So a run
// of failed reloads leaves at most the informers of the latest one. A Secret of names stays followed
// until a Watch or Retain leaves it out, even if the wait fails. It implements config.SecretWatcher.
func (r *Resolver) Watch(ctx context.Context, names []config.SecretName) error {
	r.release(names)
	es, err := r.follow(names, false)
	if err != nil {
		return err
	}
	return r.await(ctx, es)
}

// await waits until every one of es has synced. It returns early when one that has not is denied,
// when ctx ends, when the Resolver is stopped, and when one that has not synced was let go (Retain or
// Watch stopped its informer, which an on-demand read can meet): the error then wraps ErrNotWatched.
func (r *Resolver) await(ctx context.Context, es []*entry) error {
	t := time.NewTicker(syncPoll)
	defer t.Stop()
	for {
		var unsynced []*entry
		for _, e := range es {
			if !e.synced() {
				unsynced = append(unsynced, e)
			}
		}
		if len(unsynced) == 0 {
			return nil
		}
		for _, e := range unsynced {
			if cause := r.denial(e); cause != nil {
				return fmt.Errorf("secret caches not synced for %s: %w: %w", listNames(unsynced), ErrAccessDenied, cause)
			}
		}
		if r.ctx.Err() == nil {
			for _, e := range unsynced {
				select {
				case <-e.ended:
					return fmt.Errorf("secret %s: %w", e.name, ErrNotWatched)
				default:
				}
			}
		}
		select {
		case <-ctx.Done():
			err := fmt.Errorf("secret caches not synced for %s: %w", listNames(unsynced), context.Cause(ctx))
			if causes := r.causesOf(unsynced); len(causes) > 0 {
				err = fmt.Errorf("%w: %w", err, errors.Join(causes...))
			}
			return err
		case <-r.ctx.Done():
			return fmt.Errorf("secret caches not synced for %s: %w", listNames(unsynced), ErrStopped)
		case <-t.C:
		}
	}
}

// listNames lists the Secrets of es, sorted, for an error message.
func listNames(es []*entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.name.String())
	}
	slices.Sort(out)
	return out
}

// causesOf returns the latest failure of each of es that has one, each naming its Secret.
func (r *Resolver) causesOf(es []*entry) []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []error
	for _, e := range es {
		if e.cause != nil {
			out = append(out, fmt.Errorf("secret %s: %w", e.name, e.cause))
		}
	}
	return out
}

// Retain follows exactly names and the pinned Secrets, and stops the informer of every other
// Secret, which frees what it cached. It implements config.SecretWatcher. A name that is not
// followed is not started: Watch does that.
func (r *Resolver) Retain(names []config.SecretName) {
	keep := make(map[config.SecretName]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	var gone []*entry
	r.mu.Lock()
	for n, e := range r.entries {
		e.retained = keep[n]
		if !e.retained && !e.pinned {
			delete(r.entries, n)
			gone = append(gone, e)
		}
	}
	r.mu.Unlock()
	stopAll(gone)
}

// release stops the informer of every entry that is not pinned, not retained and not in names.
func (r *Resolver) release(names []config.SecretName) {
	keep := make(map[config.SecretName]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	var gone []*entry
	r.mu.Lock()
	for n, e := range r.entries {
		if !e.pinned && !e.retained && !keep[n] {
			delete(r.entries, n)
			gone = append(gone, e)
		}
	}
	r.mu.Unlock()
	stopAll(gone)
}

// stopAll ends the informers of es and waits for their goroutines, which the caller must not do under r.mu.
func stopAll(es []*entry) {
	for _, e := range es {
		e.stop()
		e.factory.Shutdown()
	}
}

// Ready returns nil while the Secrets that Agent Kourier depends on are readable: the pinned ones
// and those of the latest published snapshot. Otherwise it returns an error wrapping ErrNotSynced
// (the Resolver has not started, or one of them has not synced) or ErrStopped. A Secret that a
// reload is waiting for does not count until a snapshot refers to it. A retained Secret that the API server denied and that
// never synced does not count either: where a denied Secret is fatal (a pinned one, or the config of files, which a reload
// does not publish with one the Role does not grant) it never gets here, and with the custom resource source it is a
// Binding's, which reports its credentials as unreadable while the rest keep serving. A pinned Secret always counts.
func (r *Resolver) Ready() error {
	if r.ctx.Err() != nil {
		return ErrStopped
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return fmt.Errorf("the resolver has not started: %w", ErrNotSynced)
	}
	var unsynced []*entry
	for _, e := range r.entries {
		if (e.pinned || e.retained) && !e.synced() && (e.pinned || !forbidden(e.cause)) {
			unsynced = append(unsynced, e)
		}
	}
	if len(unsynced) > 0 {
		return fmt.Errorf("secrets %s: %w", listNames(unsynced), ErrNotSynced)
	}
	return nil
}

// Stop ends the informers and waits for their goroutines to exit. Reads return ErrStopped
// afterwards, and the caches are dropped.
func (r *Resolver) Stop() {
	r.cancel()
	r.mu.Lock()
	all := slices.Collect(maps.Values(r.entries))
	clear(r.entries)
	r.mu.Unlock()
	for _, e := range all {
		e.factory.Shutdown()
	}
}

// get returns the cached Secret. It wraps config.ErrSecretNotFound when the Secret does not
// exist, ErrNotWatched when the Resolver does not follow it, ErrNotSynced while its cache has not
// synced (ErrAccessDenied, with the API server's answer, if that was forbidden), and ErrStopped
// after Stop. A cache that has synced is served as it is, whether or not its list or watch is
// failing now.
func (r *Resolver) get(namespace, name string) (*corev1.Secret, error) {
	if r.ctx.Err() != nil {
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, ErrStopped)
	}
	r.mu.Lock()
	e, ok := r.entries[config.SecretName{Namespace: namespace, Name: name}]
	r.mu.Unlock()
	switch {
	case !ok:
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, ErrNotWatched)
	case !e.synced():
		if cause := r.denial(e); cause != nil {
			return nil, fmt.Errorf("secret %s/%s: %w: %w", namespace, name, ErrAccessDenied, cause)
		}
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, ErrNotSynced)
	}
	s, err := e.lister.Secrets(namespace).Get(name)
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, config.ErrSecretNotFound)
	case err != nil:
		return nil, fmt.Errorf("read secret %s/%s from cache: %w", namespace, name, err)
	}
	return s, nil
}

// getWaiting reads the Secret as get does, but for a Secret that is not followed or whose cache has not synced it follows
// the Secret and waits for the cache, and tries again if the informer was let go meanwhile, until the cache can be read,
// ctx or the on-demand wait ends, or the Resolver stops. A denial is returned at once, as is ErrStopped. It waits in
// await, which looks at the caches every syncPoll and not more often, so it does not spin.
func (r *Resolver) getWaiting(parent context.Context, namespace, name string) (*corev1.Secret, error) {
	ctx, cancel := context.WithTimeout(parent, r.wait)
	defer cancel()
	n := config.SecretName{Namespace: namespace, Name: name}
	for {
		s, err := r.get(namespace, name)
		if !errors.Is(err, ErrNotWatched) && !errors.Is(err, ErrNotSynced) {
			return s, err
		}
		es, ferr := r.follow([]config.SecretName{n}, false)
		if ferr != nil {
			return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, ferr)
		}
		if aerr := r.await(ctx, es); aerr != nil && !errors.Is(aerr, ErrNotWatched) {
			s, err = r.get(namespace, name) // get says why the Secret cannot be read: not synced, or denied
			if err != nil && parent.Err() != nil && (errors.Is(err, ErrNotSynced) || errors.Is(err, ErrNotWatched)) {
				// The caller's context ended, not only the on-demand wait: keep that visible, as Pool.Check needs it to tell
				// a closed pool from a Secret that could not be read. The wait's own timeout reads as ErrNotSynced alone.
				err = fmt.Errorf("%w: %w", err, context.Cause(parent))
			}
			return s, err
		}
	}
}

// Keys returns the sorted key names of the Secret from the cache. It makes no API call, and it
// fails as get does: a Secret that is not followed is ErrNotWatched.
func (r *Resolver) Keys(_ context.Context, namespace, name string) ([]string, error) {
	s, err := r.get(namespace, name)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(s.Data)), nil
}

// SecretVersion returns the resourceVersion of the cached Secret, which the API server changes on
// every write to the Secret, including a new value under the same keys. It reads the cache and
// makes no API call, even for a Secret that is not followed (ErrNotWatched), and it fails as get does.
//
// The cache is as current as its last good list or watch. While one is failing the version stays
// where it was, as do the values Value and Keys return, and a Secret rotated meanwhile is not
// seen until the list or watch succeeds again. Each failure is logged as a warning (see failed);
// SecretVersion does not report it.
func (r *Resolver) SecretVersion(_ context.Context, namespace, name string) (string, error) {
	s, err := r.get(namespace, name)
	if err != nil {
		return "", err
	}
	return s.ResourceVersion, nil
}

// Value returns the value of key in the Secret from the cache; an empty key means
// v1alpha1.DefaultSecretKey. It fails as get does, and wraps secret.ErrKeyNotFound when the
// Secret has no such key, secret.ErrEmptyValue when the key holds no bytes, and
// secret.ErrUnsupportedType when the Secret is not Opaque. Error messages name the namespace,
// Secret and key (and the type), never the value.
//
// A Secret that is not followed is followed now, for a holder of a Binding that a reload has since
// dropped (see the package comment): Value waits for its cache, at most the on-demand wait or until
// ctx ends, and then reads it, so a Value of such a Secret makes API calls and needs a live API
// server and a Role that still grants it. If the Role was revoked meanwhile, or the API server is
// out, the read fails (ErrAccessDenied, or ErrNotSynced once the wait ends), where a cache of the
// whole namespace would have served what it held. A Value of a Secret whose cache another read is
// waiting for waits for it too. If the cache has not synced by then, or access was denied, it
// fails as get does.
func (r *Resolver) Value(ctx context.Context, namespace, name, key string) (secretval.Value, error) {
	if key == "" {
		key = v1alpha1.DefaultSecretKey
	}
	s, err := r.getWaiting(ctx, namespace, name)
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
