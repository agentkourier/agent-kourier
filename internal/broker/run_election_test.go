package broker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// fakeFence is the epoch of a database that two fencing stores share: what Postgres keeps in the leadership table.
type fakeFence struct {
	mu      sync.Mutex
	epoch   int64
	holders []string
	failing bool // Lead fails: the database cannot be reached
}

// fencedSQLite is a store that fences, in the sense that Run asks of it, over a SQLite file. It stands in for the
// Postgres store in the tests of Run that need no database: the split of the leader half and the election.
type fencedSQLite struct {
	openedStore
	fence *fakeFence
}

func (s fencedSQLite) Lead(_ context.Context, holder string) (int64, error) {
	s.fence.mu.Lock()
	defer s.fence.mu.Unlock()
	if s.fence.failing {
		return 0, errors.New("the database cannot be reached")
	}
	s.fence.epoch++
	s.fence.holders = append(s.fence.holders, holder)
	return s.fence.epoch, nil
}

// replica is a Run of a test that takes part in an election.
type replica struct {
	*running
	built atomic.Int32 // how many chat platforms the replica built: only the leader half builds one
	exits chan int
	logs  *logBuffer
}

// electionConfig is startConfig for a replica of a pair: the same cluster, its own database file and port, and a
// store that fences over the shared fence. exit is recorded and does not end the test binary.
func electionConfig(t *testing.T, client *fake.Clientset, fence *fakeFence, identity string) (RunConfig, *replica) {
	t.Helper()
	c := startConfig(t)
	if client != nil {
		c.Client = client
	}
	rep := &replica{exits: make(chan int, 4), logs: &logBuffer{}}
	c.Logger = slog.New(slog.NewTextHandler(rep.logs, nil))
	c.Election = testElection
	c.Election.Identity = identity
	c.newSlack = func(slack.Config) slackAPI { rep.built.Add(1); return idleSlack{} }
	c.wrapStore = func(s openedStore) openedStore { return fencedSQLite{openedStore: s, fence: fence} }
	c.exit = func(code int) { rep.exits <- code }
	c.leaveKlog = true // two Runs in one process
	return c, rep
}

func (r *replica) metric(line string) bool {
	_, body := r.get("/metrics")
	return strings.Contains(body, line+"\n")
}

// A replica that has not won the lease is as ready as the leader and does none of its work: it builds no chat
// platform, so it opens no Socket Mode connection, and it reports courier_leader 0. The one that wins is given the
// epoch, and does all of it.
func TestRunOnlyTheLeaderStartsTheLeaderHalf(t *testing.T) {
	client, fence := cluster(), &fakeFence{}
	ca, a := electionConfig(t, client, fence, "pod-a_1")
	a.running = start(t, ca)
	eventually(t, 10*time.Second, "the first replica did not start the leader half", func() bool { return a.built.Load() == 1 })
	cb, b := electionConfig(t, client, fence, "pod-b_1")
	b.running = start(t, cb)

	a.until("/readyz", http.StatusOK)
	b.until("/readyz", http.StatusOK)
	time.Sleep(testElection.RetryPeriod * 5) // long enough for the standby to have tried the lease a few times

	if b.built.Load() != 0 {
		t.Errorf("the standby built %d chat platforms; want none: only the leader connects", b.built.Load())
	}
	if !a.metric("courier_leader 1") || !a.metric("courier_leader_takeovers_total 1") {
		t.Error("the leader's /metrics lacks courier_leader 1 and courier_leader_takeovers_total 1")
	}
	if !b.metric("courier_leader 0") || !b.metric("courier_leader_takeovers_total 0") {
		t.Error("the standby's /metrics lacks courier_leader 0 and courier_leader_takeovers_total 0")
	}
	if fence.epoch != 1 || len(fence.holders) != 1 || fence.holders[0] != "pod-a_1" {
		t.Errorf("the store was told to lead %v at epoch %d; want pod-a_1 once, at epoch 1", fence.holders, fence.epoch)
	}
	if l, err := client.CoordinationV1().Leases(testElection.Namespace).Get(t.Context(), testElection.Name, metav1.GetOptions{}); err != nil || l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "pod-a_1" {
		t.Errorf("the Lease = %+v, %v; want it held by pod-a_1", l, err)
	}
}

// The leader stops as a signal says: the steps in order, and the Lease handed over, so that the standby takes it
// without waiting a lease duration and starts the leader half, with the next epoch.
func TestRunAStandbyTakesOverWhenTheLeaderStopsGracefully(t *testing.T) {
	client, fence := cluster(), &fakeFence{}
	ca, a := electionConfig(t, client, fence, "pod-a_1")
	a.running = start(t, ca)
	eventually(t, 10*time.Second, "the first replica did not start the leader half", func() bool { return a.built.Load() == 1 })
	cb, b := electionConfig(t, client, fence, "pod-b_1")
	b.running = start(t, cb)
	b.until("/readyz", http.StatusOK)

	start := time.Now()
	a.cancel()
	if err := a.wait(); err != nil {
		t.Fatalf("the leader's Run = %v after a signal; want a clean stop", err)
	}

	eventually(t, 10*time.Second, "the standby did not take over", func() bool { return b.built.Load() == 1 })
	if took := time.Since(start); took > testElection.LeaseDuration {
		t.Errorf("the takeover took %s after a graceful stop; want less than the lease duration, %s", took, testElection.LeaseDuration)
	}
	if !b.metric("courier_leader 1") {
		t.Error("the new leader's /metrics lacks courier_leader 1")
	}
	if fence.epoch != 2 || fence.holders[1] != "pod-b_1" {
		t.Errorf("leads %v at epoch %d; want pod-b_1 second, at epoch 2", fence.holders, fence.epoch)
	}
	select {
	case code := <-a.exits:
		t.Errorf("the old leader's graceful stop ended the process with exit %d: only a lost lease or a stale epoch does", code)
	default:
	}
}

// A standby that leads hours after it started connects with the credentials of now, as a restart would: the tokens were
// rotated while it waited, and the old ones are revoked.
func TestRunTheNewLeaderUsesTheCredentialsOfNowAndNotOfItsStart(t *testing.T) {
	client, fence := cluster(), &fakeFence{}
	ca, a := electionConfig(t, client, fence, "pod-a_1")
	a.running = start(t, ca)
	eventually(t, 10*time.Second, "the first replica did not start the leader half", func() bool { return a.built.Load() == 1 })
	cb, b := electionConfig(t, client, fence, "pod-b_1")
	var token atomic.Value
	cb.newSlack = func(sc slack.Config) slackAPI { token.Store(sc.BotToken.Reveal()); b.built.Add(1); return idleSlack{} }
	b.running = start(t, cb)
	b.until("/readyz", http.StatusOK)

	rotated := secretOf("courier-system", "slack-main", "botToken", "appToken")
	rotated.Data["botToken"] = []byte("rotated-bot-token")
	if _, err := client.CoreV1().Secrets("courier-system").Update(t.Context(), rotated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	a.cancel()

	eventually(t, 10*time.Second, "the standby did not take over", func() bool { return b.built.Load() == 1 })
	if got := token.Load(); got != "rotated-bot-token" {
		t.Errorf("the new leader's bot token = %v; want the rotated one: it read the Secret at its start", got)
	}
}

// A leader that cannot renew its lease ends the process at once, with exit 5 and one line, and without the shutdown
// that would post.
func TestRunTheProcessExitsWhenTheLeaseIsLost(t *testing.T) {
	client, fence := cluster(), &fakeFence{}
	var unreachable atomic.Bool
	client.PrependReactor("*", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if unreachable.Load() {
			return true, nil, errors.New("the API server is unreachable")
		}
		return false, nil, nil
	})
	var steps []string
	var mu sync.Mutex
	ca, a := electionConfig(t, client, fence, "pod-a_1")
	ca.shutdownTrace = func(step string) { mu.Lock(); steps = append(steps, step); mu.Unlock() }
	a.running = start(t, ca)
	eventually(t, 10*time.Second, "the replica did not start the leader half", func() bool { return a.built.Load() == 1 })
	a.until("/readyz", http.StatusOK)

	unreachable.Store(true)

	select {
	case code := <-a.exits:
		if code != ExitLeadershipLost {
			t.Errorf("exit code = %d; want %d", code, ExitLeadershipLost)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the process did not exit after the lease could not be renewed")
	}
	if n := strings.Count(a.logs.String(), "leadership lost"); n != 1 {
		t.Errorf("%d leadership-lost lines in the log; want exactly one:\n%s", n, a.logs.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(steps) != 0 {
		t.Errorf("the shutdown ran steps %v before the exit; want none: the process ends without a drain", steps)
	}
}

// A replica that cannot be given the epoch does not hold the lease for ever: after the time a leader is given to renew,
// Run ends with the error, so that its restart, or the other replica, can try.
func TestRunEndsWhenTheEpochCannotBeTakenAndHandsTheLeaseOver(t *testing.T) {
	client, fence := cluster(), &fakeFence{failing: true}
	ca, a := electionConfig(t, client, fence, "pod-a_1")
	a.running = start(t, ca)

	err := a.waitWithin(testElection.RenewDeadline + 8*time.Second)

	if err == nil || !strings.Contains(err.Error(), "take the leader's epoch") || errors.Is(err, ErrWiring) {
		t.Errorf("Run = %v; want a failure that says the epoch could not be taken", err)
	}
	if a.built.Load() != 0 {
		t.Error("the leader half started on a replica that has no epoch")
	}
	if l, err := client.CoordinationV1().Leases(testElection.Namespace).Get(t.Context(), testElection.Name, metav1.GetOptions{}); err != nil || (l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity != "") {
		t.Errorf("the Lease = %+v, %v; want it handed over, so that the other replica need not wait", l, err)
	}
}

// A standby that is stopped never held anything, and Run returns at once; a signal while it waits is not a failure.
func TestRunAStandbyStopsCleanly(t *testing.T) {
	client, fence := cluster(), &fakeFence{}
	ca, a := electionConfig(t, client, fence, "pod-a_1")
	a.running = start(t, ca)
	eventually(t, 10*time.Second, "the first replica did not start the leader half", func() bool { return a.built.Load() == 1 })
	cb, b := electionConfig(t, client, fence, "pod-b_1")
	b.running = start(t, cb)
	b.until("/readyz", http.StatusOK)

	b.cancel()

	if err := b.waitWithin(5 * time.Second); err != nil {
		t.Errorf("a standby's Run = %v after a signal; want nil", err)
	}
	select {
	case code := <-b.exits:
		t.Errorf("a standby's stop ended the process with exit %d", code)
	default:
	}
	if b.built.Load() != 0 || a.built.Load() != 1 {
		t.Errorf("platforms built: standby %d, leader %d; want 0 and 1", b.built.Load(), a.built.Load())
	}
}

// With SQLite there is no election: the replica does the work at once, makes no Lease, and says courier_leader 1.
func TestRunWithSQLiteHasNoElection(t *testing.T) {
	c := startConfig(t)
	var built atomic.Int32
	c.newSlack = func(slack.Config) slackAPI { built.Add(1); return idleSlack{} }
	r := start(t, c)

	eventually(t, 10*time.Second, "the leader half did not start", func() bool { return built.Load() == 1 })
	r.until("/readyz", http.StatusOK)

	for _, a := range c.Client.(*fake.Clientset).Actions() {
		if a.GetResource().Resource == "leases" {
			t.Errorf("SQLite touched the leases API: %v", a)
		}
	}
	if leases, err := c.Client.(*fake.Clientset).CoordinationV1().Leases("").List(t.Context(), metav1.ListOptions{}); err != nil || len(leases.Items) != 0 {
		t.Errorf("leases = %v, %v; want none with SQLite", leases, err)
	}
	if _, body := r.get("/metrics"); !strings.Contains(body, "courier_leader 1\n") || !strings.Contains(body, "courier_leader_takeovers_total 0\n") {
		t.Errorf("/metrics with SQLite lacks courier_leader 1 and courier_leader_takeovers_total 0:\n%s", body)
	}
}

// A store that fences needs an election: Run says so as a wiring error before it serves ready.
func TestRunRefusesAFencingStoreWithoutAnElection(t *testing.T) {
	c := startConfig(t)
	c.wrapStore = func(s openedStore) openedStore { return fencedSQLite{openedStore: s, fence: &fakeFence{}} }
	r := start(t, c)

	if err := r.wait(); !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "election") {
		t.Errorf("Run = %v; want an ErrWiring about the election", err)
	}
}
