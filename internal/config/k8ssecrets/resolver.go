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
// field selector. An answer of forbidden or unauthorized for one Secret ends the wait for that Secret (see
// ErrAccessDenied) and is reported for that Secret alone; what it means is the caller's to decide.
//
// # Denied Secrets
//
// client-go would retry a denied list for ever, 30 to 60 s apart once it has backed off, on the one rate limiter that every informer
// of the client shares, and the names of Secrets are a tenant's to choose and not capped. So the Resolver does not let it. The
// informer of a Secret that the API server answered forbidden or unauthorized is ended at the first answer, whether or not it had
// synced (what it cached is served as it was). It is tried again only when the Reloader asks for the Secret again (Watch, WatchKeys:
// its recheck, which is every SecretsInterval, 1 m unless set) and at least the retry interval after it was last tried
// (WithDeniedRetry), and the tries are paced: the first deniedRetryBurst at once, then one every deniedRetryPace, so that they leave
// the shared limiter room for the requests of everyone else. A reload that comes sooner than the interval, because a config changed,
// does not try it. A Secret whose keys that are read change is tried at once, as a new informer for new keys always is. The
// denial is logged once for each change of what it says (and the recovery, when the informer of a retry replaces the one that was
// denied), not once for each try; another failure is logged each time, client-go backing off between them. One answer of forbidden or
// unauthorized for every Secret at once, such as an expired token of the broker or an outage of the authorizer, ends every informer,
// the broker's own too: what they cached is served, and a rotation is not seen until the retry reaches the Secret. A Secret that does not exist is not denied: the list answers with no items, the
// informer is synced, and it holds one watch open, so it is not retried and costs no repeated request (the answer of the
// API server to a list with a name selector, from its documented behaviour and the tests' fake; not run against a cluster here).
//
// # Values
//
// The cache holds each Secret's name, type, resourceVersion (which SecretVersion returns) and the keys that the
// config reads, and nothing else: the informer's transform drops annotations such as
// last-applied-configuration, which can hold a second copy of the values, and every key that no object of
// the config names, which a writer of the namespace can add in any number up to the API server's 1 MiB for a Secret.
// The Reloader tells the Resolver the keys of each Secret that the served objects read (WatchKeys, RetainKeys), the
// pinned Secrets keep every key, and Watch and Retain, which carry names alone, keep every key too.
// Only Opaque Secrets (or an empty type) keep their values, so that Value of a followed Secret costs no API
// call; every other type (a Helm release, a service account token, TLS keys, a docker config) keeps the names of the keys that are read,
// with no values, and Value refuses it. Keys returns the names of the keys that are read and that the Secret has.
// Value returns a secret.Value, which no formatting path can print, built from a copy of the
// bytes, so a Secret's rotation never changes a Value already read.
//
// When the keys that are read change, a new informer for the new keys lists the Secret, and the informer that serves goes on
// serving until it has: a reload that adds a reference to another key of a Secret does not cut off the keys that are read. Until
// the new informer has listed, Keys answers ErrNotSynced, as for a cache that has not synced, and does not answer that a key
// is missing which the Secret has; the Watch that asked for the keys waits for the list, at most as long as it was given. A key
// is let go the same way, by the Retain of the snapshot that no longer reads it, and so is a key that a Value asked for that no Watch or
// Retain named: it stays kept until the next Retain, and a read of it after that fetches it again, as a read of a Secret that a Retain let
// go does. A read of a key that is not kept, which a holder of an older snapshot's
// Binding makes, waits for the list that keeps it, as a read of a Secret that is not followed does.
//
// The Opaque values of the followed Secrets stay resident for the life of the process, or until
// the Secret is no longer followed. That has these consequences for the deployment:
//
//   - klog verbosity must stay below 8. AGENTKOURIER_KLOG_V sets it (cmd/agent-kourier), and the
//     binary holds it to 7, whatever it is given. The transform runs after client-go has decoded the
//     response, and client-go logs response bodies, Secret lists included, through klog at
//     verbosity 8 and above.
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

const (
	// defaultDeniedRetry is the shortest time between two tries of a denied Secret unless WithDeniedRetry says otherwise. It is the
	// config Reloader's default recheck, which is what asks.
	defaultDeniedRetry = time.Minute
	// deniedRetryBurst is how many denied Secrets one reload tries at once, and deniedRetryPace the time between the starts of the
	// others. At 10 a second, a thousand denied names take under two minutes to go round and use a fifth of the 50 requests a second
	// of the broker's client.
	deniedRetryBurst = 10
	deniedRetryPace  = 100 * time.Millisecond
)

var (
	// ErrNotWatched is returned for a Secret the Resolver does not follow: it was never asked to
	// (Watch, WithPinned), or no longer is (Retain). It is distinct from config.ErrSecretNotFound:
	// the Secret may exist, but nothing here reads it.
	ErrNotWatched = errors.New("secret not watched")
	// ErrNotSynced is returned while a Secret's cache has not synced, or has not listed the keys that are read of it since they
	// changed, when an empty cache would otherwise be indistinguishable from a missing Secret or a missing key.
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
	_ config.SecretResolver     = (*Resolver)(nil)
	_ config.SecretVersioner    = (*Resolver)(nil)
	_ config.SecretWatcher      = (*Resolver)(nil)
	_ config.KeyedSecretWatcher = (*Resolver)(nil)
)

// informer is one list and watch of a Secret, and what it keeps of it: the keys of keys, or all of them.
type informer struct {
	keys    keySet // what its transform keeps; fixed when it is made
	factory informers.SharedInformerFactory
	stop    context.CancelFunc // ends this informer alone; Shutdown then waits for its goroutines
	done    <-chan struct{}    // closed once it is told to end
	lister  corelisters.SecretLister
	synced  cache.InformerSynced
	cause   error // the latest list or watch failure, guarded by Resolver.mu
	// denied: the API server answered forbidden or unauthorized, and the informer was ended for it (guarded by Resolver.mu). fresh: it
	// is a try of a denied Secret that the API server has not answered yet, and cause is still the denial before it, which is what a
	// read says meanwhile.
	denied, fresh bool
}

// entry is the follow of one Secret: the informer that serves it and, while the keys that must be kept have changed, the one that
// takes its place once it has listed. The embedded informer, next and the fields from pinned on are guarded by Resolver.mu.
type entry struct {
	name  config.SecretName
	ctx   context.Context    // the life of the entry, which the life of each of its informers is a part of
	stop  context.CancelFunc // ends the entry and so every informer of it
	ended <-chan struct{}    // closed once the entry is told to end: by Retain, by Watch, or by Stop
	*informer
	next     *informer // being filled with the keys that are wanted now; replaces the embedded informer when it has synced
	pinned   bool      // followed until Stop, whatever the config says; all its keys are kept
	retained bool      // named by the latest published snapshot, which is what readiness is about
	// The keys that something asks to be kept, by who asks: the latest Watch, the latest Retain, and the reads of Value that neither
	// named, which last until the next Retain (or until the Secret is let go). What an informer keeps is their union, and all of them
	// if pinned.
	watched, kept, demanded keySet
	attemptedAt             time.Time // when the informer of the entry was last started, or its retry started: a denied Secret is tried again no sooner than the retry interval after
	retryQueued             bool      // waiting in Resolver.retries
	failing                 string    // what the latest reported denial said, so that the same one is reported once
}

// deniedLocked reports whether the newest informer of e, the one that is being filled or else the one that serves, was ended by a denial.
func (e *entry) deniedLocked() bool {
	l := e.next
	if l == nil {
		l = e.informer
	}
	return l != nil && l.denied
}

// wantLocked returns the keys that the entry's informer has to keep.
func (e *entry) wantLocked() keySet {
	if e.pinned {
		return allKeys()
	}
	return e.watched.union(e.kept).union(e.demanded)
}

// latestCauseLocked is the latest failure of the informer that is being filled, if there is one, and of the one that serves otherwise.
func (e *entry) latestCauseLocked() error {
	if e.next != nil {
		return e.next.cause
	}
	return e.cause
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
	wait   time.Duration  // the bound of a Value's wait for a cache
	retry  time.Duration  // the shortest time between two tries of a denied Secret
	pace   time.Duration  // the time between the starts of the tries that do not go at once
	swaps  sync.WaitGroup // the goroutines that wait for an informer to list and then put it in the place of the one that serves, and the one that paces the tries of denied Secrets (paceRetries)

	mu      sync.Mutex
	started bool
	entries map[config.SecretName]*entry
	retries []*entry // the denied Secrets whose turn to be tried has not come, in order
	pacing  bool     // a goroutine is starting them
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithLogger sets where a failed list or watch is reported. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(r *Resolver) { r.log = l } }

// WithDeniedRetry sets the shortest time between two tries of a Secret that the API server denied: a Watch of it that comes sooner
// does not try it. The Reloader's recheck is what asks regularly, so a value below its SecretsInterval only lets a reload that a changed config causes try it too. The default is 1 m.
func WithDeniedRetry(d time.Duration) Option {
	return func(r *Resolver) {
		if d > 0 {
			r.retry = d
		}
	}
}

// WithPinned names Secrets that Start follows and that stay followed until Stop, whatever Retain
// is given: the ones that Agent Kourier itself reads and no resource of the config refers to,
// such as the Postgres DSN's. They are Agent Kourier's own, and every key of them is kept.
func WithPinned(names ...config.SecretName) Option {
	return func(r *Resolver) { r.pins = append(r.pins, names...) }
}

// New returns a Resolver that follows Secrets through client. It follows none until Start (the
// pinned ones) and Watch (the config's).
func New(client kubernetes.Interface, opts ...Option) *Resolver {
	r := &Resolver{client: client, log: slog.Default(), wait: defaultOnDemandWait, retry: defaultDeniedRetry, pace: deniedRetryPace, entries: map[config.SecretName]*entry{}}
	for _, o := range opts {
		o(r)
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r
}

// read is a Secret and the keys of it that are asked to be kept.
type read struct {
	name config.SecretName
	keys keySet
}

// allOf returns the reads of every key of each of names.
func allOf(names []config.SecretName) []read {
	out := make([]read, len(names))
	for i, n := range names {
		out[i] = read{n, allKeys()}
	}
	return out
}

// readsOf returns the reads of reads.
func readsOf(reads []config.SecretRead) []read {
	out := make([]read, len(reads))
	for i, rd := range reads {
		out[i] = read{rd.SecretName, keysOf(rd.Keys...)}
	}
	return out
}

// who is what asks for keys to be kept.
type who int

const (
	byWatch  who = iota // a Watch names them
	byDemand            // a Value reads a key that no Watch or Retain named
	byPin               // the Secret is pinned, which keeps every key
)

// follow returns the entry of each of reads, starting the informer of each that has none and, for one that has, one for the keys that
// are now asked for if the informer it has does not keep exactly those. A pinned entry stays pinned. It fails with ErrStopped once the
// Resolver is stopped.
func (r *Resolver) follow(reads []read, by who) ([]*entry, error) {
	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return nil, ErrStopped
	}
	out := make([]*entry, 0, len(reads))
	var retired []*informer
	for _, rd := range reads {
		e, ok := r.entries[rd.name]
		if !ok {
			e = r.newEntry(rd.name)
			r.entries[rd.name] = e
		}
		switch by {
		case byWatch:
			e.watched = rd.keys
		case byDemand:
			if !e.wantLocked().covers(rd.keys) {
				e.demanded = e.demanded.union(rd.keys)
			}
		case byPin:
			e.pinned = true
		}
		if e.informer == nil {
			e.informer = r.newInformer(e, e.wantLocked())
		} else {
			retired = append(retired, r.alignLocked(e)...)
		}
		out = append(out, e)
	}
	if by == byWatch {
		r.queueRetriesLocked(time.Now())
		retired = append(retired, r.startRetriesLocked(deniedRetryBurst)...)
	}
	r.mu.Unlock()
	shutdown(retired)
	return out, nil
}

// queueRetriesLocked queues each denied Secret that was last tried at least the retry interval before now, whether or not the Watch
// that is being served names it: a pinned Secret, and a retained one that the config of this Watch leaves out, are tried too.
func (r *Resolver) queueRetriesLocked(now time.Time) {
	for _, e := range r.entries {
		if e.retryQueued || !e.deniedLocked() || now.Sub(e.attemptedAt) < r.retry {
			continue
		}
		e.retryQueued = true
		r.retries = append(r.retries, e)
	}
}

// startRetriesLocked tries up to n of the queued Secrets now and leaves the rest to a goroutine that starts one every pace. It returns
// the informers that were given up, for the caller to shut down once it has let go of r.mu.
func (r *Resolver) startRetriesLocked(n int) (retired []*informer) {
	for n > 0 && len(r.retries) > 0 {
		e := r.retries[0]
		r.retries[0], r.retries = nil, r.retries[1:] // the slot must not keep the entry, and so its cache, alive
		e.retryQueued = false
		if r.entries[e.name] != e || !e.deniedLocked() {
			continue // let go meanwhile, or tried already because its keys changed
		}
		n--
		e.attemptedAt = time.Now() // the interval runs from when it is tried, which a long queue puts well after the recheck that queued it
		denial := e.latestCauseLocked()
		if e.next != nil {
			e.next.stop()
			retired, e.next = append(retired, e.next), nil
		}
		next := r.newInformer(e, e.wantLocked())
		next.cause, next.fresh = denial, true // a read says denied until the API server says otherwise
		e.next = next
		r.swapWhenSynced(e, next)
	}
	if len(r.retries) == 0 {
		r.retries = nil
	}
	if len(r.retries) > 0 && !r.pacing {
		r.pacing = true
		r.swaps.Go(r.paceRetries)
	}
	return retired
}

// paceRetries starts the queued tries of denied Secrets one every pace, until none is left or the Resolver stops.
func (r *Resolver) paceRetries() {
	t := time.NewTicker(r.pace)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
		}
		r.mu.Lock()
		retired := r.startRetriesLocked(1)
		done := len(r.retries) == 0
		if done {
			r.pacing = false
		}
		r.mu.Unlock()
		shutdown(retired)
		if done {
			return
		}
	}
}

// newEntry returns the entry of a Secret that is not followed yet, without an informer. The caller holds r.mu and has checked that r
// is not stopped.
func (r *Resolver) newEntry(n config.SecretName) *entry {
	ctx, stop := context.WithCancel(r.ctx)
	return &entry{name: n, ctx: ctx, stop: stop, ended: ctx.Done(), attemptedAt: time.Now()}
}

// newInformer starts an informer of the entry's Secret that keeps the keys of keys. The caller holds r.mu.
func (r *Resolver) newInformer(e *entry, keys keySet) *informer {
	n := e.name
	f := informers.NewSharedInformerFactoryWithOptions(r.client, 0,
		informers.WithNamespace(n.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", n.Name).String()
		}),
		informers.WithTransform(trimTo(keys)))
	secrets := f.Core().V1().Secrets()
	ctx, stop := context.WithCancel(e.ctx)
	inf := &informer{keys: keys, factory: f, stop: stop, done: ctx.Done(), lister: secrets.Lister(), synced: secrets.Informer().HasSynced}
	// The handler replaces client-go's, which logs through klog. The only error is an
	// informer that has started, and this one has not.
	_ = secrets.Informer().SetWatchErrorHandler(func(_ *cache.Reflector, err error) { r.failed(e, inf, err) })
	f.Start(ctx.Done())
	return inf
}

// alignLocked makes the informer of e keep what is wanted of it now. If the one that serves, or the one that is being filled, keeps
// exactly that there is nothing to do. Otherwise it starts a new informer for it, which takes the place of the one that serves once it
// has listed, so that what is cached is served until then, and gives up the one that was being filled for another set of keys. It
// returns the informers that were given up, for the caller to shut down once it has let go of r.mu.
func (r *Resolver) alignLocked(e *entry) (retired []*informer) {
	want := e.wantLocked()
	if e.next != nil && e.next.keys.equal(want) {
		return nil
	}
	if e.next != nil {
		e.next.stop()
		retired, e.next = append(retired, e.next), nil
	}
	if e.keys.equal(want) {
		return retired
	}
	e.next, e.attemptedAt = r.newInformer(e, want), time.Now()
	r.swapWhenSynced(e, e.next)
	return retired
}

// swapWhenSynced puts next in the place of the informer that serves e once next has synced, and ends the one it replaces. It does
// nothing if next was given up or the entry let go meanwhile.
func (r *Resolver) swapWhenSynced(e *entry, next *informer) {
	r.swaps.Go(func() {
		t := time.NewTicker(syncPoll)
		defer t.Stop()
		for !next.synced() {
			select {
			case <-next.done:
				return
			case <-t.C:
			}
		}
		r.mu.Lock()
		if r.entries[e.name] != e || e.next != next {
			r.mu.Unlock()
			return
		}
		old, recovered := e.informer, e.failing != ""
		e.informer, e.next, e.failing = next, nil, ""
		r.mu.Unlock()
		shutdown([]*informer{old})
		if recovered {
			r.log.Info("secret informer can list and watch again", "namespace", e.name.Namespace, "secret", e.name.Name)
		}
	})
}

// shutdown ends each of infs and waits for its goroutines, which the caller must not do under r.mu.
func shutdown(infs []*informer) {
	for _, inf := range infs {
		inf.stop()
		inf.factory.Shutdown()
	}
}

// failed records why an informer's list or watch failed, ends the informer if the API server denied it (a retry loop that client-go
// would run for ever at the cost of everyone who shares the client: see the package comment), and reports it: a denial once for each change of
// what the denial says (see the package comment).
func (r *Resolver) failed(e *entry, inf *informer, err error) {
	deny := forbidden(err)
	r.mu.Lock()
	inf.cause, inf.fresh, inf.denied = err, false, inf.denied || deny
	// Only a denial is reported once. Any other failure is one that client-go recovers from by itself, backing off between tries, and
	// nothing tells us when it has, so a failure that is deduplicated would never be said again.
	report := !deny || e.failing != err.Error()
	if deny {
		e.failing = err.Error()
	}
	r.mu.Unlock()
	if deny {
		inf.stop() // what it cached stays and is served; Shutdown, when it is let go of, finds its goroutines gone
	}
	if !report {
		return
	}
	if deny {
		r.log.Warn("secret informer cannot list or watch", "namespace", e.name.Namespace, "secret", e.name.Name, "error", err, "retried", "at a recheck at least "+r.retry.String()+" from now")
		return
	}
	r.log.Warn("secret informer cannot list or watch", "namespace", e.name.Namespace, "secret", e.name.Name, "error", err)
}

// denial returns the latest failure of e if the API server answered it with forbidden or unauthorized, and nil otherwise.
func (r *Resolver) denial(e *entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := e.next
	if l == nil {
		l = e.informer
	}
	if cause := e.latestCauseLocked(); forbidden(cause) && !l.fresh { // a try that has not been answered is waited for
		return cause
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
	es, err := r.follow(allOf(r.pins), byPin)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	return r.await(ctx, es)
}

// Watch follows each of names, every key of it, and is WatchKeys for a caller that does not know which keys it reads.
// It implements config.SecretWatcher.
func (r *Resolver) Watch(ctx context.Context, names []config.SecretName) error {
	return r.watch(ctx, allOf(names))
}

// WatchKeys follows each of reads that is not followed yet, and keeps of each only the keys that it names, and waits as Start
// does, for all of them, so it is safe to call again with the same reads. It first stops every informer that is not pinned,
// not retained by the latest Retain and not in reads: what an earlier Watch started for a config that never became a snapshot, and what
// Value followed for a holder of a dropped Secret. So a run of failed reloads leaves at most the informers of the latest one. A
// Secret of reads stays followed until a Watch or Retain leaves it out, even if the wait fails.
//
// A Secret that is followed already, with other keys, is not let go of: the informer that serves it goes on serving until a new
// one, which keeps the keys named now, has listed, and the wait is for that. The keys that the latest Retain named stay kept, so a
// reload that does not become the snapshot cannot drop what the last good snapshot reads. It implements config.KeyedSecretWatcher.
func (r *Resolver) WatchKeys(ctx context.Context, reads []config.SecretRead) error {
	return r.watch(ctx, readsOf(reads))
}

func (r *Resolver) watch(ctx context.Context, reads []read) error {
	r.release(reads)
	es, err := r.follow(reads, byWatch)
	if err != nil {
		return err
	}
	return r.await(ctx, es)
}

// covered reports whether e serves a synced cache that keeps every key that is wanted of it.
func (r *Resolver) covered(e *entry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return e.synced() && e.keys.covers(e.wantLocked())
}

// await waits until every one of es has synced and keeps the keys that are wanted of it. It returns early when one that has not is
// denied, when ctx ends, when the Resolver is stopped, and when one that has not synced was let go (Retain or
// Watch stopped its informer, which an on-demand read can meet): the error then wraps ErrNotWatched.
func (r *Resolver) await(ctx context.Context, es []*entry) error {
	t := time.NewTicker(syncPoll)
	defer t.Stop()
	for {
		var unsynced []*entry
		for _, e := range es {
			if !r.covered(e) {
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
		if cause := e.latestCauseLocked(); cause != nil {
			out = append(out, fmt.Errorf("secret %s: %w", e.name, cause))
		}
	}
	return out
}

// Retain follows exactly names and the pinned Secrets, every key of each, and stops the informer of every other
// Secret, which frees what it cached. It implements config.SecretWatcher. A name that is not
// followed is not started: Watch does that.
func (r *Resolver) Retain(names []config.SecretName) { r.retain(allOf(names)) }

// RetainKeys is Retain for a snapshot that reads only the keys that reads name: the informer of a Secret keeps those, and the
// keys that an earlier Retain named and this one does not are let go. It implements config.KeyedSecretWatcher.
func (r *Resolver) RetainKeys(reads []config.SecretRead) { r.retain(readsOf(reads)) }

func (r *Resolver) retain(reads []read) {
	keep := make(map[config.SecretName]keySet, len(reads))
	for _, rd := range reads {
		keep[rd.name] = rd.keys
	}
	var ended []*entry
	var retired []*informer
	r.mu.Lock()
	for n, e := range r.entries {
		keys, named := keep[n]
		e.retained, e.kept = named, keys
		e.demanded = keySet{} // what a holder of an older snapshot read is let go with that snapshot, as a Secret is
		if !named && !e.pinned {
			delete(r.entries, n)
			ended = append(ended, e)
		} else {
			retired = append(retired, r.alignLocked(e)...)
		}
	}
	infs := append(informersOf(ended), retired...)
	r.mu.Unlock()
	endAll(ended)
	shutdown(infs)
}

// release stops the informer of every entry that is not pinned, not retained and not in reads, and takes back from the entries that are
// kept for some other reason the keys that the latest Watch asked them to keep.
func (r *Resolver) release(reads []read) {
	keep := make(map[config.SecretName]bool, len(reads))
	for _, rd := range reads {
		keep[rd.name] = true
	}
	var ended []*entry
	var retired []*informer
	r.mu.Lock()
	for n, e := range r.entries {
		switch {
		case keep[n]:
		case !e.pinned && !e.retained:
			delete(r.entries, n)
			ended = append(ended, e)
		default:
			e.watched = keySet{}
			retired = append(retired, r.alignLocked(e)...)
		}
	}
	infs := append(informersOf(ended), retired...)
	r.mu.Unlock()
	endAll(ended)
	shutdown(infs)
}

// informersOf returns the informers that serve or fill es, which are no longer in r.entries. The caller holds r.mu.
func informersOf(es []*entry) []*informer {
	var out []*informer
	for _, e := range es {
		out = append(out, e.informer)
		if e.next != nil {
			out = append(out, e.next)
		}
	}
	return out
}

// endAll tells each of es to end. Their informers are shut down with shutdown, which the caller must not do under r.mu.
func endAll(es []*entry) {
	for _, e := range es {
		e.stop()
	}
}

// Ready returns nil while the Secrets that Agent Kourier depends on are readable: the pinned ones
// and those of the latest published snapshot. Otherwise it returns an error wrapping ErrNotSynced
// (the Resolver has not started, or one of them has not synced) or ErrStopped. A Secret that a
// reload is waiting for does not count until a snapshot refers to it, and nor do the keys that a reload is waiting for of a Secret
// that serves: what is cached is served. A retained Secret that the API server denied and that
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
	infs := informersOf(all)
	r.mu.Unlock()
	for _, inf := range infs {
		inf.factory.Shutdown()
	}
	r.swaps.Wait()
}

// get returns the cached Secret. It wraps config.ErrSecretNotFound when the Secret does not
// exist, ErrNotWatched when the Resolver does not follow it, ErrNotSynced while its cache has not
// synced (ErrAccessDenied, with the API server's answer, if that was forbidden), and ErrStopped
// after Stop. A cache that has synced is served as it is, whether or not its list or watch is
// failing now.
//
// A caller that reads keys passes enough, which says whether the cache keeps them (it runs under r.mu). A cache that does not keep
// them, because the keys it was asked for have changed and the informer for the new ones has not listed, is not served: it is
// answered as one that has not synced (or is denied), since it would say that a key is missing which the Secret may have. A caller
// that reads nothing of the keys, the version, passes nil.
func (r *Resolver) get(namespace, name string, enough func(*entry) bool) (*corev1.Secret, error) {
	if r.ctx.Err() != nil {
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, ErrStopped)
	}
	r.mu.Lock()
	e, ok := r.entries[config.SecretName{Namespace: namespace, Name: name}]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, ErrNotWatched)
	}
	inf, synced, cause := e.informer, e.synced(), e.latestCauseLocked()
	keeps := enough == nil || enough(e)
	r.mu.Unlock()
	if !synced || !keeps {
		if forbidden(cause) {
			return nil, fmt.Errorf("secret %s/%s: %w: %w", namespace, name, ErrAccessDenied, cause)
		}
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, ErrNotSynced)
	}
	s, err := inf.lister.Secrets(namespace).Get(name)
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, config.ErrSecretNotFound)
	case err != nil:
		return nil, fmt.Errorf("read secret %s/%s from cache: %w", namespace, name, err)
	}
	return s, nil
}

// keepsKey is the enough of a read of the one key.
func keepsKey(key string) func(*entry) bool {
	return func(e *entry) bool { return e.keys.has(key) }
}

// keepsWanted is the enough of Keys, which answers for what the config reads: the keys of the latest Watch and Retain. A key that
// only a holder of an older snapshot asked for (demanded) is not one of them, so that fetching it does not make Keys fail and a reload
// that resolves fail with it.
func keepsWanted(e *entry) bool { return e.keys.covers(e.watched.union(e.kept)) }

// getWaiting reads the Secret as get does, but for a Secret that is not followed, whose cache has not synced, or that does not keep
// the key, it follows the Secret with the key and waits for the cache, and tries again if the informer was let go meanwhile, until the
// cache can be read, ctx or the on-demand wait ends, or the Resolver stops. A denial is returned at once, as is ErrStopped. It waits
// in await, which looks at the caches every syncPoll and not more often, so it does not spin.
func (r *Resolver) getWaiting(parent context.Context, namespace, name, key string) (*corev1.Secret, error) {
	ctx, cancel := context.WithTimeout(parent, r.wait)
	defer cancel()
	reads := []read{{config.SecretName{Namespace: namespace, Name: name}, keysOf(key)}}
	for {
		s, err := r.get(namespace, name, keepsKey(key))
		if !errors.Is(err, ErrNotWatched) && !errors.Is(err, ErrNotSynced) {
			return s, err
		}
		es, ferr := r.follow(reads, byDemand)
		if ferr != nil {
			return nil, fmt.Errorf("secret %s/%s: %w", namespace, name, ferr)
		}
		aerr := r.await(ctx, es)
		if (aerr != nil && !errors.Is(aerr, ErrNotWatched)) || ctx.Err() != nil { // the bound is this function's, whatever await answers
			s, err = r.get(namespace, name, keepsKey(key)) // get says why the Secret cannot be read: not synced, or denied
			if err != nil && parent.Err() != nil && (errors.Is(err, ErrNotSynced) || errors.Is(err, ErrNotWatched)) {
				// The caller's context ended, not only the on-demand wait: keep that visible, as Pool.Check needs it to tell
				// a closed pool from a Secret that could not be read. The wait's own timeout reads as ErrNotSynced alone.
				err = fmt.Errorf("%w: %w", err, context.Cause(parent))
			}
			return s, err
		}
		select { // await found the cache ready, or the entry let go, and get still cannot answer: look again after a poll, not at once
		case <-ctx.Done():
		case <-time.After(syncPoll):
		}
	}
}

// Keys returns the sorted names of the keys of the Secret that the config reads and that the Secret has, from the cache. Those are
// the keys that were named to the latest WatchKeys and RetainKeys (all of the Secret's if it was followed by Watch), and a Secret has
// others the cache does not hold. It makes no API call, and it fails as get does: a Secret that is not followed is ErrNotWatched,
// and one for which the keys that were named last have not been listed yet is ErrNotSynced.
func (r *Resolver) Keys(_ context.Context, namespace, name string) ([]string, error) {
	s, err := r.get(namespace, name, keepsWanted)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(s.Data)), nil
}

// SecretVersion returns the resourceVersion of the cached Secret, which the API server changes on
// every write to the Secret, including a new value under the same keys, and one under a key that is not kept. It reads the cache and
// makes no API call, even for a Secret that is not followed (ErrNotWatched), and it fails as get does.
//
// The cache is as current as its last good list or watch. While one is failing the version stays
// where it was, as do the values Value and Keys return, and a Secret rotated meanwhile is not
// seen until the list or watch succeeds again, which for a denial is the next try at a recheck (see the package comment). A failure
// is logged as a warning (a denial once for each change of what it says: see failed); SecretVersion does not report it.
func (r *Resolver) SecretVersion(_ context.Context, namespace, name string) (string, error) {
	s, err := r.get(namespace, name, nil)
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
// whole namespace would have served what it held. So is a Secret that is followed without the key:
// one that no WatchKeys or RetainKeys named, which a holder of a dropped Binding reads, is kept from
// now until the next Retain, and Value waits for the list that keeps it, where a cache of
// the whole Secret would have had it. A Value of a Secret whose cache another read is
// waiting for waits for it too. If the cache has not synced by then, or access was denied, it
// fails as get does.
func (r *Resolver) Value(ctx context.Context, namespace, name, key string) (secretval.Value, error) {
	if key == "" {
		key = v1alpha1.DefaultSecretKey
	}
	s, err := r.getWaiting(ctx, namespace, name, key)
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
