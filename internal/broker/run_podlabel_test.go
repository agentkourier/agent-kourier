package broker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// labelPod gives c a chat listener, a cluster that holds the pod ref with the labels given, and the pod as the one Run runs in.
func labelPod(t *testing.T, c *RunConfig, client *fake.Clientset, ref PodRef, labels map[string]string) {
	t.Helper()
	if err := client.Tracker().Add(podOf(ref, labels)); err != nil {
		t.Fatal(err)
	}
	c.Client = client
	c.LeaderPod = ref
	if c.ChatListener == nil {
		chatListen(t, c, http.NotFoundHandler())
	}
}

// slowFirstPatch is a client whose first patch of a pod waits delay before it reaches the cluster, or until its context ends. The fake's
// own reactors cannot stand in for it: the fake holds one lock while a reactor runs, which would hold every other call of the replica too.
type slowFirstPatch struct {
	kubernetes.Interface
	delay time.Duration
	done  *atomic.Bool
}

// IsWatchListSemanticsUnSupported passes on what the fake says of itself: the informers ask it, and would wait for a watch-list stream the
// fake never sends were the wrapper to hide it.
func (s slowFirstPatch) IsWatchListSemanticsUnSupported() bool { return true }

func (s slowFirstPatch) CoreV1() corev1client.CoreV1Interface {
	return slowCore{CoreV1Interface: s.Interface.CoreV1(), s: s}
}

type slowCore struct {
	corev1client.CoreV1Interface
	s slowFirstPatch
}

func (c slowCore) Pods(namespace string) corev1client.PodInterface {
	return slowPods{PodInterface: c.CoreV1Interface.Pods(namespace), s: c.s}
}

type slowPods struct {
	corev1client.PodInterface
	s slowFirstPatch
}

func (p slowPods) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, sub ...string) (*corev1.Pod, error) {
	if p.s.done.CompareAndSwap(false, true) {
		select {
		case <-time.After(p.s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return p.PodInterface.Patch(ctx, name, pt, data, opts, sub...)
}

// With one replica (SQLite, no election) the replica is the leader, so it labels its pod for the chat Service once its leader half has
// started, and takes the label off at shutdown. The writes are counted.
func TestRunTheOnlyReplicaLabelsItsPodAndTakesTheLabelOffAtShutdown(t *testing.T) {
	t.Parallel()
	client := cluster()
	c := startConfig(t)
	runsAlongside(&c)
	labelPod(t, &c, client, testPod, map[string]string{"app.kubernetes.io/name": "agent-kourier"})
	started := leaderUp(t, &c)
	r := start(t, c)
	started()

	testutil.Eventually(t, "the leader's pod was not labeled", func() bool { return isLeaderPod(t, client, testPod) })
	r.waitMetric(`agentkourier_leader_pod_label_writes_total{result="labeled"} 1`)
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v after a signal; want a clean stop", err)
	}
	if got := podLabels(t, client, testPod); got[LeaderLabel] != "" || got["app.kubernetes.io/name"] != "agent-kourier" {
		t.Errorf("labels %v after the shutdown; want the leader label gone and the others kept", got)
	}
}

// Of two replicas only the leader's pod carries the label, and a standby whose pod kept it from an earlier process (a leader that exited
// and was restarted in the same pod) has it taken off before it says it is ready, so the chat Service never selects a standby that
// answers 503. When the leader stops, its label comes off and the replica that takes over labels its own pod.
func TestRunOnlyTheLeadersPodCarriesTheLabelAndItMovesOnATakeover(t *testing.T) {
	client, fence := cluster(), &fakeFence{}
	podA, podB := PodRef{Namespace: "agent-kourier", Name: "pod-a"}, PodRef{Namespace: "agent-kourier", Name: "pod-b"}
	ca, a := electionConfig(t, client, fence, "pod-a_1")
	labelPod(t, &ca, client, podA, nil)
	a.running = start(t, ca)
	testutil.Eventually(t, "the first replica did not start the leader half", func() bool { return a.built.Load() == 1 }, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))
	testutil.Eventually(t, "the leader's pod was not labeled", func() bool { return isLeaderPod(t, client, podA) })

	cb, b := electionConfig(t, client, fence, "pod-b_1")
	labelPod(t, &cb, client, podB, map[string]string{LeaderLabel: LeaderLabelValue})
	// The standby's first write is slow, as on a busy API server, though within its bound, so that a replica that said it was ready before
	// the write had been tried would be seen ready with the label still on.
	cb.Client = slowFirstPatch{Interface: client, delay: podLabelTimeout * 8 / 10, done: &atomic.Bool{}}
	b.running = start(t, cb)
	b.until("/readyz", http.StatusOK)
	if isLeaderPod(t, client, podB) {
		t.Error("the standby is ready with the leader label it kept from an earlier process; want it taken off first")
	}
	time.Sleep(testElection.RetryPeriod * 3) // the standby tries the lease and does not get it
	if isLeaderPod(t, client, podB) || !isLeaderPod(t, client, podA) {
		t.Errorf("leader label on pod-a %v, pod-b %v; want it on the leader's pod alone", isLeaderPod(t, client, podA), isLeaderPod(t, client, podB))
	}

	a.cancel()
	if err := a.wait(); err != nil {
		t.Fatalf("the leader's Run = %v after a signal; want a clean stop", err)
	}
	if isLeaderPod(t, client, podA) {
		t.Error("the old leader's pod still carries the label after its shutdown")
	}
	testutil.Eventually(t, "the replica that took over did not label its pod", func() bool { return isLeaderPod(t, client, podB) }, testutil.Within(10*time.Second))
}

// A label that cannot be written does not stop the broker leading, or being ready: the write is logged, counted as failed and tried again.
func TestRunLeadsWhenItsPodCannotBeLabeled(t *testing.T) {
	t.Parallel()
	client := cluster()
	failPodPatches(client, -1)
	c := startConfig(t)
	runsAlongside(&c)
	logs := &logBuffer{}
	c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	labelPod(t, &c, client, testPod, nil)
	started := leaderUp(t, &c)
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	started()

	r.waitMetric(`agentkourier_leader 1`)
	testutil.Eventually(t, "no failed write was counted", func() bool {
		_, body := r.get("/metrics")
		return strings.Contains(body, `agentkourier_leader_pod_label_writes_total{result="failed"}`)
	})
	testutil.Eventually(t, "the failed write was not logged", func() bool {
		return strings.Contains(logs.String(), "writing the leader label of the pod failed; trying again")
	})
	if isLeaderPod(t, client, testPod) {
		t.Error("the pod is labeled although every write failed")
	}
}

// A broker that is not told its pod labels nothing, chat listener or not: no pod is patched, and there is no series of the writes.
func TestRunWithoutItsPodLabelsNothing(t *testing.T) {
	t.Parallel()
	client := cluster()
	c := startConfig(t)
	runsAlongside(&c)
	c.Client = client
	chatListen(t, &c, http.NotFoundHandler())
	started := leaderUp(t, &c)
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	started()

	if n := patches(client); n != 0 {
		t.Errorf("%d patches of pods; want none", n)
	}
	if _, body := r.get("/metrics"); strings.Contains(body, "agentkourier_leader_pod_label_writes_total{") {
		t.Error("/metrics has a series of the label's writes; want none without a pod to label")
	}
}

// The pod is labeled for the chat Service, so naming it without a chat listener, or naming half of it, is a wiring error.
func TestValidateRefusesALeaderPodWithoutAChatListenerOrAName(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pod      PodRef
		listener bool
		want     string // "" is accepted
	}{
		"none":                  {PodRef{}, false, ""},
		"none, with a listener": {PodRef{}, true, ""},
		"a pod and a listener":  {testPod, true, ""},
		"a pod and no listener": {testPod, false, "LeaderPod is set without a ChatListener"},
		"a namespace alone":     {PodRef{Namespace: "agent-kourier"}, true, "LeaderPod must be set to a namespace and a name"},
		"a name alone":          {PodRef{Name: "pod-a"}, true, "LeaderPod must be set to a namespace and a name"},
	} {
		t.Run(name, func(t *testing.T) {
			c := valid(t)
			c.LeaderPod = tc.pod
			if tc.listener {
				c.ChatListener = c.Listener // any listener: validate only looks at whether there is one
			}
			err := c.validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("validate = %v; want nil", err)
			case tc.want != "" && (!errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("validate = %v; want an ErrWiring saying %q", err, tc.want)
			}
		})
	}
}
