package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// ExitLeadershipLost is the exit code of a replica that was the leader and no longer is: its lease lapsed or could
// not be renewed, or a write found that another replica has led since (store.ErrDeposed). The process ends at
// once, with no graceful drain, since a drain would post (spec D37); Kubernetes restarts it as a standby.
const ExitLeadershipLost = 5

// The timings of the election (spec D37), used where ElectionConfig leaves one zero. The leader gives up renewing
// 5 s before a candidate may take the lease, and exits at once, so the old leader has normally stopped before the
// new one starts; the worst case to resume work is a lease duration and a start-up.
const (
	defaultLeaseDuration = 15 * time.Second
	defaultRenewDeadline = 10 * time.Second
	defaultRetryPeriod   = 2 * time.Second
	// takeRetryEvery is how long the takeover of the epoch waits before it tries again.
	takeRetryEvery = 250 * time.Millisecond
	// releaseTimeout bounds the hand-over of the lease after a graceful shutdown.
	releaseTimeout = 3 * time.Second
)

// ElectionConfig says how a replica takes part in the election of the leader, which happens with a store that fences
// (Postgres) and not with SQLite, and then needs every field but the timings. The replicas of one Deployment name the
// same Lease.
type ElectionConfig struct {
	// Namespace and Name are the Lease's: courier's own namespace, and one Lease for the Deployment.
	Namespace, Name string
	// Identity says who holds the Lease, and is the holder recorded in the database (leadership.holder). It must be
	// unique to this process, not only to its pod: a container that restarts in the same pod must not take the
	// Lease its dead predecessor held for its own, and skip the wait that a standby is owed.
	Identity string
	// LeaseDuration is how long a candidate waits after the last renewal it saw before it takes the Lease (15 s if
	// zero), RenewDeadline how long the leader keeps trying to renew before it gives up (10 s), and RetryPeriod how
	// often each of them tries (2 s).
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
}

// withDefaults returns c with the timings that were left zero filled in.
func (c ElectionConfig) withDefaults() ElectionConfig {
	if c.LeaseDuration == 0 {
		c.LeaseDuration = defaultLeaseDuration
	}
	if c.RenewDeadline == 0 {
		c.RenewDeadline = defaultRenewDeadline
	}
	if c.RetryPeriod == 0 {
		c.RetryPeriod = defaultRetryPeriod
	}
	return c
}

// validate says whether c, with its defaults, can run an election: a wiring error otherwise. The ordering of the
// timings is the one client-go's leader election needs, said here so that it is a usage error and not a failure.
func (c ElectionConfig) validate() error {
	c = c.withDefaults()
	var missing []string
	for name, v := range map[string]string{"Namespace": c.Namespace, "Name": c.Name, "Identity": c.Identity} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("%w: the election lacks %v", ErrWiring, missing)
	}
	switch {
	case c.LeaseDuration <= c.RenewDeadline:
		return fmt.Errorf("%w: the lease duration (%s) must be greater than the renew deadline (%s)", ErrWiring, c.LeaseDuration, c.RenewDeadline)
	case c.RenewDeadline <= time.Duration(leaderelection.JitterFactor*float64(c.RetryPeriod)):
		return fmt.Errorf("%w: the renew deadline (%s) must be greater than the retry period (%s) times %.1f", ErrWiring, c.RenewDeadline, c.RetryPeriod, leaderelection.JitterFactor)
	case c.RetryPeriod < 0 || c.RenewDeadline < 0 || c.LeaseDuration < 0:
		return fmt.Errorf("%w: the election's timings must be positive", ErrWiring)
	}
	return nil
}

// failStop ends the process at once, once, with a line that says why: the answer to a lost lease and to a stale
// epoch (spec D37). exit is os.Exit; a test replaces it.
type failStop struct {
	log  *slog.Logger
	exit func(code int)
	once sync.Once
}

func (f *failStop) now(reason string) {
	f.once.Do(func() {
		f.log.Error("leadership lost: stopping at once, without a drain", "reason", reason)
		f.exit(ExitLeadershipLost)
	})
}

// startGate lets the leader half start only while Run is not shutting down, and lets Run's shutdown wait for a start
// that is in progress. The lease can be gained at any moment, in a goroutine of client-go's, while Run is on its way
// out; a start that registered its stops after the shutdown had passed them would leave the parts it started running.
type startGate struct {
	mu       sync.Mutex
	closed   bool
	inFlight int
	wg       sync.WaitGroup
}

// enter admits a start, which must call leave when it is done, unless the gate is closed.
func (g *startGate) enter() (leave func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, false
	}
	g.inFlight++
	g.wg.Add(1)
	return func() {
		g.mu.Lock()
		g.inFlight--
		g.mu.Unlock()
		g.wg.Done()
	}, true
}

// close refuses every later start and waits for the one in progress, calling abort first if there is one, so that it
// ends at once instead of running to the end of its recovery.
func (g *startGate) close(abort func()) {
	g.mu.Lock()
	g.closed = true
	running := g.inFlight > 0
	g.mu.Unlock()
	if running {
		abort()
	}
	g.wg.Wait()
}

// takeEpoch asks the store for the next epoch until it has one or ctx ends, waiting every between attempts. One
// attempt is bounded by timeouts of the store's; the ones to expect are a lock held by the writes of a deposed leader
// that are still in flight (they give up after a few seconds), and the database being briefly unreachable.
func takeEpoch(ctx context.Context, f store.Fenced, holder string, every time.Duration, log *slog.Logger) (int64, error) {
	var last error
	for attempt := 1; ; attempt++ {
		epoch, err := f.Lead(ctx, holder)
		if err == nil {
			return epoch, nil
		}
		if errors.Is(err, store.ErrInvalid) {
			return 0, err
		}
		last = err
		log.WarnContext(ctx, "taking the leader's epoch failed; trying again", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("%w (last attempt: %w)", ctx.Err(), last)
		case <-time.After(every):
		}
	}
}

// candidacy is this replica's part in the election: the client-go elector running in a goroutine of its own.
type candidacy struct {
	lock     resourcelock.Interface
	cancel   context.CancelFunc
	done     chan struct{}
	stopping *atomic.Bool
	log      *slog.Logger
}

// candidacyCallbacks are what the election calls back.
type candidacyCallbacks struct {
	// gained runs, in a goroutine of its own, when the Lease is won. Its context ends when the election does.
	gained func(ctx context.Context)
	// lost runs when a replica that held the Lease no longer does: it is the fail-stop.
	lost func()
}

// startCandidacy begins to ask for the Lease. The goroutine ends with stop, or when the Lease is lost, which calls
// cb.lost; it does not stand again, since the process is to end.
func startCandidacy(ctx context.Context, client kubernetes.Interface, cfg ElectionConfig, log *slog.Logger, cb candidacyCallbacks) (*candidacy, error) {
	cfg = cfg.withDefaults()
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.Name},
		Client:     client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: cfg.Identity},
	}
	c := &candidacy{lock: lock, done: make(chan struct{}), stopping: new(atomic.Bool), log: log}
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: lock, LeaseDuration: cfg.LeaseDuration, RenewDeadline: cfg.RenewDeadline, RetryPeriod: cfg.RetryPeriod,
		Name: cfg.Name,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: cb.gained,
			// client-go calls this whenever its loop ends, a candidate that never led included. It ends for two
			// reasons only: stop, which sets stopping first, or a leader that lost the Lease.
			OnStoppedLeading: func() {
				if !c.stopping.Load() {
					cb.lost()
				}
			},
			OnNewLeader: func(identity string) {
				if identity != cfg.Identity {
					log.InfoContext(ctx, "another replica leads", "holder", identity)
				}
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: leader election: %w", ErrWiring, err)
	}
	ectx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.cancel = cancel
	go func() { defer close(c.done); le.Run(ectx) }()
	return c, nil
}

// stop ends the candidacy once the leader half has stopped, and hands the Lease over if this replica holds it, so
// that the other one need not wait out its lease duration after a deliberate stop (a rolling update). The lease
// was renewed throughout the shutdown, so the new leader never starts beside a leader that is still draining.
// A failed hand-over is logged and costs only that wait.
func (c *candidacy) stop(ctx context.Context) {
	if c == nil {
		return
	}
	c.stopping.Store(true)
	c.cancel()
	<-c.done
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := c.release(ctx); err != nil {
		c.log.Warn("handing the lease over: the other replica will wait for it to lapse", "error", err)
	}
}

// release gives the Lease up if this replica holds it: it is what client-go's ReleaseOnCancel does, minus its
// timing, which would hold the exit of a leader that lost the Lease for a release that cannot succeed.
func (c *candidacy) release(ctx context.Context) error {
	rec, _, err := c.lock.Get(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read the lease: %w", err)
	}
	if rec.HolderIdentity != c.lock.Identity() {
		return nil
	}
	now := metav1.NewTime(time.Now())
	if err := c.lock.Update(ctx, resourcelock.LeaderElectionRecord{
		LeaderTransitions: rec.LeaderTransitions, LeaseDurationSeconds: 1, AcquireTime: now, RenewTime: now,
	}); err != nil {
		return fmt.Errorf("release the lease: %w", err)
	}
	return nil
}
