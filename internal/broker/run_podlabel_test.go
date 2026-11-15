package broker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	k8stesting "k8s.io/client-go/testing"

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

// slowNextPatch is a client whose next patch of a pod, once armed, waits delay before it reaches the cluster, or until its context ends;
// the patch disarms it. The fake's own reactors cannot stand in for it: the fake holds one lock while a reactor runs, which would hold
// every other call of the replica too.
type slowNextPatch struct {
	kubernetes.Interface
	delay time.Duration
	armed *atomic.Bool
}

// slowedFirstPatch is a client of cluster whose first patch of a pod is slowed by delay.
func slowedFirstPatch(cluster kubernetes.Interface, delay time.Duration) slowNextPatch {
	s := slowNextPatch{Interface: cluster, delay: delay, armed: &atomic.Bool{}}
	s.armed.Store(true)
	return s
}

// IsWatchListSemanticsUnSupported passes on what the fake says of itself: the informers ask it, and would wait for a watch-list stream the
// fake never sends were the wrapper to hide it.
func (s slowNextPatch) IsWatchListSemanticsUnSupported() bool { return true }

func (s slowNextPatch) CoreV1() corev1client.CoreV1Interface {
	return slowCore{CoreV1Interface: s.Interface.CoreV1(), s: s}
}

type slowCore struct {
	corev1client.CoreV1Interface
	s slowNextPatch
}

func (c slowCore) Pods(namespace string) corev1client.PodInterface {
	return slowPods{PodInterface: c.CoreV1Interface.Pods(namespace), s: c.s}
}

type slowPods struct {
	corev1client.PodInterface
	s slowNextPatch
}

func (p slowPods) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, sub ...string) (*corev1.Pod, error) {
	if p.s.armed.CompareAndSwap(true, false) {
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
	got := podLabels(t, client, testPod)
	if _, ok := got[LeaderLabel]; ok || got["app.kubernetes.io/name"] != "agent-kourier" {
		t.Errorf("labels %v after the shutdown; want the leader label gone and the others kept", got)
	}
}

// leaderLabelOnAt records, when the replica of pod labels it (the "on" patch reaches the cluster), whether other still carries the label:
// the chat Service must never select two pods.
func leaderLabelOnAt(t *testing.T, client *fake.Clientset, pod, other PodRef) (seen func() (seen, otherOn bool)) {
	t.Helper()
	var mu sync.Mutex
	var saw, on bool
	client.PrependReactor("patch", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p, ok := a.(k8stesting.PatchAction)
		if !ok || p.GetNamespace() != pod.Namespace || p.GetName() != pod.Name || !bytes.Contains(p.GetPatch(), []byte(`"`+LeaderLabelValue+`"`)) {
			return false, nil, nil
		}
		obj, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), other.Namespace, other.Name)
		if err != nil {
			t.Errorf("get pod %s/%s: %v", other.Namespace, other.Name, err)
			return false, nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if !saw {
			saw, on = true, obj.(*corev1.Pod).Labels[LeaderLabel] == LeaderLabelValue
		}
		return false, nil, nil // the default reactor applies it
	})
	return func() (bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		return saw, on
	}
}

// Of two replicas only the leader's pod carries the label, and a standby whose pod kept it from an earlier process (a leader that exited
// and was restarted in the same pod) has it taken off before it says it is ready, so the chat Service never selects a standby that
// answers 503. When the leader stops, its label comes off before it lets go of the lease, so it is off before the replica that takes
// over labels its own pod.
func TestRunOnlyTheLeadersPodCarriesTheLabelAndItMovesOnATakeover(t *testing.T) {
	client, fence := cluster(), &fakeFence{}
	podA, podB := PodRef{Namespace: "agent-kourier", Name: "pod-a"}, PodRef{Namespace: "agent-kourier", Name: "pod-b"}
	bLabeled := leaderLabelOnAt(t, client, podB, podA)
	ca, a := electionConfig(t, client, fence, "pod-a_1")
	labelPod(t, &ca, client, podA, nil)
	// The leader's removal at shutdown is slowed, though within its bound, so that a leader that let go of the lease first would leave
	// its label on long enough for the other replica to take over and label its own pod.
	aClient := slowNextPatch{Interface: client, delay: podLabelTimeout * 8 / 10, armed: &atomic.Bool{}}
	ca.Client = aClient
	a.running = start(t, ca)
	testutil.Eventually(t, "the first replica did not start the leader half", func() bool { return a.built.Load() == 1 }, testutil.Within(10*time.Second), testutil.Every(10*time.Millisecond))
	testutil.Eventually(t, "the leader's pod was not labeled", func() bool { return isLeaderPod(t, client, podA) })

	cb, b := electionConfig(t, client, fence, "pod-b_1")
	labelPod(t, &cb, client, podB, map[string]string{LeaderLabel: LeaderLabelValue})
	// The standby's first write is slow, as on a busy API server, though within its bound, so that a replica that said it was ready before
	// the write had been tried would be seen ready with the label still on.
	cb.Client = slowedFirstPatch(client, podLabelTimeout*8/10)
	b.running = start(t, cb)
	b.until("/readyz", http.StatusOK)
	if isLeaderPod(t, client, podB) {
		t.Error("the standby is ready with the leader label it kept from an earlier process; want it taken off first")
	}
	time.Sleep(testElection.RetryPeriod * 3) // the standby tries the lease and does not get it
	if isLeaderPod(t, client, podB) || !isLeaderPod(t, client, podA) {
		t.Errorf("leader label on pod-a %v, pod-b %v; want it on the leader's pod alone", isLeaderPod(t, client, podA), isLeaderPod(t, client, podB))
	}

	aClient.armed.Store(true)
	a.cancel()
	if err := a.wait(); err != nil {
		t.Fatalf("the leader's Run = %v after a signal; want a clean stop", err)
	}
	if isLeaderPod(t, client, podA) {
		t.Error("the old leader's pod still carries the label after its shutdown")
	}
	testutil.Eventually(t, "the replica that took over did not label its pod", func() bool { return isLeaderPod(t, client, podB) }, testutil.Within(10*time.Second))
	if seen, aOn := bLabeled(); !seen || aOn {
		t.Errorf("when pod-b was labeled (seen %v), pod-a still carried the label: %v; want it off before the lease was let go", seen, aOn)
	}
}

// A pod that the broker may not patch (no Role), or that is not there (a wrong pod name or namespace), can never be labeled: the start
// fails with a wiring error, which exits 2, rather than a replica that says it is ready while the chat Service can never reach it.
func TestRunRefusesToStartWhenItsPodCannotBePatched(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pod    bool
		refuse error
	}{
		"forbidden":   {pod: true, refuse: apierrors.NewForbidden(corev1.Resource("pods"), testPod.Name, errors.New("no RBAC"))},
		"no such pod": {pod: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := cluster()
			if tc.refuse != nil {
				client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, tc.refuse })
			}
			c := startConfig(t)
			runsAlongside(&c)
			if tc.pod {
				labelPod(t, &c, client, testPod, nil)
			} else {
				c.Client, c.LeaderPod = client, testPod
				chatListen(t, &c, http.NotFoundHandler())
			}
			r := start(t, c)
			select {
			case err := <-r.done:
				r.done <- err // for the cleanup
				if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "leader label") {
					t.Errorf("Run = %v; want an ErrWiring about the leader label", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not fail its start")
			}
		})
	}
}

// A label that cannot be written, as when the API server cannot be reached, does not stop the broker leading: the write is logged as an
// error, counted as failed and tried again. The pod is not ready meanwhile, since it may still carry the label from an earlier process.
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
	started()

	r.waitMetric(`agentkourier_leader 1`)
	testutil.Eventually(t, "no failed write was counted", func() bool {
		_, body := r.get("/metrics")
		return strings.Contains(body, `agentkourier_leader_pod_label_writes_total{result="failed"}`)
	})
	testutil.Eventually(t, "the failed write was not logged as an error", func() bool {
		return strings.Contains(logs.String(), `level=ERROR msg="writing the leader label of the pod failed; trying again"`)
	})
	if code, body := r.get("/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "leader-label:") {
		t.Errorf("/readyz = %d %q; want 503 naming the leader-label check", code, body)
	}
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
