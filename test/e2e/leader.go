package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
)

// Courier's container and the label of its pod (charts/courier, as the tiers install it); its namespace and Deployment
// are kube.CourierNamespace and kube.CourierDeployment.
const (
	courierContainer = "courier"
	courierPods      = "app.kubernetes.io/name=courier"
	// courierMetricsPort is the port of a pod's health and metrics (the chart's listen.port, 8080).
	courierMetricsPort = "8080"
	// askTimeout is how long one question to one replica may take: a frozen process does not answer, and must not hold
	// up the question to the other.
	askTimeout = 2 * time.Second
)

// replicaURL is where a replica's health and metrics are: its pod's IP, on courier's port.
func replicaURL(r kube.Replica) string {
	return "http://" + net.JoinHostPort(r.IP, courierMetricsPort)
}

// Replicas finds courier's running processes and says which of them leads. With one replica that is the only one; with
// two on Postgres it is the one that holds the Lease (spec 11, D37), which courier says in courier_leader. The Service
// of courier's metrics port spreads its calls over the replicas, so a scenario that reads a counter of the leader, or
// asks which one leads, goes to a pod's own address.
type Replicas struct {
	list func(ctx context.Context) ([]kube.Replica, error)
	base func(kube.Replica) string
	ask  time.Duration
}

// NewReplicas returns the finder for courier's pods, listed through pods.
func NewReplicas(pods *kube.Pods) *Replicas {
	return &Replicas{
		list: func(ctx context.Context) ([]kube.Replica, error) {
			return pods.Replicas(ctx, kube.CourierNamespace, courierPods, courierContainer)
		},
		base: replicaURL,
		ask:  askTimeout,
	}
}

// All returns the processes that are running now, in the order the API lists them.
func (r *Replicas) All(ctx context.Context) ([]kube.Replica, error) { return r.list(ctx) }

// Counter reads one sample of the replica's own /metrics (see Courier.Counter). It is for a counter or a gauge of one
// process: courier's counters start again at 0 with each process.
func (r *Replicas) Counter(ctx context.Context, rep kube.Replica, name string, labels map[string]string) (float64, error) {
	base := r.base(rep)
	if base == "" {
		return 0, fmt.Errorf("e2e: no address for replica %s", rep.ID())
	}
	ctx, cancel := context.WithTimeout(ctx, r.ask)
	defer cancel()
	return NewCourier(base).Counter(ctx, name, labels)
}

// Leaders returns the replicas that say they lead (courier_leader is 1). A replica that does not answer in time is no
// leader: a frozen one does not answer, and neither does one that has just started. There may be two for a moment, a
// deposed leader that has not yet exited and its successor, which is why a scenario waits with WaitLeader for one.
func (r *Replicas) Leaders(ctx context.Context) ([]kube.Replica, error) {
	all, err := r.list(ctx)
	if err != nil {
		return nil, err
	}
	var leaders []kube.Replica
	for _, rep := range all {
		if v, err := r.Counter(ctx, rep, "courier_leader", nil); err == nil && v == 1 {
			leaders = append(leaders, rep)
		}
	}
	return leaders, nil
}

// WaitLeader waits up to timeout until exactly one replica leads, its chat connection is up (courier_chat_connected
// is 1: the connection to Slack is what takes events in) and it is none of the processes named in not (by ID: the
// replicas that were leaders before an event, whose end or takeover is what a scenario waits for). It returns that
// replica. A restart in the same pod is another process, so the pod that was the leader may be the answer after a
// crash, once it has come back as a new process and won the Lease.
func (r *Replicas) WaitLeader(ctx context.Context, timeout time.Duration, not ...string) (kube.Replica, error) {
	var found kube.Replica
	desc := "exactly one connected leader"
	if len(not) > 0 {
		desc += fmt.Sprintf(" other than %v", not)
	}
	err := Wait(ctx, timeout, desc, func(ctx context.Context) (bool, error) {
		leaders, err := r.Leaders(ctx)
		if err != nil || len(leaders) != 1 || slices.Contains(not, leaders[0].ID()) {
			return false, err
		}
		up, err := r.Counter(ctx, leaders[0], "courier_chat_connected", nil)
		if err != nil || up != 1 {
			return false, err
		}
		found = leaders[0]
		return true, nil
	})
	if err != nil {
		return kube.Replica{}, err
	}
	return found, nil
}

// errNoReplica says a scenario asked for a replica that no process is.
var errNoReplica = errors.New("e2e: no such replica")

// Other returns the replica of all that is not the process named by id: the standby of a pair whose leader that is. It is
// an error if there is not exactly one other.
func Other(all []kube.Replica, id string) (kube.Replica, error) {
	var others []kube.Replica
	for _, r := range all {
		if r.ID() != id {
			others = append(others, r)
		}
	}
	if len(others) != 1 {
		return kube.Replica{}, fmt.Errorf("%w: %d replicas besides %s, want exactly one", errNoReplica, len(others), id)
	}
	return others[0], nil
}
