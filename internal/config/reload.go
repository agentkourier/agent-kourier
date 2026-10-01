package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ReloaderConfig configures a Reloader.
type ReloaderConfig struct {
	// Dir is the config directory, as for Load.
	Dir string
	// Secrets checks the Secrets that objects refer to.
	Secrets SecretResolver
	// PollInterval is how often Dir is checked for changes. It defaults to 10s.
	PollInterval time.Duration
	// SecretsInterval is how often the config is resolved again even if no file changed,
	// because Secrets change on their own. It defaults to 1m.
	SecretsInterval time.Duration
	// Logger receives reload problems. It defaults to slog.Default().
	Logger *slog.Logger
	// OnError is called with the error of each failed reload that Status.Errors counts, for a
	// metric. It runs on the reloading goroutine, so it must not block.
	OnError func(error)
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

// Reloader keeps a Snapshot of a config directory current. Readers call Current, which takes
// no lock; a failed reload keeps the last good snapshot.
type Reloader struct {
	dir          string
	secrets      SecretResolver
	poll         time.Duration
	secretsEvery time.Duration
	log          *slog.Logger
	onError      func(error)

	snap   atomic.Pointer[Snapshot]
	status atomic.Pointer[Status]
	wg     sync.WaitGroup

	// Owned by whoever is reloading: Start, then the loop goroutine.
	lastFingerprint string
	lastAttempt     time.Time
	failures        int // consecutive failed reloads
}

// errConfigMoving means the config directory changed during two loads in a row, so neither
// result can be trusted. It is not a config error; the next poll tries again.
var errConfigMoving = errors.New("config directory changed during the load")

// NewReloader returns a Reloader that has not loaded anything yet.
func NewReloader(cfg ReloaderConfig) *Reloader {
	r := &Reloader{
		dir:          cfg.Dir,
		secrets:      cfg.Secrets,
		poll:         cmp.Or(cfg.PollInterval, 10*time.Second),
		secretsEvery: cmp.Or(cfg.SecretsInterval, time.Minute),
		log:          cmp.Or(cfg.Logger, slog.Default()),
		onError:      cfg.OnError,
	}
	r.status.Store(&Status{})
	return r
}

// Current returns the latest good snapshot, or nil before Start has succeeded. It is
// read-only, like everything a Snapshot returns.
func (r *Reloader) Current() *Snapshot { return r.snap.Load() }

// Status returns the health of the Reloader.
func (r *Reloader) Status() Status { return *r.status.Load() }

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

// reload rebuilds the snapshot if the files changed since the last attempt, or if force is
// set, and publishes it. A failed rebuild is recorded and logged and returned, and leaves the
// last good snapshot in place. A rebuild cut short by ctx is returned but not recorded.
func (r *Reloader) reload(ctx context.Context, force bool) error {
	if !force {
		if fingerprint(generation(r.dir)) == r.lastFingerprint {
			return nil
		}
	}
	snap, fp, err := r.build(ctx)
	if err != nil && (ctx.Err() != nil || errors.Is(err, errConfigMoving)) {
		r.log.Info("config reload interrupted", "error", err)
		return err
	}
	changed := fp != r.lastFingerprint
	r.lastFingerprint, r.lastAttempt = fp, time.Now()
	st := r.Status()
	if err != nil {
		st.Stale, st.Errors = true, st.Errors+1
		r.failures++
		r.log.Error("config reload failed; keeping the last good snapshot", "error", err)
	} else {
		switch { // a recheck that finds nothing new stays silent
		case st.Stale:
			r.log.Info("config reloaded after failed reloads", "failed_reloads", r.failures)
		case changed:
			r.log.Info("config reloaded")
		}
		st.Stale, st.LastSuccess, r.failures = false, r.lastAttempt, 0
		r.snap.Store(snap)
	}
	r.status.Store(&st)
	if err != nil && r.onError != nil { // after the status, so a hook that reads Status sees Stale
		r.onError(err)
	}
	return err
}

// build loads and resolves the config from one generation of the directory. If the
// generation changes while it works, it goes again, once. If it changes again, neither result
// is trusted and build returns errConfigMoving.
func (r *Reloader) build(ctx context.Context) (snap *Snapshot, fp string, err error) {
	for range 2 {
		root, gen := generation(r.dir)
		fp = fingerprint(root, gen)
		var set *Set
		if set, err = Load(ctx, root); err == nil {
			snap, err = Resolve(ctx, set, r.secrets)
		}
		if _, now := generation(r.dir); now == gen {
			return snap, fp, err
		}
	}
	return nil, "", errConfigMoving
}

// generation returns the directory to read config from. A mounted ConfigMap replaces its
// files by pointing the ..data symlink at a new directory, and files read through the
// top-level links can come from two generations. Reading the target directory itself, which
// never changes, cannot. Without ..data, the directory is read as it is, and gen is empty.
func generation(dir string) (root, gen string) {
	gen, err := os.Readlink(filepath.Join(dir, "..data"))
	if err != nil {
		return dir, ""
	}
	if filepath.IsAbs(gen) {
		return gen, gen
	}
	return filepath.Join(dir, gen), gen
}

// fingerprint summarizes the config files of root by name, size, and modification time, and
// the generation, so that a change to any of them changes it. It cannot fail: a directory or
// file that cannot be read fingerprints as its error, so that a broken state looks the same
// on every poll and fixing it looks like a change.
func fingerprint(root, gen string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "!" + err.Error()
	}
	var b strings.Builder
	b.WriteString(gen)
	for _, e := range entries {
		if !isConfigFile(e.Name()) {
			continue
		}
		fi, err := os.Stat(filepath.Join(root, e.Name())) // follows symlinks
		if err != nil {
			fmt.Fprintf(&b, "|%s:!%v", e.Name(), err)
			continue
		}
		fmt.Fprintf(&b, "|%s:%d:%d", e.Name(), fi.Size(), fi.ModTime().UnixNano())
	}
	return b.String()
}
