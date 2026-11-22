// Package cluster runs several replicas of the adapter on one Postgres store as one: they elect a leader through the
// store's lease row (store.TryLead), the leader alone runs turns and the store's upkeep, and a standby forwards each A2A
// request to the leader. With SQLite there is one replica, which always leads.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/cma/telemetry"
)

// ExitLeadershipLost is the exit code of a replica that led and no longer does: its lease could not be renewed within
// the renew deadline, or a write or a renewal found that another replica has taken it since. The process ends at once,
// without a drain, and Kubernetes restarts it as a standby. It is the broker's code for the same end.
const ExitLeadershipLost = 5

// ErrLeadershipLost is what Run returns when this replica stops leading: the caller ends the process with
// ExitLeadershipLost.
var ErrLeadershipLost = errors.New("leadership lost: the lease was taken or could not be renewed")

// Lease is the store's side of the election. *store.Store implements it on Postgres.
type Lease interface {
	// TryLead makes one attempt to take the lease for holder, reachable at address, for d; ok reports a take, and
	// lease is the row after the attempt.
	TryLead(ctx context.Context, holder, address string, d time.Duration) (lease store.Lease, ok bool, err error)
	// Renew extends the held lease to d from now; store.ErrDeposed when it is no longer this store's.
	Renew(ctx context.Context, d time.Duration) error
	// Release ends the held lease now.
	Release(ctx context.Context) error
	// CurrentLeader reads the lease.
	CurrentLeader(ctx context.Context) (store.Lease, error)
}

// Status is what this replica is doing: leading or not, and, when it leads, whether it has finished taking over
// (reconciled the store with CMA, failed the interrupted turns) and serves A2A requests.
type Status struct {
	leading, serving atomic.Bool
}

// Leading reports whether this replica holds the lease (or is the only replica).
func (s *Status) Leading() bool { return s.leading.Load() }

// Serving reports whether this replica runs A2A requests itself.
func (s *Status) Serving() bool { return s.serving.Load() }

// Ready reports whether the replica can take requests: a standby can (it forwards them), and a leader once it serves.
func (s *Status) Ready() bool { return !s.Leading() || s.Serving() }

// Lead marks the replica as the leader, which does not serve until Serve.
func (s *Status) Lead() { s.leading.Store(true) }

// Serve marks the leader as serving: its takeover is done.
func (s *Status) Serve() { s.serving.Store(true) }

// Elector takes part in the election for one replica: it campaigns for the lease, and once it holds it, renews it and
// runs the leader's work. Every field but Telemetry and Log is required.
type Elector struct {
	Lease Lease
	// Holder names this replica (POD_NAME); Address is where the others reach it (ADVERTISE_URL).
	Holder, Address string
	// LeaseDuration is how long the lease lasts after a renewal; RenewDeadline how long the leader keeps trying to
	// renew before it gives up, shorter, so that it has stopped before a standby may take over; RetryPeriod how often a
	// standby tries to take the lease and the leader renews it.
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
	Status                                    *Status
	Telemetry                                 *telemetry.Telemetry
	Log                                       *slog.Logger

	mu     sync.Mutex
	leader cachedLease
	// taken is when TryLeadOnce took the lease, before Run; zero when it did not.
	taken time.Time
}

// cachedLease is the lease as a standby last read it.
type cachedLease struct {
	lease store.Lease
	at    time.Time // zero: none
}

// Run campaigns until this replica takes the lease or ctx ends, trying every RetryPeriod. Once it leads, it runs lead in
// a goroutine of its own, with a context that ends when Run is done, and renews the lease every RetryPeriod; Run returns
// only once lead has. It returns ErrLeadershipLost when a renewal finds the lease taken, or no renewal has succeeded for
// RenewDeadline; the error of lead, if lead fails; and nil when ctx ends. It never releases the lease: the caller does
// that (Release) once it has stopped serving, so that a standby does not take over while this replica still writes.
//
// After TryLeadOnce has taken the lease, Run leads at once, without campaigning.
func (e *Elector) Run(ctx context.Context, lead func(ctx context.Context) error) error {
	lastRenewal := e.taken
	if lastRenewal.IsZero() {
		e.Telemetry.SetLeader(false)
		taken, err := e.campaign(ctx)
		if err != nil || !taken {
			return err
		}
		lastRenewal = time.Now() // no later than the database's now() of the take
		e.lead()
	}

	// lead's work stops before Run returns, whichever way it ends: the caller releases the lease and closes the store
	// next, and the work (the upkeep) must not write under either.
	leadCtx, cancel := context.WithCancel(ctx)
	leadDone := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		leadDone <- lead(leadCtx)
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	tick := time.NewTicker(e.RetryPeriod)
	defer tick.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-leadDone:
			if err != nil {
				return fmt.Errorf("take over as the leader: %w", err)
			}
			leadDone = nil // lead returned early; keep the lease until ctx ends
		case <-tick.C:
			attempt := time.Now()
			renewCtx, cancelRenew := context.WithDeadline(ctx, lastRenewal.Add(e.RenewDeadline))
			err := e.Lease.Renew(renewCtx, e.LeaseDuration)
			cancelRenew()
			switch {
			case err == nil:
				lastRenewal = attempt
				if failing {
					failing = false
					e.Log.Info("renewed the lease again")
				}
			case ctx.Err() != nil:
				return nil
			case errors.Is(err, store.ErrDeposed):
				e.Log.Error("another replica has taken the lease: stopping", "error", err)
				return ErrLeadershipLost
			case time.Since(lastRenewal) >= e.RenewDeadline:
				e.Log.Error("could not renew the lease within the renew deadline: stopping", "renew_deadline", e.RenewDeadline, "error", err)
				return ErrLeadershipLost
			case !failing:
				failing = true
				e.Log.Warn("renew the lease; will retry until the renew deadline", "renew_deadline", e.RenewDeadline, "error", err)
			}
		}
	}
}

// TryLeadOnce makes one attempt to take the lease, and reports whether it did. Call it before the replica listens and
// before Run: a replica that takes the lease then is the leader, and not ready, from its first request, so that the
// startup probe bounds its takeover; one that does not is a standby, ready at once. A failure to reach the store is
// logged, and Run's campaign tries again.
func (e *Elector) TryLeadOnce(ctx context.Context) bool {
	attempt := time.Now() // no later than the database's now() of the take, so the renew deadline is not overstated
	lease, ok, err := e.Lease.TryLead(ctx, e.Holder, e.Address, e.LeaseDuration)
	if err != nil {
		e.Log.Warn("take the lease at start; will campaign", "error", err)
		return false
	}
	e.remember(lease)
	if !ok {
		return false
	}
	e.Log.Info("took the lease: leading", "epoch", lease.Epoch, "lease_duration", e.LeaseDuration)
	e.taken = attempt
	e.lead()
	return true
}

// lead marks this replica as the leader, once it holds the lease.
func (e *Elector) lead() {
	e.Status.Lead()
	e.Telemetry.SetLeader(true)
	e.Telemetry.Takeover()
}

// campaign tries to take the lease every RetryPeriod until it does (true) or ctx ends (false). Each attempt reads the
// lease, which keeps the standby's view of the leader current. A failure to reach the store is logged when it starts
// and when it ends, not at every attempt.
func (e *Elector) campaign(ctx context.Context) (bool, error) {
	failing := false
	for {
		lease, ok, err := e.Lease.TryLead(ctx, e.Holder, e.Address, e.LeaseDuration)
		switch {
		case ctx.Err() != nil:
			return false, nil
		case err != nil:
			if !failing {
				failing = true
				e.Log.Warn("take the lease; will retry", "retry_period", e.RetryPeriod, "error", err)
			}
		default:
			if failing {
				failing = false
				e.Log.Info("the store is reachable again")
			}
			e.remember(lease)
			if ok {
				e.Log.Info("took the lease: leading", "epoch", lease.Epoch, "lease_duration", e.LeaseDuration)
				return true, nil
			}
		}
		select {
		case <-ctx.Done():
			return false, nil
		case <-time.After(e.RetryPeriod):
		}
	}
}

// Release ends the lease this replica holds, so that a standby takes over on its next try. Call it after Run has
// returned and the replica has stopped serving.
func (e *Elector) Release(ctx context.Context) error {
	if !e.Status.Leading() {
		return nil
	}
	if err := e.Lease.Release(ctx); err != nil {
		return fmt.Errorf("release the lease: %w", err)
	}
	e.Log.Info("released the lease")
	return nil
}

func (e *Elector) remember(l store.Lease) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.leader = cachedLease{lease: l, at: time.Now()}
}

// Leader returns the base URL of the replica that leads, when it is another one and its lease is live. It reads the
// lease the campaign last read, if that is less than RetryPeriod old, and the store otherwise.
func (e *Elector) Leader(ctx context.Context) (*url.URL, bool) {
	e.mu.Lock()
	c := e.leader
	e.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) >= e.RetryPeriod {
		l, err := e.Lease.CurrentLeader(ctx)
		if err != nil {
			e.Log.Warn("read the lease to forward a request", "error", err)
			return nil, false
		}
		e.remember(l)
		c.lease = l
	}
	l := c.lease
	if !l.Live || l.Holder == e.Holder || l.Address == "" || l.Address == e.Address {
		return nil, false
	}
	u, err := url.Parse(l.Address)
	if err != nil {
		e.Log.Warn("the leader's address is not a URL", "holder", l.Holder)
		return nil, false
	}
	return u, true
}

// Forget drops the lease Leader read, after its address could not be reached: the next request reads the store.
func (e *Elector) Forget() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.leader = cachedLease{}
}
