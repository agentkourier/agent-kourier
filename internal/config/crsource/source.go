// Package crsource is the config.Source that reads AgentBackend, ChatConnection and Binding from the custom resources of a
// cluster, through informers of the dynamic client, so that a broker can be configured by objects its teams own (spec D50).
// It lives apart from internal/config so that package keeps importing apimachinery only.
//
// # What it gives
//
// Every object goes through config.DecodeObject, which runs the Default and Validate of the file loader, so a custom resource
// and a file are held to the same checks. They differ in one thing: a field that the broker's types do not have is not a
// reason to refuse a custom resource (see "Unknown fields"). Unlike the files, one bad object is not a bad config: an object that fails to decode or
// validate is left out of the Set and listed in Set.Invalid, and when the object had a good generation, that generation is
// kept in memory and put in the Set in its place, so that a typo in an update does not take a Binding out of routing. A
// Reloader that is PerObject turns the two into what is served and what is reported.
//
// # What it reads
//
// The broker may have an own ChatConnection, named in Options, and is admitted to namespaces by Options.ConnectionNamespaces, a fixed
// list, and Options.ConnectionNamespaceSelector, which matches namespaces by their labels (spec D52, D54). The connections it serves are
// the own, if it has one, and every ChatConnection of an admitted namespace, up to maxServedConnections (see there for the rule); the Set
// holds those and no others. It also holds the Bindings whose connectionRef names a served connection, or names no connection that
// exists (so that some broker reports it). A Binding that names another existing connection, one of a namespace that is not admitted
// among them, belongs to the broker that serves that one, and is not in the Set and not reported here. So is a Binding of a connection
// that the cap leaves out: until a status says so, nobody reports it. Without a list or a selector the served set is the own connection
// alone, and without an own connection either it is every namespace's, as it is with an empty selector (the rule is on
// Options.ConnectionNamespaceSelector). If the own connection is deleted while the broker runs, the Set keeps the last good generation of
// it, and says so in Set.Invalid. A connection of an admitted namespace is held by nothing: it is any other object (see "The hold").
//
// The list is read once, by New. A selector is read live, from an informer on namespaces that keeps each one's name and labels: each
// read admits the namespaces that match it then, so a namespace that gains a matching label is admitted at the next read, and one that
// loses it, or is deleted, is not, its connection and the Bindings on it leaving the Set at that read as another broker's would. The
// source says each such change once, at Info, with the namespace's name. ConnectionBindings still finds the Bindings of a connection
// that is no longer admitted.
//
// # Unknown fields
//
// On this route the API server's schema is the arbiter of what an object may hold, and a broker is routinely older than the CRDs
// of its cluster: CRDs are applied before an upgrade, an image-only rollback leaves them new, and the API server adds a
// field with a default to every stored object it serves, with no change of generation. A broker that refused such a field
// would serve no Binding, and one that refused a field of status or metadata would drop sessions. So decoding ignores a
// field it has no type for, in spec, status and metadata, on every kind; the file route's decoder keeps refusing. Each
// is named in the log, once for each generation of the object: the object and the path of the field, never a value. The
// metric agentkourier_crd_schema_state says that the CRDs and the broker differ. A field the broker does know, with a value it
// cannot read or that Validate refuses, is still a bad object.
//
// # The hold
//
// The hold is for the own connection alone, and a broker with no own connection has no hold. A deleted team connection is not
// held, with or without its Bindings: it stops as any other object does, under the rules below, and its Bindings that are still there
// name a connection that does not exist. What follows, up to "Change detection", is the hold of an own connection; without one the
// deletion rules still apply to every object, and nothing is held.
//
// While the own connection is deleted and held, the Set holds with it, at their last good generation, the Bindings that
// named it and are gone from the cluster too, and the AgentBackends that those Bindings, and the Bindings of it that are
// still there, name and that are gone. Helm deletes the objects it rendered one after another, the backend, then the Bindings
// by name, the connection last, while the old pods still run, so a rollback from custom resources to a ConfigMap takes them
// all away, and a broker that dropped the Bindings would stop answering for as long as it takes the new pods to lead. Each
// held object is in Set.Invalid as Deleted, so that it is counted and says what it is. The hold lasts until the connection is
// back, when what is still gone goes, or until the broker restarts, which has no last good and fails at start as it always
// did.
//
// A read can fall between those deletes, and across a slow API server (a 40 ms round trip, 50 Bindings) it often does. So a
// deletion takes effect only at a read that finds nothing newly gone: Helm's whole run of deletes counts as one change. A read
// that finds an object gone that an earlier read still found there changes nothing: that object, and every one gone before it
// that has not taken effect, stays served at its last good generation as if it were still there, with no word of it in
// Set.Invalid, and the Binding status finds no object behind it, as for a held one. At the first read that finds nothing newly
// gone, and no sooner than settleFloor after the last read that did, they take effect together: they are not served, and are
// kept, not served, for goneGrace more, so that a connection deleted inside that takes them into the hold. A stream of deletions
// cannot keep an object served for ever: one first found gone goneGrace ago takes effect whatever else is found. A connection
// that is gone at any read takes every one of them into the hold at that read. The Source's Fingerprint changes when a
// deletion is due to take effect, so that the Reloader reads again then and not at its slow recheck. The rule bounds the split
// of a deletion and does not close every case: a deletion that goes on for longer than goneGrace between its first object and the
// connection can still lose what went first.
//
// The hold is for the connection and not for a Binding. A Binding deleted alone, while the connection exists, stops at the
// first read, no sooner than settleFloor after the read that found it gone, that finds nothing newly gone: about 10 to 20 s with
// the reads every 10 s, and a stream of other deletions delays it, to goneGrace at most. If its connection is deleted within
// goneGrace of then it comes back, held. While the connection is held a Binding that is deleted stays held, and so stops only
// when the connection is back and the Binding is deleted.
//
// # Change detection
//
// Fingerprint is the sorted uid and metadata.generation of every object, never resourceVersion: a status write bumps
// resourceVersion and not generation, and a source that moved with it would start a reload, a probe pass and a status write
// for every status write. With a selector it is also the sorted names of the admitted namespaces, so that a label that admits a
// namespace, or stops admitting one, starts a reload at the next poll; a write to a namespace that changes no admission (another
// label, a new resourceVersion) does not move it.
//
// Set reads every object from one listing of the three caches, and the admitted namespaces with it, and returns that listing's
// fingerprint: the Set is the state the fingerprint names, however long the decoding takes and whatever the caches do meanwhile.
// That is what lets a config.Reloader that is PerObject publish a read without checking that the source held still while it was
// made: an edit that landed after the listing is not in the Set, and is found at the next poll, which compares the fingerprint of
// the caches with that of the read. A tenant that edits an object in a loop gets, at most, a reload at each poll, each one read behind.
// The three caches are listed one after another, Bindings first, and not at one instant, so a Set can hold objects of
// different kinds from slightly different moments. Each object in it is a whole object of its cache, and what an edit that
// landed between the lists changed is found at the next poll too, as it is for any edit after the listing.
//
// # What a read costs
//
// A read decodes, defaults and validates an object only if the informer has replaced the one the last read decoded: the result
// is kept with the pointer of the cached object it came from (see decodedObject), so a read that finds nothing changed decodes
// nothing, and one that finds an object edited decodes that one. A refusal is kept too. What is kept goes when no read uses the
// object any more. The decoded object is the one that every Set is made from: a Set holds a copy of its struct and shares what it
// holds (strings, slices and maps) with the decoded object and with the Sets before it, so nothing may change a Set's objects, as
// config.Source says; and config.ResolvePartial makes the Snapshot copy the structs of the Bindings and backends it serves, so that
// the Snapshot keeps alive only those and not the slices of the Set. Every AgentBackend of the cluster is decoded, and the decoded
// copy of each is kept, whether or not a Binding names it, and so is that of every Binding the broker owns, among them those that
// name no connection that exists, which every broker owns and none serves. A decoded Binding has no status: the informer holds it.
//
// # RBAC
//
// The ServiceAccount needs list and watch on bindings, agentbackends and chatconnections of agentkourier.dev, in every
// namespace (a ClusterRole), and, with a selector that is not empty, list and watch on namespaces, and nothing else for this
// source; it never issues a get. With no selector, or an empty one ("all"), it reads no namespace. Start lists each resource once
// and opens and closes a watch on it before it starts the informers, so that a missing CRD or a missing right is a clear error at
// start (ErrCRDsAbsent, ErrAccessDenied) and not an informer that retries for ever.
//
// # Staleness
//
// The caches are kept while the API server cannot be reached, which is right: an outage is not every object removed, and for
// the namespace cache not every label removed, so a namespace stays admitted as it was. Stale says that they may lag. Two things
// feed it. A probe lists one object of each kind, and one namespace with a selector, every 30 s, with a 10 s timeout, and
// is the only thing that sees an outage: client-go's reflector retries a refused or throttled watch without telling anyone,
// and turns a timeout or an end of stream into an empty watch, so a cut connection reaches no handler. The probe's verdict
// is exact: it is stale from the first failed probe to the first good one. The informers' own error handler adds what does
// reach it, a list or watch the API server answers with an error status such as forbidden after a Role is revoked, for 90 s
// after the report. The probe is three small lists per 30 s (four with a selector) on the source's own client, so it shares that
// client's rate limiter with the informers: about 0.1 requests a second, against the limit of 5 that client-go gives a client
// that sets none.
package crsource

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
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
	// ErrAccessDenied is wrapped by the error of Start when the API server denies the broker the right to list or watch one of the
	// three resources, or namespaces when a selector reads them (Options.ConnectionNamespaceSelector). It is the operator's to fix:
	// the RBAC of the ServiceAccount.
	ErrAccessDenied = errors.New("the API server denied access to the resources the config source reads")
	// ErrNotSynced is returned by Set while an informer's cache has not synced, since an empty cache would read as every
	// object removed, and by BindingStatus while the Binding informer has not.
	ErrNotSynced = errors.New("the custom resource caches have not synced")
	// ErrStopped is returned by Set and BindingStatus after Stop.
	ErrStopped = errors.New("the custom resource source is stopped")
)

// The three resources, by the plural names the CRDs give them, and namespaces, which a selector reads.
var (
	bindingsGVR    = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "bindings"}
	backendsGVR    = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "agentbackends"}
	connectionsGVR = schema.GroupVersionResource{Group: v1alpha1.Group, Version: v1alpha1.Version, Resource: "chatconnections"}
	namespacesGVR  = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
)

var (
	_ config.Source      = (*Source)(nil)
	_ config.StaleSource = (*Source)(nil)
)

// Options configures a Source. Client is required.
type Options struct {
	// Client reads the resources. In a broker it is the dynamic client of the pod's own ServiceAccount.
	Client dynamic.Interface
	// Connection is the broker's own ChatConnection, as AGENTKOURIER_CHAT_CONNECTION names it. It is always served, and held when
	// deleted. The zero reference, the default, means the broker has none: it serves the ChatConnections of the admitted namespaces
	// alone and holds nothing. A reference with a namespace and no name, or a name and no namespace, is refused by New.
	Connection v1alpha1.ObjectRef
	// ConnectionNamespaces is the platform's admission list (AGENTKOURIER_CONNECTION_NAMESPACES): the Source serves the
	// ChatConnections of these namespaces besides the own, if there is one, up to maxServedConnections in all. Each is a namespace
	// name, and an empty one is ignored. New reads it once.
	ConnectionNamespaces []string
	// ConnectionNamespaceSelector admits, besides the namespaces of ConnectionNamespaces, every namespace whose labels it matches
	// (AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR). It is read live, from an informer on namespaces: a namespace that comes to match
	// is admitted at the next read, and one that stops matching, or is deleted, is not admitted from the next read on. nil, the
	// default, is no selector. An empty selector (labels.Everything(), or what metav1.LabelSelectorAsSelector makes of an empty
	// LabelSelector) admits every namespace, and reads none.
	//
	// With Connection and ConnectionNamespaces it makes the admission rule. The namespaces whose ChatConnections are served are:
	//   - every namespace, when there is no own connection, no list and no selector, or when the selector is empty, whatever else is set;
	//   - the namespaces of the list and those the selector matches, when either is set;
	//   - none, when there is an own connection and nothing else: the own connection is served alone.
	//
	// The own connection, when there is one, is served in every case, and takes a place before every admitted connection. Only a
	// selector that is neither nil nor empty needs the right to list and watch namespaces (see "RBAC").
	ConnectionNamespaceSelector labels.Selector
	// Logger receives the failures of a list or watch. It defaults to slog.Default().
	Logger *slog.Logger
}

// key identifies an object of one kind.
type key struct{ kind, namespace, name string }

// goodObject is the last generation of an object that passed Default and Validate. It holds the object as the informer's cache
// held it, a pointer and not a copy, so that an object that is still the cache's costs nothing more than the cache: only a
// generation that was refused, or an object that is gone, keeps one the cache no longer has. It is read only (nothing may
// change a cached object). typed is the object decoded from it, a pointer to the type of the kind: the very one that the Sets are
// made from (see decodedObject).
type goodObject struct {
	uid        types.UID
	generation int64
	obj        *unstructured.Unstructured
	typed      any
	goneAt     time.Time // when a read first found the object gone, zero while it is there
	settledAt  time.Time // when the deletion took effect, zero while it is deferred (served as if still there) or the object is there
	held       bool      // the hold took it: it stays until the connection is back
}

// decodedObject is what decoding and validating one object of an informer's cache came to, kept so that a read decodes only
// what changed. An object is decoded, defaulted and validated on its own, so the result depends on that object and on this
// binary and on nothing else: not on what it refers to, not on which broker owns it, not on the connection that is held. A
// reference to another object is checked by the resolution, at every read.
//
// The object is told by its pointer, and not by uid and generation: the API server defaults a field onto an object it serves
// when a CRD changes, with no change of generation (and none of resourceVersion), and a cache replaces the object that it
// holds whenever the informer sees it again, as an update or a list. A cached object is never changed, so the same pointer is
// the same object, and src, which this keeps alive, cannot be the address of another.
//
// obj is the decoded object (a pointer to the type of the kind), nil when it was refused. It is the one that goodObject holds
// and that every Set copies the struct of, so a Set shares with it, and with the Sets before it, what the object holds
// (strings, slices, maps): a read of an unchanged object allocates its struct and nothing more, and the cache is not a second
// copy of what is served. Nothing may change a decoded object after it is made, as nothing may change a Snapshot's.
type decodedObject struct {
	src     *unstructured.Unstructured
	obj     any
	unknown []string // the paths of the fields the type does not have, see noteUnknown
	message string   // the problems that refused it, capped, as Invalid.Message says them
	readNo  uint64   // the Set that last used it, so that the objects no Set read are forgotten
}

// goneGrace is how long an object whose deletion has taken effect is kept, not served, so that a connection deleted just after
// it takes it into the hold, and the most that a deletion is deferred behind a stream of others. Helm deletes what it rendered
// one at a time, the AgentBackend and the Bindings before the connection, and the time from the first of them to the last was
// measured at up to 47% of the 10 s read on a 40 ms round trip with 50 Bindings. A minute is six reads and several times the
// longest rollback measured. A deletion that takes longer than this can still split.
const goneGrace = time.Minute

// settleFloor is the least time between the last read that found an object newly gone and the read at which the deletions
// take effect. The Reloader reads every 10 s while the source changes, and also once a minute whether it did or not, and a
// caller may read at any moment: a read that comes a moment after the last must not find "nothing newly gone" while Helm is
// between two deletes. Eight seconds is the 10 s poll less its jitter, so that the next regular read settles and an extra one
// does not; Helm's run was measured at up to 5 s, and one that goes on longer finds something newly gone at the next read, which
// starts the wait again.
const settleFloor = 8 * time.Second

// maxServedConnections is the most ChatConnections the broker serves, its own included. The own connection, if the broker has one, is
// always served and counts first, and takes one of the places. When every namespace is admitted the cap is cluster-wide: the
// connections of the whole cluster compete for the places. The ChatConnections of the admitted namespaces are ordered by
// creationTimestamp (the oldest first), then namespace, then name (creationTimestamp has a resolution of one second, so connections
// made together tie and fall to namespace and name), and the first maxServedConnections-1 are served with an own connection, the first
// maxServedConnections without. An invalid connection counts too, so that which
// connections are served depends on the objects' ages and names and not on their specs. A connection past the limit is not served:
// the Set lists it in UnservedConnections with the reason config.UnservedCap, the Bindings that name it are not read (they are
// nobody's, until a status says so), and the source says once at Warn that it was left out and once at Info when it is served
// again, which it is when a connection ahead of it goes away. A deleted connection keeps its place until its deletion takes effect
// (it is served until then), so the limit holds at every read. The value is fixed in code and no setting changes it: it is 16 for
// now, and the measurement of what a served connection costs (memory and goroutines each) may revise it.
const maxServedConnections = 16

// Source is a config.Source of the custom resources. Start it, and Stop it when done.
type Source struct {
	client    dynamic.Interface
	own       key       // the broker's own ChatConnection, the zero key when it has none (hasOwn)
	hasOwn    bool      // the broker has an own connection: it is served first, held when deleted, and takes one of the places
	admission admission // which namespaces' ChatConnections are served besides the own (Options.ConnectionNamespaces and ConnectionNamespaceSelector)
	log       *slog.Logger
	ctx       context.Context
	cancel    context.CancelFunc

	mu       sync.Mutex // guards the fields below
	factory  dynamicinformer.DynamicSharedInformerFactory
	stores   [3]cache.Store // bindings, backends, connections
	synced   [3]cache.InformerSynced
	nsStore  cache.Store // namespaces, only when admission.selector reads them
	nsSynced cache.InformerSynced
	selected map[string]bool // the namespaces that the last Set admitted by the selector alone, so that the log says each change once
	good     map[key]goodObject
	decoded  map[key]decodedObject    // what decoding each object that a Set read came to, see decodedObject
	decodes  int                      // how many objects have been decoded, which a test counts
	readNo   uint64                   // the Set being read
	warned   map[key]warnedGeneration // the generation of each object whose unknown fields the log has named
	started  bool

	bindingEvents atomic.Uint64 // the events the Binding informer has delivered (BindingEvents)

	errMu    sync.Mutex
	lastErr  error     // the latest failure of a list or watch of an informer, from its error handler
	errAt    time.Time // when it was reported
	probeErr error     // the latest probe's failure, nil once a probe passes
	now      func() time.Time

	probeEvery, probeTimeout time.Duration
	holding                  bool           // the own connection is deleted and held, with what went with it
	capped                   map[key]bool   // the connections the last read left out under maxServedConnections, so that the log says each once
	lastGone                 time.Time      // when a read last found an object gone that an earlier read found there
	probeExited              func()         // a test's hook, called as the probe's goroutine ends
	wg                       sync.WaitGroup // the probe
}

// New returns a Source that has read nothing. It refuses Options without a Client, and a Connection that names a namespace and no name
// or a name and no namespace; a Connection that names neither is no own connection (see Options.Connection).
func New(opts Options) (*Source, error) {
	hasOwn := opts.Connection != (v1alpha1.ObjectRef{})
	switch {
	case opts.Client == nil:
		return nil, errors.New("crsource: Options.Client is required")
	case hasOwn && (opts.Connection.Name == "" || opts.Connection.Namespace == ""):
		return nil, errors.New("crsource: Options.Connection must name a ChatConnection as namespace and name, or neither")
	}
	s := &Source{
		client: opts.Client, log: cmp.Or(opts.Logger, slog.Default()), good: map[key]goodObject{}, decoded: map[key]decodedObject{}, warned: map[key]warnedGeneration{},
		now: time.Now, probeEvery: probeEvery, probeTimeout: probeTimeout, capped: map[key]bool{},
		hasOwn: hasOwn, admission: newAdmission(hasOwn, opts.ConnectionNamespaces, opts.ConnectionNamespaceSelector),
	}
	if hasOwn {
		s.own = key{config.KindChatConnection, opts.Connection.Namespace, opts.Connection.Name}
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// isOwn says whether k is the broker's own ChatConnection, which is no key at all when the broker has none.
func (s *Source) isOwn(k key) bool { return s.hasOwn && k == s.own }

// admission is the rule that admits the ChatConnections of a namespace to the served set (see Options.ConnectionNamespaceSelector):
// every namespace, or those of list and those whose labels selector matches. New fixes it; what the selector matches is read from the
// namespace informer at each listing.
type admission struct {
	all      bool
	list     map[string]bool
	selector labels.Selector // nil when no selector reads labels: none was given, or it was empty and all is set
}

// newAdmission makes the rule of the options. It reads no namespace.
func newAdmission(hasOwn bool, namespaces []string, selector labels.Selector) admission {
	a := admission{list: map[string]bool{}}
	for _, ns := range namespaces {
		if ns != "" {
			a.list[ns] = true
		}
	}
	switch {
	case selector != nil && selector.Empty():
		a.all = true
	case selector != nil:
		a.selector = selector
	case !hasOwn && len(a.list) == 0:
		a.all = true
	}
	return a
}

// resourceName is how errors and the log name a resource: its plural and group, or its plural alone for one of the core group.
func resourceName(gvr schema.GroupVersionResource) string {
	if gvr.Group == "" {
		return gvr.Resource
	}
	return gvr.Resource + "." + gvr.Group
}

// Start lists each of the three resources once, and namespaces when a selector reads them, so that a missing CRD or a missing right is
// an error that says so, then starts an informer for each and waits for its cache to sync. ctx bounds the wait only: the informers run
// until Stop. On an error nothing is left to wait for, but Stop must still be called. Call Start once.
func (s *Source) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("crsource: Start was called twice")
	}
	s.started = true
	s.mu.Unlock()

	gvrs := [3]schema.GroupVersionResource{bindingsGVR, backendsGVR, connectionsGVR}
	probed := gvrs[:]
	if s.admission.selector != nil {
		probed = append(slices.Clone(probed), namespacesGVR)
	}
	for _, gvr := range probed {
		if err := s.probe(ctx, gvr); err != nil {
			return err
		}
	}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(s.client, 0, metav1.NamespaceAll, nil)
	s.mu.Lock()
	for i, gvr := range gvrs {
		inf := factory.ForResource(gvr).Informer()
		// The informer holds each object it lists, for as long as it runs, so what the broker never reads is dropped as it comes in.
		_ = inf.SetTransform(slim)
		// The handler replaces client-go's, which logs through klog. The only error is an informer that has started.
		_ = inf.SetWatchErrorHandler(s.watchErrorHandler(gvr))
		s.stores[i], s.synced[i] = inf.GetStore(), inf.HasSynced
		if i == 0 { // the Bindings: BindingEvents tells a reader of their statuses that one moved
			count := func(...any) { s.bindingEvents.Add(1) }
			_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
				AddFunc: func(any) { count() }, UpdateFunc: func(any, any) { count() }, DeleteFunc: func(any) { count() },
			})
		}
	}
	waitFor := s.synced[:]
	if s.admission.selector != nil {
		// Beside the three, and only with a selector: "all" and a list read no namespace, and need no right to.
		inf := factory.ForResource(namespacesGVR).Informer()
		_ = inf.SetTransform(slimNamespace)
		_ = inf.SetWatchErrorHandler(s.watchErrorHandler(namespacesGVR))
		s.nsStore, s.nsSynced = inf.GetStore(), inf.HasSynced
		waitFor = append(slices.Clone(waitFor), inf.HasSynced)
	}
	s.factory = factory
	s.mu.Unlock()
	factory.Start(s.ctx.Done())
	s.wg.Go(func() { s.probeLoop(s.ctx, probed) })
	syncStop := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { close(syncStop) })
	defer stop()
	if !cache.WaitForCacheSync(syncStop, waitFor...) {
		return fmt.Errorf("%w: %w", ErrNotSynced, cmp.Or(context.Cause(ctx), s.ctx.Err(), errors.New("stopped")))
	}
	return nil
}

// watchErrorHandler is the error handler of the informer of gvr: it records the failure for Stale and says it at Warn.
func (s *Source) watchErrorHandler(gvr schema.GroupVersionResource) cache.WatchErrorHandler {
	return func(_ *cache.Reflector, err error) {
		s.errMu.Lock()
		s.lastErr, s.errAt = fmt.Errorf("%s: %w", resourceName(gvr), err), s.now()
		s.errMu.Unlock()
		s.log.Warn("custom resource informer cannot list or watch", "resource", gvr.Resource, "error", err)
	}
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
	case apierrors.IsNotFound(err) && gvr.Group == v1alpha1.Group:
		return fmt.Errorf("%w: the API server does not serve %s %s; install the CRDs of Agent Kourier first: %w", ErrCRDsAbsent, resourceName(gvr), gvr.Version, err)
	case (apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err)) && gvr == namespacesGVR:
		return fmt.Errorf("%w: namespaces: the ServiceAccount needs list and watch on namespaces, which the connection namespace selector reads: %w", ErrAccessDenied, err)
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return fmt.Errorf("%w: %s: the ServiceAccount needs list and watch on it in every namespace: %w", ErrAccessDenied, resourceName(gvr), err)
	}
	return fmt.Errorf("list or watch %s: %w", resourceName(gvr), err)
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

// probeLoop lists one object of each kind every probeEvery (and one namespace, when a selector reads them), until the source is
// stopped, and records whether the API server answered. A list is what a refused connection, a timeout and a throttled request
// all fail, which the informers' own error handler is never told of.
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
				failed = fmt.Errorf("%s: %w", resourceName(gvr), err)
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

// Fingerprint is a digest of the uid and generation of every object, with a mark while a deletion that a read deferred is due to
// take effect. It is "!" and why while the caches have not synced or the source is stopped, so that a state that cannot be read
// looks the same on every poll.
func (s *Source) Fingerprint(context.Context) string {
	l, err := s.list()
	if err != nil {
		return "!" + err.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fingerprintLocked(l, s.clock())
}

// BindingStatus is what the cache of the Binding informer holds of one Binding's status: the object it is the status of, and
// its conditions as written.
type BindingStatus struct {
	// UID and Generation are the cached object's, so that a status can be told from that of an object of the same name that
	// was deleted and made again.
	UID        types.UID
	Generation int64
	// ResourceVersion is the cached object's, which moves with each change the cache has seen (opaque: only compared for equal).
	// A reader that finds it as it was knows the cache has seen nothing of the object since.
	ResourceVersion string
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
	return BindingStatus{UID: u.GetUID(), Generation: u.GetGeneration(), ResourceVersion: u.GetResourceVersion(), Conditions: ConditionsOf(u)}, true, nil
}

// ConnectionBindings is what ConnectionBindings finds of one ChatConnection.
type ConnectionBindings struct {
	// Exists says whether the ChatConnection exists, whether or not the broker serves it.
	Exists bool
	// Bindings is the "namespace/name" of each Binding that names it, sorted, an invalid one too.
	Bindings []string
	// Admitted is the part of Bindings that the connection allows: those of its own namespace, and those of a namespace its
	// spec.allowedNamespaces lists. Only these may be told anything of the connection (a Binding of another namespace learns no more of
	// an object than that it exists and allows it).
	Admitted []string
}

// ConnectionBindings reads the ChatConnection namespace/name and the Bindings that name it from the caches, whether or not the broker
// serves it, so the connection need not be one the Set lists: it is how the broker finds the Bindings of a connection it served before
// and does not now (a namespace taken off the admission list, or no longer matched by the selector), which no read of the Set names. A
// Binding that is invalid still says which connection it means, as for UnservedConnection.Bindings. It fails with ErrNotSynced until
// the caches have synced, and with ErrStopped after Stop.
func (s *Source) ConnectionBindings(namespace, name string) (ConnectionBindings, error) {
	l, err := s.list()
	if err != nil {
		return ConnectionBindings{}, err
	}
	want := key{config.KindChatConnection, namespace, name}
	out := ConnectionBindings{Exists: l.hasConnection[want]}
	out.Bindings, out.Admitted = l.bindingsOf(want)
	return out, nil
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

// Set reads the objects from one listing of the caches (see "Change detection"), decodes each, and returns them with the
// fingerprint of that listing. See the package comment for what it leaves out and what it keeps. It fails only when the caches
// have not synced or the source is stopped. Each call returns a Set of its own, whose slices are its own and whose objects share
// what they hold with the source's decoded objects: see "What a read costs".
func (s *Source) Set(context.Context) (*config.Set, string, error) {
	l, err := s.list()
	if err != nil {
		return nil, "!" + err.Error(), err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set := &config.Set{}
	live := map[key]bool{}
	s.readNo++
	s.noteSelected(l.selected)
	served := s.selectServed(l)
	set.UnservedConnections = served.left
	s.noteCapped(served)
	read(s, config.KindAgentBackend, l.backends, live, func(*unstructured.Unstructured) bool { return true }, &set.AgentBackends, &set.Invalid)
	read(s, config.KindChatConnection, l.connections, live,
		func(u *unstructured.Unstructured) bool { return served.keys[keyOf(config.KindChatConnection, u)] }, &set.ChatConnections, &set.Invalid)
	read(s, config.KindBinding, l.bindings, live, func(u *unstructured.Unstructured) bool { return s.owns(l, served, u) }, &set.Bindings, &set.Invalid)
	for k := range s.warned { // what is not read now has no generation to be told of again
		if !live[k] {
			delete(s.warned, k)
		}
	}
	for k, d := range s.decoded { // nor a decoding to keep: an object that no read used is gone, or is another broker's
		if d.readNo != s.readNo {
			delete(s.decoded, k)
		}
	}

	// The broker's own connection is held when it is deleted: a running broker keeps its last good one, and with it what
	// the connection's Bindings and their backends were (see the package comment). A broker with no own connection holds nothing.
	var ownConn *v1alpha1.ChatConnection
	var ownGood goodObject
	if s.hasOwn && !live[s.own] {
		if g, ok := s.good[s.own]; ok {
			ownGood = g
			ownConn = typedOf[v1alpha1.ChatConnection](g)
		}
	}
	now := s.clock()
	s.markGone(l, live, now)
	s.forget(l, live, now, ownConn == nil)
	reselect := s.placeFreed(served)
	if ownConn != nil {
		set.ChatConnections = append(set.ChatConnections, *ownConn)
		set.Invalid = append(set.Invalid, config.Invalid{Kind: s.own.kind, Namespace: s.own.namespace, Name: s.own.name, Served: ownGood.generation, Deleted: true})
		s.holdStarted(s.hold(l, live, set))
	} else {
		s.serveDeferred(l, served, set)
		if s.holding {
			s.holding = false
			s.log.Info("the ChatConnection of the broker is back; what was held with it and is still gone is gone")
		}
	}
	fp := s.fingerprintLocked(l, now)
	if reselect {
		// A deletion took effect at this read and freed a place for a connection that the cap left out, which the next read serves.
		// Fingerprint does not carry the mark, so the Reloader's next poll finds the state changed and reads again, where it
		// would otherwise wait for its recheck.
		fp += "|reselect"
	}
	return set, fp, nil
}

// fingerprintLocked is the digest of the listing, and a mark if a deletion is due to take effect, so that the Reloader reads
// again at its next poll and the deletion takes effect then, not at its slow recheck. The caller holds s.mu.
func (s *Source) fingerprintLocked(l listing, now time.Time) string {
	fp := l.fingerprint()
	if s.settleDue(l, now) {
		fp += "|settle"
	}
	return fp
}

// settleDue says whether a deletion that a read deferred is due to take effect: the floor has passed since the last read that
// found something newly gone, or the object has been gone goneGrace. The caller holds s.mu.
func (s *Source) settleDue(l listing, now time.Time) bool {
	for k, g := range s.good {
		if s.isOwn(k) || g.held || g.goneAt.IsZero() || !g.settledAt.IsZero() {
			continue
		}
		if _, listed := l.keys[k]; listed {
			continue
		}
		if now.Sub(g.goneAt) >= goneGrace || now.Sub(s.lastGone) >= settleFloor {
			return true
		}
	}
	return false
}

// markGone dates the first read that finds an object of the last good gone, and returns how many it found gone for the first
// time. Such a read changes nothing: see forget. The caller holds s.mu.
func (s *Source) markGone(l listing, live map[key]bool, now time.Time) {
	newly := 0
	for k, g := range s.good {
		if s.isOwn(k) || live[k] || !g.goneAt.IsZero() {
			continue
		}
		if _, listed := l.keys[k]; listed {
			continue
		}
		g.goneAt = now
		s.good[k] = g
		newly++
	}
	if newly > 0 {
		s.lastGone = now
	}
}

// serveDeferred adds to set, at their last good generation and with no word in set.Invalid, the Bindings, AgentBackends and team
// ChatConnections (those of served, which counted their places) that a read found gone and whose deletion has not taken effect:
// they are served as if they were still there. The caller holds s.mu.
func (s *Source) serveDeferred(l listing, served servedConnections, set *config.Set) {
	for _, k := range slices.SortedFunc(maps.Keys(s.good), compareKeys) {
		g := s.good[k]
		if s.isOwn(k) || g.held || g.goneAt.IsZero() || !g.settledAt.IsZero() {
			continue
		}
		if _, listed := l.keys[k]; listed {
			continue
		}
		switch k.kind {
		case config.KindBinding:
			if b := typedOf[v1alpha1.Binding](g); b != nil {
				set.Bindings = append(set.Bindings, *b)
			}
		case config.KindAgentBackend:
			if be := typedOf[v1alpha1.AgentBackend](g); be != nil {
				set.AgentBackends = append(set.AgentBackends, *be)
			}
		case config.KindChatConnection:
			if c := typedOf[v1alpha1.ChatConnection](g); c != nil && served.keys[k] {
				set.ChatConnections = append(set.ChatConnections, *c)
			}
		}
	}
}

// forget drops from the last good what is not there any more and is not held, and settles the deletions that are due. An object
// that is listed and was not read as the broker's (it went to another broker, or was refused away) is dropped at once. One that
// is not listed is gone: its deletion takes effect, and it is marked so, at the first read that finds nothing newly gone, no
// sooner than settleFloor after the last read that did (a read that finds something newly gone makes lastGone that moment, so
// the floor is not passed), or when it has been gone goneGrace whatever else is found; it is dropped goneGrace after that, and until then a connection deleted takes it into the hold. One that the hold took
// is dropped when the connection is back (ownLive), and not before. Until its deletion takes effect an object is served, by
// serveDeferred, as if it were there. The caller holds s.mu.
func (s *Source) forget(l listing, live map[key]bool, now time.Time, ownLive bool) {
	for k, g := range s.good {
		if s.isOwn(k) {
			continue
		}
		if _, listed := l.keys[k]; listed {
			switch {
			case !live[k]:
				delete(s.good, k)
			case !g.goneAt.IsZero(): // it is back, as an object that was refused: what was gone is no longer
				g.goneAt, g.settledAt, g.held = time.Time{}, time.Time{}, false
				s.good[k] = g
			}
			continue
		}
		switch {
		case g.held && ownLive:
			delete(s.good, k)
		case g.held:
		case g.settledAt.IsZero():
			if now.Sub(g.goneAt) >= goneGrace || now.Sub(s.lastGone) >= settleFloor {
				g.settledAt = now
				s.good[k] = g
			}
		case now.Sub(g.settledAt) >= goneGrace:
			delete(s.good, k)
		}
	}
}

// clock is the source's time, which a test replaces.
func (s *Source) clock() time.Time {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.now()
}

// holdStarted says, once, that a hold has begun: how many Bindings and AgentBackends it holds, and how many of the held
// Bindings name a backend that is not in the set (gone and not held, or refused with no last good generation), which are
// not served.
func (s *Source) holdStarted(bindings, backends, withoutBackend int) {
	if s.holding {
		return
	}
	s.holding = true
	s.log.Warn("the ChatConnection of the broker was deleted: it is held at its last good generation, with the Bindings of it that are gone "+
		"and the AgentBackends that they and the Bindings of it that are still there use and that are gone, until the connection is back "+
		"or the broker restarts; while it is held a Binding is stopped by bringing the connection back and then deleting the Binding",
		"connection", s.own.namespace+"/"+s.own.name, "held_bindings", bindings, "held_backends", backends, "held_without_backend", withoutBackend)
}

// hold adds to set the Bindings that named the own connection and are gone from the cluster, and the AgentBackends that
// they, and the Bindings of the connection that are still there, name and that are gone, each at its last good generation
// and listed as Deleted in set.Invalid, and marks them live so that they are not forgotten. Gone is absent from the listing,
// which includes what a read found gone a moment ago (see goneGrace): an object that is there, whoever's it is now, is read as
// it always is, and a Binding made again has its new uid and generation, never a held one. It returns how many Bindings and
// backends it holds, and how many held Bindings name a backend that is not in the set. The caller holds s.mu.
func (s *Source) hold(l listing, live map[key]bool, set *config.Set) (bindings, backends, withoutBackend int) {
	gone := func(k key, kind string) bool { _, listed := l.keys[k]; return k.kind == kind && !listed && !live[k] }
	wanted := map[key]bool{}
	var heldBindings []key
	for _, k := range slices.SortedFunc(maps.Keys(s.good), compareKeys) {
		g := s.good[k]
		if !gone(k, config.KindBinding) {
			continue
		}
		if refOf(config.KindChatConnection, g.obj, "spec", "chat", "connectionRef") != s.own {
			continue
		}
		b := typedOf[v1alpha1.Binding](g)
		if b == nil {
			continue
		}
		live[k] = true
		g.held = true
		s.good[k] = g
		set.Bindings = append(set.Bindings, *b)
		set.Invalid = append(set.Invalid, config.Invalid{Kind: k.kind, Namespace: k.namespace, Name: k.name, Served: g.generation, Deleted: true})
		heldBindings = append(heldBindings, k)
		bindings++
	}
	// The backends that a Binding of the connection names, held or still there: a tenant's Binding on a backend that was
	// rendered with the release is what the hold is for.
	for _, b := range set.Bindings {
		if b.Spec.Chat.ConnectionRef.NamespaceOr(b.Namespace) == s.own.namespace && b.Spec.Chat.ConnectionRef.Name == s.own.name {
			wanted[key{config.KindAgentBackend, b.Spec.Agent.BackendRef.NamespaceOr(b.Namespace), b.Spec.Agent.BackendRef.Name}] = true
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(wanted), compareKeys) {
		g, ok := s.good[k]
		if !ok || !gone(k, config.KindAgentBackend) {
			continue
		}
		be := typedOf[v1alpha1.AgentBackend](g)
		if be == nil {
			continue
		}
		live[k] = true
		g.held = true
		s.good[k] = g
		set.AgentBackends = append(set.AgentBackends, *be)
		set.Invalid = append(set.Invalid, config.Invalid{Kind: k.kind, Namespace: k.namespace, Name: k.name, Served: g.generation, Deleted: true})
		backends++
	}
	have := map[key]bool{}
	for _, be := range set.AgentBackends {
		have[key{config.KindAgentBackend, be.Namespace, be.Name}] = true
	}
	for _, k := range heldBindings {
		if !have[refOf(config.KindAgentBackend, s.good[k].obj, "spec", "agent", "backendRef")] {
			withoutBackend++
		}
	}
	return bindings, backends, withoutBackend
}

// refOf returns the object of the kind that the Binding raw names at path (spec.chat.connectionRef, spec.agent.backendRef):
// in the Binding's own namespace when the reference names none.
func refOf(kind string, raw *unstructured.Unstructured, path ...string) key {
	ns, _, _ := unstructured.NestedString(raw.Object, append(path, "namespace")...)
	name, _, _ := unstructured.NestedString(raw.Object, append(path, "name")...)
	ref := v1alpha1.ObjectRef{Namespace: ns, Name: name}
	return key{kind, ref.NamespaceOr(raw.GetNamespace()), ref.Name}
}

func compareKeys(a, b key) int {
	return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.namespace, b.namespace), cmp.Compare(a.name, b.name))
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
		same := hasGood && (g.uid == u.GetUID() || s.isOwn(k))
		mine := owns(u)
		if !mine && !same {
			continue
		}
		d := decodeCached[T, PT](s, k, u)
		s.noteUnknown(k, u, d.unknown)
		if obj, _ := d.obj.(*T); obj != nil {
			if !mine { // a valid edit to another broker's connection: the Binding is that broker's now, and the pruning forgets it
				continue
			}
			live[k] = true
			s.good[k] = goodObject{uid: u.GetUID(), generation: u.GetGeneration(), obj: u, typed: obj}
			*dst = append(*dst, *obj)
			continue
		}
		if !mine { // refused, and its own words say it is another broker's: the last good generation says otherwise
			if !owns(g.obj) {
				continue
			}
		}
		live[k] = true
		if hasGood && !same { // another object of the name than the one that was last good: that one is gone for good
			delete(s.good, k)
		}
		inv := config.Invalid{Kind: kind, Namespace: u.GetNamespace(), Name: u.GetName(), Generation: u.GetGeneration(), Message: d.message}
		if kind != config.KindBinding {
			inv.AllowedNamespaces, _, _ = unstructured.NestedStringSlice(u.Object, "spec", "allowedNamespaces")
		}
		if same {
			if last := typedOf[T](g); last != nil {
				*dst = append(*dst, *last)
				inv.Served = g.generation
			}
		}
		*invalid = append(*invalid, inv)
	}
}

// decodeObject is config.DecodeObject of the JSON of u.
func decodeObject[T any, PT interface {
	*T
	Default()
	Validate() error
}](u *unstructured.Unstructured) (*T, []string, []error) {
	j, err := u.MarshalJSON()
	if err != nil {
		return nil, nil, []error{err}
	}
	return config.DecodeObject[T, PT](j)
}

// typedOf returns the decoded object of the last good generation g, or nil if it holds none.
func typedOf[T any](g goodObject) *T {
	t, _ := g.typed.(*T)
	return t
}

// decodeCached decodes u as T, unless a read already decoded this very object, which it answers from: see
// decodedObject. The caller holds s.mu.
func decodeCached[T any, PT interface {
	*T
	Default()
	Validate() error
}](s *Source, k key, u *unstructured.Unstructured) decodedObject {
	if d, ok := s.decoded[k]; ok && d.src == u {
		d.readNo = s.readNo
		s.decoded[k] = d
		return d
	}
	s.decodes++
	d := decodedObject{src: u, readNo: s.readNo}
	obj, unknown, errs := decodeObject[T, PT](u)
	d.unknown = unknown
	if obj == nil {
		d.message = config.CapMessage(joinErrors(errs))
	} else {
		if b, ok := any(obj).(*v1alpha1.Binding); ok {
			// A Binding's status is the informer's to hold, and the status writer reads it there (BindingStatus). The Set's
			// Binding is read for its spec, and nothing reads a status from it. A status kept here would only cost memory, up to
			// the CRD's bounds, and for the last good typed object that a refused generation is served from, which is never replaced
			// by a status write, it would be the status the object had when it was last good.
			b.Status = v1alpha1.BindingStatus{}
		}
		d.obj = obj
	}
	s.decoded[k] = d
	return d
}

// slimmed lists the fields of metadata that the informers do not keep. The API server limits none of them in total beyond the
// size of the object itself, so a namespace writer who may create these objects can grow every broker's cache with them:
// managedFields, which it keeps for every manager that has written the object; annotations, among them
// kubectl.kubernetes.io/last-applied-configuration, which a client-side apply makes a second copy of the spec; labels,
// ownerReferences and finalizers.
var slimmed = [...]string{"managedFields", "annotations", "labels", "ownerReferences", "finalizers"}

// slim is the informers' transform. It drops from each object, before it is cached, the fields of slimmed, which no code of the
// broker reads: the fingerprint is the uid and generation, a decoded object is read by name, namespace, generation, uid and creation
// time (which orders the Bindings that claim a channel), a status is written from the Binding's name alone, and a Helm or Argo
// annotation or label is for those tools. What is dropped is not in a decoded object either, so an edit of nothing but those fields
// changes neither the fingerprint (the API server does not move metadata.generation for it) nor the Set. The rest of metadata
// stays, so that a field of it that this broker has no type for is still named in the log (see "Unknown fields").
func slim(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	if meta, ok := u.Object["metadata"].(map[string]any); ok {
		for _, f := range slimmed {
			delete(meta, f)
		}
	}
	return obj, nil
}

// slimNamespace is the namespace informer's transform. A namespace is read for its name and labels alone, which the selector
// matches, so the cache holds those, with the uid and resourceVersion that tell one version from another, and nothing else.
func slimNamespace(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	out := &unstructured.Unstructured{Object: map[string]any{}}
	out.SetAPIVersion(u.GetAPIVersion())
	out.SetKind(u.GetKind())
	out.SetName(u.GetName())
	out.SetUID(u.GetUID())
	out.SetResourceVersion(u.GetResourceVersion())
	if l := u.GetLabels(); len(l) > 0 {
		out.SetLabels(l)
	}
	return out, nil
}

// warnedGeneration is the object, by uid, and the generation of it whose unknown fields the log has named.
type warnedGeneration struct {
	uid        types.UID
	generation int64
}

// maxNamedFields bounds how many unknown fields one log line names.
const maxNamedFields = 10

// noteUnknown says, once for each generation of an object, which fields of it this broker has no type for. The caller holds
// s.mu. The API server's schema decides what an object may hold, and a broker that is older than the CRDs of its cluster
// meets fields it does not know: a later release adds a field and the API server defaults it onto every stored object, with
// no change of generation. They are ignored and are no reason to refuse the object. The line names the object and the paths
// of the fields, and never a value.
func (s *Source) noteUnknown(k key, u *unstructured.Unstructured, fields []string) {
	if len(fields) == 0 {
		delete(s.warned, k)
		return
	}
	w := warnedGeneration{u.GetUID(), u.GetGeneration()}
	if s.warned[k] == w {
		return
	}
	s.warned[k] = w
	named := fields
	if len(named) > maxNamedFields {
		named = named[:maxNamedFields]
	}
	s.log.Warn("a custom resource has fields this broker does not know, and ignores them; the CRDs of the cluster are probably newer than "+
		"this broker, which agentkourier_crd_schema_state shows when they carry another schema hash",
		"kind", k.kind, "namespace", k.namespace, "name", k.name, "generation", u.GetGeneration(),
		"fields", strings.Join(named, ", "), "unknown_fields", len(fields))
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

// owns reports whether the Binding u is the broker's to serve or to report: its connectionRef names a connection the broker serves, or
// names no connection that exists. One that names another existing connection is that connection's broker's, and so is one that
// names a connection the cap left out, which no broker reports until a status says so. The reference is read from the raw object,
// since a Binding that is invalid may still say which connection it means.
func (s *Source) owns(l listing, served servedConnections, u *unstructured.Unstructured) bool {
	ref := refOf(config.KindChatConnection, u, "spec", "chat", "connectionRef")
	return served.keys[ref] || !l.hasConnection[ref]
}

// servedConnections is what one read serves of the ChatConnections: the keys of the own connection, if there is one, and of the team connections that
// fit under maxServedConnections, and the ones that exist and do not fit.
type servedConnections struct {
	keys     map[key]bool
	left     []config.UnservedConnection
	deferred []key // the deleted connections that still took a place in the choice
}

// selectServed chooses the connections to serve: the own, if there is one, and the ChatConnections of the namespaces that the listing
// admits, in the order of maxServedConnections, as many as there are places. A team connection that was deleted and whose deletion has
// not taken effect still takes its place while its namespace is admitted, since it is served until then (serveDeferred); the deletion
// that takes effect at this read frees its place at the next. The caller holds s.mu.
func (s *Source) selectServed(l listing) servedConnections {
	type candidate struct {
		k       key
		created time.Time
		listed  bool
	}
	var cands []candidate
	for _, u := range l.connections {
		if k := keyOf(config.KindChatConnection, u); !s.isOwn(k) && l.admits(k.namespace) {
			cands = append(cands, candidate{k, u.GetCreationTimestamp().Time, true})
		}
	}
	for k, g := range s.good {
		// One whose namespace is no longer admitted is another broker's, gone or not: it is not served, and takes no place.
		if _, listed := l.keys[k]; k.kind == config.KindChatConnection && !s.isOwn(k) && !listed && g.settledAt.IsZero() && l.admits(k.namespace) {
			cands = append(cands, candidate{k, g.obj.GetCreationTimestamp().Time, false})
		}
	}
	slices.SortFunc(cands, func(a, b candidate) int {
		return cmp.Or(a.created.Compare(b.created), cmp.Compare(a.k.namespace, b.k.namespace), cmp.Compare(a.k.name, b.k.name))
	})
	out := servedConnections{keys: map[key]bool{}}
	places := maxServedConnections
	if s.hasOwn {
		out.keys[s.own] = true
		places--
	}
	for i, c := range cands {
		if !c.listed {
			out.deferred = append(out.deferred, c.k)
		}
		switch {
		case i < places:
			out.keys[c.k] = true
		case c.listed:
			out.left = append(out.left, config.UnservedConnection{Namespace: c.k.namespace, Name: c.k.name, Reason: config.UnservedCap,
				Message: fmt.Sprintf("the broker serves at most %d chat connections", maxServedConnections)})
		}
	}
	if len(out.left) > 0 {
		out.nameBindings(l)
	}
	return out
}

// nameBindings gives each left-out connection the Bindings that name it, sorted, read from the raw objects: a Binding that is
// invalid may still say which connection it means. They are in no Set, and the status and the retention of sessions need them.
func (sc *servedConnections) nameBindings(l listing) {
	for i := range sc.left {
		sc.left[i].Bindings, sc.left[i].Admitted = l.bindingsOf(key{config.KindChatConnection, sc.left[i].Namespace, sc.left[i].Name})
	}
}

// bindingsOf returns the Bindings that name the connection conn, sorted, and those of them that the connection admits: its own
// namespace's, and the namespaces of its spec.allowedNamespaces, read from the raw object, which is readable when it is invalid.
func (l listing) bindingsOf(conn key) (bindings, admitted []string) {
	var allowed []string
	for _, u := range l.connections {
		if keyOf(config.KindChatConnection, u) == conn {
			allowed, _, _ = unstructured.NestedStringSlice(u.Object, "spec", "allowedNamespaces")
			break
		}
	}
	for _, u := range l.bindings {
		if refOf(config.KindChatConnection, u, "spec", "chat", "connectionRef") != conn {
			continue
		}
		name := u.GetNamespace() + "/" + u.GetName()
		bindings = append(bindings, name)
		if config.ReferenceAllowed(u.GetNamespace(), conn.namespace, allowed) {
			admitted = append(admitted, name)
		}
	}
	return bindings, admitted
}

// placeFreed says whether this read let a deleted connection's deletion take effect while a connection is left out under the cap:
// the deleted one still took its place in the choice, so the left-out one waits for the next read. The caller holds s.mu, after forget.
func (s *Source) placeFreed(served servedConnections) bool {
	if len(served.left) == 0 {
		return false
	}
	return slices.ContainsFunc(served.deferred, func(k key) bool {
		g, ok := s.good[k]
		return !ok || !g.settledAt.IsZero()
	})
}

// noteCapped says, once, that a connection is left out under maxServedConnections, at Warn, and, once, that one that was left out is
// served again, at Info. The caller holds s.mu.
func (s *Source) noteCapped(served servedConnections) {
	now := make(map[key]bool, len(served.left))
	for _, u := range served.left {
		k := key{config.KindChatConnection, u.Namespace, u.Name}
		now[k] = true
		if !s.capped[k] {
			s.log.Warn("a ChatConnection is left out: the broker serves at most so many chat connections, its own if it has one and then the oldest of the admitted namespaces, "+
				"and this one is not among them; its Bindings are not served until a place is free",
				"connection", u.Namespace+"/"+u.Name, "limit", maxServedConnections)
		}
	}
	for k := range s.capped {
		if !now[k] && served.keys[k] {
			s.log.Info("a ChatConnection that the limit kept out is served again", "connection", k.namespace+"/"+k.name, "limit", maxServedConnections)
		}
	}
	s.capped = now
}

// noteSelected says, once, at Info, that a namespace is admitted by the selector alone (it is not on the list), and, once, that one
// that was is not any more, by name. The caller holds s.mu.
func (s *Source) noteSelected(selected []string) {
	if s.admission.selector == nil {
		return
	}
	now := make(map[string]bool, len(selected))
	for _, ns := range selected {
		now[ns] = true
		if !s.selected[ns] {
			s.log.Info("a namespace is admitted by the connection namespace selector: its ChatConnections are served, up to the limit", "namespace", ns)
		}
	}
	for _, ns := range slices.Sorted(maps.Keys(s.selected)) {
		if !now[ns] {
			s.log.Info("a namespace is no longer admitted by the connection namespace selector: its ChatConnections are not served", "namespace", ns)
		}
	}
	s.selected = now
}

// listing is what the three caches held when they were listed, one after another (Bindings first, then AgentBackends, then
// ChatConnections), not at one instant, and the namespaces admitted then: what the selector matched is read with the caches, so that
// a Set and its fingerprint agree on it.
type listing struct {
	bindings, backends, connections []*unstructured.Unstructured
	hasConnection                   map[key]bool
	keys                            map[key]struct{} // every object listed, of any kind, whoever's it is
	all                             bool             // every namespace is admitted
	admitted                        map[string]bool  // the namespaces admitted, when not all: the list's, and those the selector matched
	bySelector                      bool             // a selector read the namespaces, so admitted moves and the fingerprint carries it
	selected                        []string         // the namespaces the selector admitted that are not on the list, sorted
}

// admits says whether the ChatConnections of the namespace ns are admitted at this listing.
func (l listing) admits(ns string) bool { return l.all || l.admitted[ns] }

// list reads the three caches, each sorted by namespace and name, and the namespaces when a selector reads them. It fails while one
// has not synced, and once stopped.
func (s *Source) list() (listing, error) {
	if s.ctx.Err() != nil {
		return listing{}, ErrStopped
	}
	s.mu.Lock()
	stores, synced, nsStore, nsSynced := s.stores, s.synced, s.nsStore, s.nsSynced
	s.mu.Unlock()
	for _, has := range synced {
		if has == nil || !has() {
			return listing{}, ErrNotSynced
		}
	}
	if s.admission.selector != nil && (nsSynced == nil || !nsSynced()) {
		return listing{}, ErrNotSynced
	}
	l := listing{hasConnection: map[key]bool{}, keys: map[key]struct{}{}, all: s.admission.all, admitted: s.admission.list}
	if s.admission.selector != nil {
		l.bySelector, l.admitted = true, maps.Clone(s.admission.list)
		for _, it := range nsStore.List() {
			u, ok := it.(*unstructured.Unstructured)
			if !ok || !s.admission.selector.Matches(labels.Set(u.GetLabels())) || l.admitted[u.GetName()] {
				continue
			}
			l.admitted[u.GetName()] = true
			l.selected = append(l.selected, u.GetName())
		}
		slices.Sort(l.selected)
	}
	l.bindings, l.backends, l.connections = objects(stores[0]), objects(stores[1]), objects(stores[2])
	for _, u := range l.connections {
		l.hasConnection[keyOf(config.KindChatConnection, u)] = true
		l.keys[keyOf(config.KindChatConnection, u)] = struct{}{}
	}
	for _, u := range l.bindings {
		l.keys[keyOf(config.KindBinding, u)] = struct{}{}
	}
	for _, u := range l.backends {
		l.keys[keyOf(config.KindAgentBackend, u)] = struct{}{}
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

// fingerprint digests the kind, namespace, name, uid and generation of each object, and the admitted namespaces when a selector read
// them. It leaves out resourceVersion, which a status write changes, and every other field of a namespace (see the package comment).
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
	if l.bySelector {
		for _, ns := range slices.Sorted(maps.Keys(l.admitted)) {
			_, _ = fmt.Fprintf(h, "admitted %s\n", ns)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
