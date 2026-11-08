package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// ReloaderConfig configures a Reloader.
type ReloaderConfig struct {
	// Dir is the config directory, as for Load. It is ignored if Source is set.
	Dir string
	// Source is where the objects are read from. It defaults to the files of Dir.
	Source Source
	// Secrets checks the Secrets that objects refer to.
	Secrets SecretResolver
	// PollInterval is how often the source is checked for changes. It defaults to 10s.
	PollInterval time.Duration
	// SecretsInterval is how often the config is resolved again even if the source did not change,
	// because Secrets change on their own. It defaults to 1m.
	SecretsInterval time.Duration
	// SecretsWait bounds how long a reload waits for the Secrets that it newly refers to to become readable, when Secrets
	// is a SecretWatcher. A Secret that is not readable by then fails the reload, which is tried again at the next
	// recheck (SecretsInterval) or when the files change. It defaults to 10s.
	SecretsWait time.Duration
	// Logger receives reload problems. It defaults to slog.Default().
	Logger *slog.Logger
	// OnError is called with the error of each failed reload that Status.Errors counts, for a
	// metric. It runs on the reloading goroutine, so it must not block.
	OnError func(error)
	// PerObject makes a bad object a problem of that object alone: the Set is resolved with ResolvePartial, so a Binding
	// whose references fail or that lost its channel is left out and reported (Report), where otherwise any such problem
	// fails the reload and the last good snapshot stays. It is for a source whose objects have different owners, the custom
	// resources. It also stops the Secrets from being checked: a Secret that is missing or unreadable does not keep a
	// Binding out of the snapshot, the prober says so, and only the Secrets of the objects that are served are followed.
	// The files of a directory are one artifact, loaded all or nothing: leave it false for them.
	//
	// A PerObject Reloader publishes each read of the Source, and does not read again to see whether the source moved
	// meanwhile: it takes the Source at its word that Set is the state the fingerprint it returns names (see Source). What
	// changed after the read is found at the next poll, which compares the source's fingerprint with that of the read. So a
	// source that is edited without pause is published as it was at the start of each read, a read behind at most, where a
	// Reloader that discarded a read that went stale would publish nothing for as long as the edits last. A Reloader that is
	// not PerObject reads a directory, which gives no such word, and so reads again when it changes under the read.
	PerObject bool
	// OnReport is called with the Report of each snapshot that is published, including a recheck that finds nothing new,
	// for a metric. It runs on the reloading goroutine, so it must not block.
	OnReport func(Report)
}

// Status describes the health of a Reloader.
type Status struct {
	// LastSuccess is when the current snapshot was built; zero before the first load.
	LastSuccess time.Time
	// Stale means the latest reload failed, so the current snapshot lags the config.
	Stale bool
	// Errors counts failed reloads, for config_reload_errors_total. A reload cut short by
	// cancellation is not a config error and is not counted.
	Errors uint64
}

// Reloader keeps a Snapshot of the config current. Readers call Current, which takes
// no lock; a failed reload keeps the last good snapshot.
type Reloader struct {
	source       Source
	secrets      SecretResolver
	poll         time.Duration
	secretsEvery time.Duration
	secretsWait  time.Duration
	log          *slog.Logger
	onError      func(error)
	perObject    bool
	onReport     func(Report)

	snap   atomic.Pointer[published] // the Snapshot with what Revision needs, replaced as one
	status atomic.Pointer[Status]
	wg     sync.WaitGroup

	// Owned by whoever is reloading: Start, then the loop goroutine.
	lastFingerprint string
	lastAttempt     time.Time
	failures        int             // consecutive failed reloads
	reported        map[string]bool // what the latest report said, so that a recheck stays silent about it
	approvalWarned  map[string]bool // the Bindings whose approval policy the latest warning named, so that a change elsewhere stays silent
}

// errConfigMoving means the config source changed during two loads in a row, so neither
// result can be trusted. It is not a config error; the next poll tries again. At Start there
// is no next poll: the first load fails with it. A PerObject Reloader never returns it: see ReloaderConfig.PerObject.
var errConfigMoving = errors.New("config changed during the load")

// NewReloader returns a Reloader that has not loaded anything yet.
func NewReloader(cfg ReloaderConfig) *Reloader {
	src := cfg.Source
	if src == nil {
		src = dirSource{cfg.Dir}
	}
	r := &Reloader{
		source:       src,
		secrets:      cfg.Secrets,
		poll:         cmp.Or(cfg.PollInterval, 10*time.Second),
		secretsEvery: cmp.Or(cfg.SecretsInterval, time.Minute),
		secretsWait:  cmp.Or(cfg.SecretsWait, 10*time.Second),
		log:          cmp.Or(cfg.Logger, slog.Default()),
		onError:      cfg.OnError,
		perObject:    cfg.PerObject,
		onReport:     cfg.OnReport,
	}
	r.status.Store(&Status{})
	return r
}

// Current returns the latest good snapshot, or nil before Start has succeeded. It is
// read-only, like everything a Snapshot returns.
func (r *Reloader) Current() *Snapshot {
	if p := r.snap.Load(); p != nil {
		return p.snap
	}
	return nil
}

// Report returns what the Reloader knows about the snapshot it serves besides the snapshot: the objects it refused and the
// Bindings it does not serve as written. It is the zero Report before Start has succeeded, and has no rejections or
// invalid objects unless the Reloader is PerObject.
func (r *Reloader) Report() Report {
	if p := r.snap.Load(); p != nil {
		return p.report
	}
	return Report{}
}

// Status returns the health of the Reloader. A Source that is a StaleSource and says its view may lag makes it stale, for as
// long as it says so, whatever the latest reload did: the cache that a reload read is kept while the cluster cannot be
// reached, and only the source knows. LastSuccess and Errors are the reloads'.
func (r *Reloader) Status() Status {
	st := *r.status.Load()
	if s, ok := r.source.(StaleSource); ok && s.Stale() != nil {
		st.Stale = true
	}
	return st
}

// Start loads the config and, if that works, starts checking for changes every PollInterval
// until ctx ends. A failure of the first load is returned and starts nothing. Call Start once.
func (r *Reloader) Start(ctx context.Context) error {
	if err := r.reload(ctx, true); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	r.wg.Go(func() {
		t := time.NewTicker(r.poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Failures are recorded and logged by reload, and the last good snapshot stays.
				_ = r.reload(ctx, time.Since(r.lastAttempt) >= r.secretsEvery)
			}
		}
	})
	return nil
}

// Wait blocks until the loop started by Start has stopped, which happens once its ctx ends.
func (r *Reloader) Wait() { r.wg.Wait() }

// reload rebuilds the snapshot if the source changed since the last attempt, or if force is
// set, and publishes it. A failed rebuild is recorded and logged and returned, and leaves the
// last good snapshot in place. A rebuild cut short by ctx is returned but not recorded.
func (r *Reloader) reload(ctx context.Context, force bool) error {
	if !force {
		if r.source.Fingerprint(ctx) == r.lastFingerprint {
			return nil
		}
	}
	pub, fp, err := r.build(ctx)
	if err != nil && (ctx.Err() != nil || errors.Is(err, errConfigMoving)) {
		r.log.Info("config reload interrupted", "error", err)
		return err
	}
	changed := fp != r.lastFingerprint
	r.lastFingerprint, r.lastAttempt = fp, time.Now()
	st := *r.status.Load() // not Status: the source's own word is not a reload's outcome, and must not be stored as one
	if err != nil {
		st.Stale, st.Errors = true, st.Errors+1
		r.failures++
		r.log.Error("config reload failed; keeping the last good snapshot", "error", err)
	} else {
		warnPolicies := r.snap.Load() == nil || changed || st.Stale
		switch { // a recheck that finds nothing new stays silent
		case st.Stale:
			r.log.Info("config reloaded after failed reloads", "failed_reloads", r.failures)
		case changed:
			r.log.Info("config reloaded")
		}
		st.Stale, st.LastSuccess, r.failures = false, r.lastAttempt, 0
		r.snap.Store(pub)
		r.logReport(pub.report)
		// After the store, so that whoever finds a Secret dropped finds the snapshot that dropped it; a reader that still holds the old
		// snapshot follows its Secrets again itself.
		switch w := r.secrets.(type) {
		case KeyedSecretWatcher:
			w.RetainKeys(pub.reads)
		case SecretWatcher:
			w.Retain(pub.names())
		}
		if warnPolicies {
			r.warnToolApprovalPolicies(pub.snap)
		}
		if r.onReport != nil {
			r.onReport(pub.report)
		}
	}
	r.status.Store(&st)
	if err != nil && r.onError != nil { // after the status, so a hook that reads Status sees Stale
		r.onError(err)
	}
	return err
}

// warnToolApprovalPolicies reports the enabled approval policy that leaves every non-bot channel member eligible, once for each
// Binding that has it: a changed read that leaves the Binding's policy as it was says nothing, so that one team editing in a
// loop does not repeat the warning about another's Binding at every poll. A Binding that fixes its policy and enables it again
// is warned of again. The warning belongs to the config load, so it is emitted even before a Binding is used by the broker.
func (r *Reloader) warnToolApprovalPolicies(snap *Snapshot) {
	now := map[string]bool{}
	for _, rb := range snap.Bindings() {
		in := rb.Binding.Spec.Interactions
		if in.ToolApprovals && len(in.ApproverGroups) == 0 {
			name := rb.Binding.Namespace + "/" + rb.Binding.Name
			now[name] = true
			if !r.approvalWarned[name] {
				r.log.Warn("config: tool approvals are enabled without approver groups; any non-bot channel member may decide", "binding", name)
			}
		}
	}
	r.approvalWarned = now
}

// build reads the config from the source and resolves it. A PerObject Reloader resolves the one read it makes and returns it with
// the fingerprint the source gave it (see ReloaderConfig.PerObject). Otherwise, if the source changes while it works, it goes
// again, once. If it changes again, neither result is trusted and build returns errConfigMoving.
func (r *Reloader) build(ctx context.Context) (pub *published, fp string, err error) {
	if r.perObject {
		var set *Set
		if set, fp, err = r.source.Set(ctx); err != nil {
			return nil, fp, err
		}
		return r.buildPerObject(ctx, set, fp), fp, nil
	}
	for range 2 {
		var set *Set
		set, fp, err = r.source.Set(ctx)
		pub = nil
		if err == nil {
			refs := set.secretRefs() // before Resolve, which takes the set
			r.watchSecrets(ctx, refs)
			var snap *Snapshot
			if snap, err = Resolve(ctx, set, r.secrets); err == nil {
				pub = newPublished(snap, fp, refs)
			}
		}
		if r.source.Fingerprint(ctx) == fp {
			return pub, fp, err
		}
	}
	return nil, "", errConfigMoving
}

// watchSecrets has a SecretWatcher follow the Secrets that refs name (a KeyedSecretWatcher with the keys that they read), and waits a bounded time for each to be readable.
// It ignores the answer: Resolve asks for each of the Secrets next and reports every one that is not readable, with its
// cause, in the error of the reload.
func (r *Reloader) watchSecrets(ctx context.Context, refs []secretRef) {
	var watch func(context.Context) error
	switch w := r.secrets.(type) {
	case KeyedSecretWatcher:
		watch = func(ctx context.Context) error { return w.WatchKeys(ctx, secretReads(refs)) }
	case SecretWatcher:
		watch = func(ctx context.Context) error { return w.Watch(ctx, secretNames(refs)) }
	default:
		return
	}
	ctx, cancel := context.WithTimeout(ctx, r.secretsWait)
	defer cancel()
	_ = watch(ctx)
}

// buildPerObject resolves set object by object: a Binding that cannot be served is left out and reported, and nothing is
// a failure of the reload. The Secrets it follows are those of what the snapshot serves, so that a Secret of a Binding
// that was left out is neither followed nor a part of the revision. It reads no Secret: a Secret that is missing or lacks a
// key does not keep a Binding out, and the prober reports it.
func (r *Reloader) buildPerObject(ctx context.Context, set *Set, fp string) *published {
	invalid := slices.Clone(set.Invalid) // the Set is the snapshot's from here
	snap, rejections := resolvePerObject(set)
	refs := servedSet(snap).secretRefs()
	r.watchSecrets(ctx, refs)
	pub := newPublished(snap, fp, refs)
	slices.SortFunc(invalid, func(a, b Invalid) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	pub.report.Rejections, pub.report.Invalid = rejections, invalid
	pub.report.UnservedConnections = slices.Clone(set.UnservedConnections)
	slices.SortFunc(pub.report.UnservedConnections, func(a, b UnservedConnection) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return pub
}

// logReport says, once, each object that the report refuses and each Binding that is not served as written. A recheck
// that finds the same says nothing, and when none is left it says so. It owns r.reported, and runs on the goroutine that
// reloads.
func (r *Reloader) logReport(rep Report) {
	now := map[string]bool{}
	for _, inv := range rep.Invalid {
		if inv.Deleted && inv.Kind != KindChatConnection {
			continue // held with the connection: the source says so once, with how many
		}
		key := fmt.Sprintf("invalid %s %d %d %t %s", inv.ID(), inv.Generation, inv.Served, inv.Deleted, inv.Message)
		now[key] = true
		if !r.reported[key] {
			r.log.Warn("config: an object is refused; it is served from its last good generation, or left out if it has none",
				"kind", inv.Kind, "object", v1alpha1.NamespacedName(inv.Namespace, inv.Name), "generation", inv.Generation,
				"served_generation", inv.Served, "deleted", inv.Deleted, "problems", inv.Message)
		}
	}
	for _, rej := range rep.Rejections {
		key := fmt.Sprintf("binding %s/%s %s %d %d %s", rej.Namespace, rej.Name, rej.Reason, rej.Generation, rej.Served, rej.Message)
		now[key] = true
		if !r.reported[key] {
			r.log.Warn("config: a Binding is not accepted as written", "binding", v1alpha1.NamespacedName(rej.Namespace, rej.Name),
				"reason", rej.Reason, "generation", rej.Generation, "served_generation", rej.Served, "problem", rej.Message)
		}
	}
	if len(r.reported) > 0 && len(now) == 0 {
		r.log.Info("config: every object is accepted")
	}
	r.reported = now
}
