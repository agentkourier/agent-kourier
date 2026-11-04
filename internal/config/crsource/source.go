// Package crsource is the config.Source that reads AgentBackend, ChatConnection and Binding from the custom resources of a
// cluster, through informers of the dynamic client, so that a broker can be configured by objects its teams own (spec D50).
// It lives apart from internal/config so that package keeps importing apimachinery only.
//
// # What it gives
//
// Every object goes through config.DecodeObject, the decoder of the file loader, so a custom resource and a file are held
// to the same Default and Validate. Unlike the files, one bad object is not a bad config: an object that fails to decode or
// validate is left out of the Set and listed in Set.Invalid, and when the object had a good generation, that generation is
// kept in memory and put in the Set in its place, so that a typo in an update does not take a Binding out of routing. A
// Reloader that is PerObject turns the two into what is served and what is reported.
//
// # What it reads
//
// The broker owns one ChatConnection, named in Options. The Set holds that connection alone, so the broker serves exactly
// one, and the Bindings whose connectionRef names it, or names no connection that exists (so that some broker reports it).
// A Binding that names another existing connection belongs to the broker that serves that one, and is not in the Set and not
// reported here. If the own connection is deleted while the broker runs, the Set keeps the last good generation of it, and
// says so in Set.Invalid.
//
// # Change detection
//
// Fingerprint is the sorted uid and metadata.generation of every object, never resourceVersion: a status write bumps
// resourceVersion and not generation, and a source that moved with it would start a reload, a probe pass and a status write
// for every status write.
//
// # RBAC
//
// The ServiceAccount needs list and watch on bindings, agentbackends and chatconnections of agentkourier.dev, in every
// namespace (a ClusterRole), and nothing else for this source; it never issues a get. Start lists each kind once and opens
// and closes a watch on it before it starts the informers, so that a missing CRD or a missing right is a clear error at
// start and not an informer that retries for ever.
//
// # Staleness
//
// The caches are kept while the API server cannot be reached, which is right: an outage is not every object removed. Stale
// says that they may lag. Two things feed it. A probe lists one object of each kind every 30 s, with a 10 s timeout, and
// is the only thing that sees an outage: client-go's reflector retries a refused or throttled watch without telling anyone,
// and turns a timeout or an end of stream into an empty watch, so a cut connection reaches no handler. The probe's verdict
// is exact: it is stale from the first failed probe to the first good one. The informers' own error handler adds what does
// reach it, a list or watch the API server answers with an error status such as forbidden after a Role is revoked, for 90 s
// after the report. The probe is three small lists per 30 s on the source's own client, so it shares that client's rate
// limiter with the three informers: 0.1 requests a second, against the limit of 5 that client-go gives a client that sets
// none.
package crsource

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

var (
	// ErrCRDsAbsent is wrapped by the error of Start when the API server does not serve one of the three resources, which
	// means the CRDs are not installed (or not at this version). It is the operator's to fix.
	ErrCRDsAbsent = errors.New("the custom resource definitions are not installed")
	// ErrAccessDenied is wrapped by the error of Start when the API server denies the broker the right to list one of the
	// three resources. It is the operator's to fix: the RBAC of the ServiceAccount.
	ErrAccessDenied = errors.New("the API server denied access to the custom resources")
	// ErrNotSynced is returned by Set while an informer's cache has not synced, since an empty cache would read as every
	// object removed, and by BindingStatus while the Binding informer has not.
	ErrNotSynced = errors.New("the custom resource caches have not synced")
	// ErrStopped is returned by Set and BindingStatus after Stop.
	ErrStopped = errors.New("the custom resource source is stopped")
)

// The three resources, by the plural names the CRDs give them.
var (
	bindingsGVR    = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "bindings"}
	backendsGVR    = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "agentbackends"}
	connectionsGVR = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "chatconnections"}
)

var (
	_ config.Source      = (*Source)(nil)
	_ config.StaleSource = (*Source)(nil)
)

// Options configures a Source. Client and Connection are required.
type Options struct {
	// Client reads the resources. In a broker it is the dynamic client of the pod's own ServiceAccount.
	Client dynamic.Interface
	// Connection is the ChatConnection this broker serves, as AGENTKOURIER_CHAT_CONNECTION names it.
	Connection v1alpha1.ObjectRef
	// Logger receives the failures of a list or watch. It defaults to slog.Default().
	Logger *slog.Logger
}

// key identifies an object of one kind.
type key struct{ kind, namespace, name string }

// goodObject is the last generation of an object that passed Default and Validate, as the JSON it was decoded from. It is
// decoded again for each Set, so that no Set shares memory with another.
type goodObject struct {
	uid        types.UID
	generation int64
	json       []byte
}

// Source is a config.Source of the custom resources. Start it, and Stop it when done.
type Source struct {
	client dynamic.Interface
	own    key
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex // guards the fields below
	factory dynamicinformer.DynamicSharedInformerFactory
	stores  [3]cache.Store // bindings, backends, connections
	synced  [3]cache.InformerSynced
	good    map[key]goodObject
	started bool

	bindingEvents atomic.Uint64 // the events the Binding informer has delivered (BindingEvents)

	errMu    sync.Mutex
	lastErr  error     // the latest failure of a list or watch of an informer, from its error handler
	errAt    time.Time // when it was reported
	probeErr error     // the latest probe's failure, nil once a probe passes
	now      func() time.Time

	probeEvery, probeTimeout time.Duration
	probeExited              func()         // a test's hook, called as the probe's goroutine ends
	wg                       sync.WaitGroup // the probe
}

// New returns a Source that has read nothing. It refuses Options without a Client or a Connection.
func New(opts Options) (*Source, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("crsource: Options.Client is required")
	case opts.Connection.Name == "" || opts.Connection.Namespace == "":
		return nil, errors.New("crsource: Options.Connection must name a ChatConnection as namespace and name")
	}
	s := &Source{
		client: opts.Client, log: cmp.Or(opts.Logger, slog.Default()), good: map[key]goodObject{},
		own: key{config.KindChatConnection, opts.Connection.Namespace, opts.Connection.Name}, now: time.Now,
		probeEvery: probeEvery, probeTimeout: probeTimeout,
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// Start lists each of the three resources once, so that a missing CRD or a missing right is an error that says so, then
// starts an informer for each and waits for its cache to sync. ctx bounds the wait only: the informers run until Stop. On
// an error nothing is left to wait for, but Stop must still be called. Call Start once.
func (s *Source) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("crsource: Start was called twice")
	}
	s.started = true
	s.mu.Unlock()

	gvrs := [3]schema.GroupVersionResource{bindingsGVR, backendsGVR, connectionsGVR}
	for _, gvr := range gvrs {
		if err := s.probe(ctx, gvr); err != nil {
			return err
		}
	}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(s.client, 0, metav1.NamespaceAll, nil)
	s.mu.Lock()
	for i, gvr := range gvrs {
		inf := factory.ForResource(gvr).Informer()
		// The handler replaces client-go's, which logs through klog. The only error is an informer that has started.
		_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
			s.errMu.Lock()
			s.lastErr, s.errAt = fmt.Errorf("%s.%s: %w", gvr.Resource, gvr.Group, err), s.now()
			s.errMu.Unlock()
			s.log.Warn("custom resource informer cannot list or watch", "resource", gvr.Resource, "error", err)
		})
		s.stores[i], s.synced[i] = inf.GetStore(), inf.HasSynced
		if i == 0 { // the Bindings: BindingEvents tells a reader of their statuses that one moved
			count := func(...any) { s.bindingEvents.Add(1) }
			_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
				AddFunc: func(any) { count() }, UpdateFunc: func(any, any) { count() }, DeleteFunc: func(any) { count() },
			})
		}
	}
	s.factory = factory
	s.mu.Unlock()
	factory.Start(s.ctx.Done())
	s.wg.Go(func() { s.probeLoop(s.ctx, gvrs[:]) })
	syncStop := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { close(syncStop) })
	defer stop()
	if !cache.WaitForCacheSync(syncStop, s.synced[:]...) {
		return fmt.Errorf("%w: %w", ErrNotSynced, cmp.Or(context.Cause(ctx), s.ctx.Err(), errors.New("stopped")))
	}
	return nil
}

// probe lists one resource, a page of one, and opens a watch on it and closes it, at start, to tell the three things that go
// wrong then apart, whichever of the two the ServiceAccount may not do.
func (s *Source) probe(ctx context.Context, gvr schema.GroupVersionResource) error {
	_, err := s.client.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if err == nil {
		var w watch.Interface
		one := int64(1)
		if w, err = s.client.Resource(gvr).Watch(ctx, metav1.ListOptions{TimeoutSeconds: &one}); err == nil {
			w.Stop()
		}
	}
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: the API server does not serve %s.%s %s; install the CRDs of Agent Kourier first: %w", ErrCRDsAbsent, gvr.Resource, gvr.Group, gvr.Version, err)
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return fmt.Errorf("%w: %s.%s: the ServiceAccount needs list and watch on it in every namespace: %w", ErrAccessDenied, gvr.Resource, gvr.Group, err)
	}
	return fmt.Errorf("list or watch %s.%s: %w", gvr.Resource, gvr.Group, err)
}

// The probe's pace: a list of one object of each kind every probeEvery, each bounded by probeTimeout.
const (
	probeEvery   = 30 * time.Second
	probeTimeout = 10 * time.Second
)

// staleAfter is how long after an informer's error handler reports a failure the source says it is stale. The handler is
// called for what the API server answers with an error status (a denied list or watch), by a reflector that backs off
// exponentially to about 30 s, and with jitter the gap between two reports is up to about a minute, so a failure that goes
// on is seen again inside the window, and one that has cleared stops being said after it. An outage is not one of those: the
// probe sees it.
const staleAfter = 90 * time.Second

// probeLoop lists one object of each kind every probeEvery, until the source is stopped, and records whether the API server
// answered. A list is what a refused connection, a timeout and a throttled request all fail, which the informers' own error
// handler is never told of.
func (s *Source) probeLoop(ctx context.Context, gvrs []schema.GroupVersionResource) {
	if s.probeExited != nil {
		defer s.probeExited()
	}
	t := time.NewTicker(s.probeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var failed error
		for _, gvr := range gvrs {
			if err := s.listOne(ctx, gvr); err != nil {
				failed = fmt.Errorf("%s.%s: %w", gvr.Resource, gvr.Group, err)
				break // one that fails is enough, and the others would each wait out the timeout
			}
		}
		if ctx.Err() != nil { // stopped while a list was in flight: that is no outage, and nothing is left to say it
			return
		}
		s.errMu.Lock()
		if failed != nil && s.probeErr == nil {
			s.log.Warn("the API server does not answer the custom resource probe; the cache is kept and may lag", "error", failed)
		} else if failed == nil && s.probeErr != nil {
			s.log.Info("the API server answers the custom resource probe again")
		}
		s.probeErr = failed
		s.errMu.Unlock()
	}
}

// listOne lists a page of one object of gvr, and gives up after probeTimeout. The call runs on a goroutine of its own and the
// wait is on the clock and the source's end, so that a client that ignores its context cannot hold the probe or Stop.
func (s *Source) listOne(parent context.Context, gvr schema.GroupVersionResource) error {
	ctx, cancel := context.WithTimeout(parent, s.probeTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.client.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("no answer in %s: %w", s.probeTimeout, context.Cause(ctx))
	}
}

// Stale returns why the cache may lag the cluster, or nil. It is the latest probe's failure, from that probe until the first
// that passes, or else the latest failure that an informer's error handler reported, for staleAfter after it was reported.
// The cache is kept while the cluster cannot be reached, which is right (an outage is not every object removed), and this is
// how a Reloader says so. It implements config.StaleSource.
func (s *Source) Stale() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.probeErr != nil {
		return s.probeErr
	}
	if s.lastErr != nil && s.now().Sub(s.errAt) < staleAfter {
		return s.lastErr
	}
	return nil
}

// Stop ends the informers and the probe and waits for their goroutines to exit. Set reports ErrStopped afterwards.
func (s *Source) Stop() {
	s.cancel()
	s.wg.Wait()
	s.mu.Lock()
	factory := s.factory
	s.mu.Unlock()
	if factory != nil {
		factory.Shutdown()
	}
}

// Fingerprint is a digest of the uid and generation of every object. It is "!" and why while the caches have not synced or
// the source is stopped, so that a state that cannot be read looks the same on every poll.
func (s *Source) Fingerprint(context.Context) string {
	l, err := s.list()
	if err != nil {
		return "!" + err.Error()
	}
	return l.fingerprint()
}

// BindingStatus is what the cache of the Binding informer holds of one Binding's status: the object it is the status of, and
// its conditions as written.
type BindingStatus struct {
	// UID and Generation are the cached object's, so that a status can be told from that of an object of the same name that
	// was deleted and made again.
	UID        types.UID
	Generation int64
	// Conditions is status.conditions, in the order it is stored, none if it has none. It is a copy.
	Conditions []metav1.Condition
}

// BindingStatus returns the status that the informer's cache holds for the Binding namespace/name, and false if the cache
// holds no such Binding. It reads the cached object and nothing else: nothing the Set serves, and nothing in the
// fingerprint, depends on it, and a change of status is no change of the config. It fails with ErrNotSynced until the Binding
// informer has synced, since an empty cache would read as a Binding with no status, and with ErrStopped after Stop.
func (s *Source) BindingStatus(namespace, name string) (BindingStatus, bool, error) {
	if s.ctx.Err() != nil {
		return BindingStatus{}, false, ErrStopped
	}
	s.mu.Lock()
	store, synced := s.stores[0], s.synced[0]
	s.mu.Unlock()
	if store == nil || synced == nil || !synced() {
		return BindingStatus{}, false, ErrNotSynced
	}
	item, exists, err := store.GetByKey(namespace + "/" + name)
	if err != nil || !exists {
		return BindingStatus{}, false, err
	}
	u, ok := item.(*unstructured.Unstructured)
	if !ok {
		return BindingStatus{}, false, nil
	}
	return BindingStatus{UID: u.GetUID(), Generation: u.GetGeneration(), Conditions: ConditionsOf(u)}, true, nil
}

// BindingEvents counts the events (an object added, changed or deleted) that the Binding informer has delivered, and only grows.
// A reader of the statuses that sees it unchanged since it last looked knows that no Binding has changed since: the cache is
// updated before the event is counted, so a change that the count has not caught up with yet is seen at the next look. It reads
// no object, and is what lets a reader skip a pass over every status when nothing moved.
func (s *Source) BindingEvents() uint64 { return s.bindingEvents.Load() }

// ConditionsOf reads status.conditions of an object, in the order they are stored, and none if it has no readable ones. It
// copies, and so does not change the object, which may be one that an informer's cache holds.
func ConditionsOf(u *unstructured.Unstructured) []metav1.Condition {
	if u == nil {
		return nil
	}
	raw, _, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil {
		return nil
	}
	var out []metav1.Condition
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var c metav1.Condition
		if runtime.DefaultUnstructuredConverter.FromUnstructured(m, &c) == nil {
			out = append(out, c)
		}
	}
	return out
}

// Set reads the objects as of one moment, decodes each, and returns them with the fingerprint of what it read. See the
// package comment for what it leaves out and what it keeps. It fails only when the caches have not synced or the source is
// stopped. Each call returns a Set of its own.
func (s *Source) Set(context.Context) (*config.Set, string, error) {
	l, err := s.list()
	if err != nil {
		return nil, "!" + err.Error(), err
	}
	fp := l.fingerprint()
	s.mu.Lock()
	defer s.mu.Unlock()
	set := &config.Set{}
	live := map[key]bool{}
	read(s, config.KindAgentBackend, l.backends, live, func(*unstructured.Unstructured) bool { return true }, &set.AgentBackends, &set.Invalid)
	read(s, config.KindChatConnection, l.connections, live,
		func(u *unstructured.Unstructured) bool { return keyOf(config.KindChatConnection, u) == s.own }, &set.ChatConnections, &set.Invalid)
	read(s, config.KindBinding, l.bindings, live, func(u *unstructured.Unstructured) bool { return s.owns(l, u) }, &set.Bindings, &set.Invalid)

	// The broker's own connection is held when it is deleted: a running broker keeps its last good one.
	if !live[s.own] {
		if g, ok := s.good[s.own]; ok {
			if conn, _ := config.DecodeObject[v1alpha1.ChatConnection](g.json); conn != nil {
				set.ChatConnections = append(set.ChatConnections, *conn)
				set.Invalid = append(set.Invalid, config.Invalid{Kind: s.own.kind, Namespace: s.own.namespace, Name: s.own.name, Served: g.generation, Deleted: true})
			}
		}
	}
	for k := range s.good {
		if !live[k] && k != s.own {
			delete(s.good, k)
		}
	}
	return set, fp, nil
}

// read decodes the listed objects of one kind that the broker owns (owns says so of an object), as T. A good one is
// appended to dst and remembered. A bad one is appended to invalid, with the last good generation of the same object, the
// same incarnation of it, in dst in its place. live gets the key of each object that was listed and owned, bad or good. The
// caller holds s.mu.
//
// Two rules keep a refused generation from taking away what was served. The own connection is one object for as long as the
// broker runs, whatever uid it has, so a connection that was deleted and made again invalid still serves the last good one.
// And a generation that fails to decode cannot be trusted to say whose it is: when the object has a last good generation,
// that one decides whether the broker owns it, so an edit that is invalid and also names another broker's connection does not
// take a Binding out of routing unreported.
func read[T any, PT interface {
	*T
	Default()
	Validate() error
}](s *Source, kind string, objs []*unstructured.Unstructured, live map[key]bool, owns func(*unstructured.Unstructured) bool, dst *[]T, invalid *[]config.Invalid) {
	for _, u := range objs {
		k := keyOf(kind, u)
		g, hasGood := s.good[k]
		same := hasGood && (g.uid == u.GetUID() || k == s.own)
		mine := owns(u)
		if !mine && !same {
			continue
		}
		j, err := u.MarshalJSON()
		var obj *T
		var errs []error
		if err != nil {
			errs = []error{err}
		} else {
			obj, errs = config.DecodeObject[T, PT](j)
		}
		if obj != nil {
			if !mine { // a valid edit to another broker's connection: the Binding is that broker's now, and the pruning forgets it
				continue
			}
			live[k] = true
			s.good[k] = goodObject{uid: u.GetUID(), generation: u.GetGeneration(), json: j}
			*dst = append(*dst, *obj)
			continue
		}
		if !mine { // refused, and its own words say it is another broker's: the last good generation says otherwise
			prev := &unstructured.Unstructured{}
			if prev.UnmarshalJSON(g.json) != nil || !owns(prev) {
				continue
			}
		}
		live[k] = true
		inv := config.Invalid{Kind: kind, Namespace: u.GetNamespace(), Name: u.GetName(), Generation: u.GetGeneration(), Message: config.CapMessage(joinErrors(errs))}
		if kind != config.KindBinding {
			inv.AllowedNamespaces, _, _ = unstructured.NestedStringSlice(u.Object, "spec", "allowedNamespaces")
		}
		if same {
			if last, _ := config.DecodeObject[T, PT](g.json); last != nil {
				*dst = append(*dst, *last)
				inv.Served = g.generation
			}
		}
		*invalid = append(*invalid, inv)
	}
}

// joinErrors renders each error on its own, separated by "; ".
func joinErrors(errs []error) string {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

func keyOf(kind string, u *unstructured.Unstructured) key {
	return key{kind, u.GetNamespace(), u.GetName()}
}

// owns reports whether the Binding u is the broker's to serve or to report: its connectionRef names the broker's connection,
// or names no connection that exists. One that names another existing connection is that connection's broker's. The
// reference is read from the raw object, since a Binding that is invalid may still say which connection it means.
func (s *Source) owns(l listing, u *unstructured.Unstructured) bool {
	ns, _, _ := unstructured.NestedString(u.Object, "spec", "chat", "connectionRef", "namespace")
	name, _, _ := unstructured.NestedString(u.Object, "spec", "chat", "connectionRef", "name")
	ref := key{config.KindChatConnection, cmp.Or(ns, u.GetNamespace()), name}
	return ref == s.own || !l.hasConnection[ref]
}

// listing is what the three caches held at one moment.
type listing struct {
	bindings, backends, connections []*unstructured.Unstructured
	hasConnection                   map[key]bool
}

// list reads the three caches, each sorted by namespace and name. It fails while one has not synced, and once stopped.
func (s *Source) list() (listing, error) {
	if s.ctx.Err() != nil {
		return listing{}, ErrStopped
	}
	s.mu.Lock()
	stores, synced := s.stores, s.synced
	s.mu.Unlock()
	for _, has := range synced {
		if has == nil || !has() {
			return listing{}, ErrNotSynced
		}
	}
	l := listing{hasConnection: map[key]bool{}}
	l.bindings, l.backends, l.connections = objects(stores[0]), objects(stores[1]), objects(stores[2])
	for _, u := range l.connections {
		l.hasConnection[keyOf(config.KindChatConnection, u)] = true
	}
	return l, nil
}

// objects returns the objects of a store, sorted by namespace and name.
func objects(store cache.Store) []*unstructured.Unstructured {
	items := store.List()
	out := make([]*unstructured.Unstructured, 0, len(items))
	for _, it := range items {
		if u, ok := it.(*unstructured.Unstructured); ok {
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(a, b *unstructured.Unstructured) int {
		return cmp.Or(cmp.Compare(a.GetNamespace(), b.GetNamespace()), cmp.Compare(a.GetName(), b.GetName()))
	})
	return out
}

// fingerprint digests the kind, namespace, name, uid and generation of each object. It leaves out resourceVersion, which a
// status write changes (see the package comment).
func (l listing) fingerprint() string {
	h := sha256.New()
	for _, group := range []struct {
		kind string
		objs []*unstructured.Unstructured
	}{{config.KindAgentBackend, l.backends}, {config.KindChatConnection, l.connections}, {config.KindBinding, l.bindings}} {
		for _, u := range group.objs {
			_, _ = fmt.Fprintf(h, "%s %s/%s %s %d\n", group.kind, u.GetNamespace(), u.GetName(), u.GetUID(), u.GetGeneration())
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
