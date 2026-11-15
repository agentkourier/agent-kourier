package broker

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// testPod is the pod a labeler of these tests runs in.
var testPod = PodRef{Namespace: "agent-kourier", Name: "agent-kourier-7d9f8-abcde"}

// podOf is a pod named by ref with the labels given, as the Deployment's ReplicaSet makes it.
func podOf(ref PodRef, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name, Labels: labels}}
}

// podLabels reads the labels of the pod ref in client, failing the test if there is no such pod.
func podLabels(t *testing.T, client *fake.Clientset, ref PodRef) map[string]string {
	t.Helper()
	p, err := client.CoreV1().Pods(ref.Namespace).Get(context.Background(), ref.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s/%s: %v", ref.Namespace, ref.Name, err)
	}
	return p.Labels
}

// isLeaderPod says whether the pod ref carries the leader label.
func isLeaderPod(t *testing.T, client *fake.Clientset, ref PodRef) bool {
	t.Helper()
	return podLabels(t, client, ref)[LeaderLabel] == LeaderLabelValue
}

// patches counts the patches of pods that client was asked for, the refused ones included.
func patches(client *fake.Clientset) int {
	n := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "pods" {
			n++
		}
	}
	return n
}

// writes records what a labeler reported of its writes, in order.
type writes struct {
	mu  sync.Mutex
	got []string
}

func (w *writes) record(result string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, result)
}

func (w *writes) list() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.got)
}

// failPodPatches makes the first n patches of pods in client fail, as an API server that cannot be reached would; n < 0 fails every one.
func failPodPatches(client *fake.Clientset, n int) {
	var mu sync.Mutex
	client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if n == 0 {
			return false, nil, nil
		}
		n--
		return true, nil, errors.New("the API server cannot be reached")
	})
}

// newTestLabeler is a labeler of testPod in client whose retries wait a few milliseconds, and the record of its writes.
func newTestLabeler(t *testing.T, client *fake.Clientset) (*podLabeler, *writes) {
	t.Helper()
	w := &writes{}
	l := newPodLabeler(client, testPod, testutil.DiscardLogger(), w.record)
	l.base, l.ceiling = 5*time.Millisecond, 20*time.Millisecond
	return l, w
}

// stopLabeler stops l within the bound a shutdown step would give it.
func stopLabeler(t *testing.T, l *podLabeler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	l.stop(ctx)
}

// A pod that kept the label from an earlier process (a container that exited as the leader and was restarted in the same pod) is no
// leader now: the labeler takes the label off as it starts, and keeps the pod's other labels.
func TestPodLabelerTakesAStaleLabelOffAtStart(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(podOf(testPod, map[string]string{LeaderLabel: LeaderLabelValue, "app.kubernetes.io/name": "agent-kourier"}))
	l, w := newTestLabeler(t, client)
	l.start(t.Context())
	defer stopLabeler(t, l)

	select {
	case <-l.settled():
	case <-time.After(5 * time.Second):
		t.Fatal("the labeler did not settle its first write")
	}
	if got := podLabels(t, client, testPod); got[LeaderLabel] != "" || got["app.kubernetes.io/name"] != "agent-kourier" {
		t.Errorf("labels %v after the start; want the leader label gone and the others kept", got)
	}
	if got := w.list(); !slices.Equal(got, []string{labelWriteUnlabeled}) {
		t.Errorf("writes %v; want one removal", got)
	}
}

// The leader labels its pod, and keeps the pod's other labels: the chat Service selects the pod by the selector labels and this one.
func TestPodLabelerLabelsThePodWhenItLeads(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(podOf(testPod, map[string]string{"app.kubernetes.io/name": "agent-kourier"}))
	l, w := newTestLabeler(t, client)
	l.start(t.Context())
	defer stopLabeler(t, l)
	<-l.settled()

	l.lead()
	testutil.Eventually(t, "the leader's pod was not labeled", func() bool { return isLeaderPod(t, client, testPod) })
	if got := podLabels(t, client, testPod); got["app.kubernetes.io/name"] != "agent-kourier" {
		t.Errorf("labels %v; want the pod's other labels kept", got)
	}
	testutil.Eventually(t, "the label write was not reported", func() bool {
		return slices.Equal(w.list(), []string{labelWriteUnlabeled, labelWriteLabeled})
	})
}

// A write that fails is logged, counted and tried again, after a wait that doubles from the base to the ceiling, until it succeeds: a
// failed write never stops the broker leading. The first attempt settles the start whether or not it succeeded, so a start is not held
// by an API server that does not answer.
func TestPodLabelerRetriesAFailedWriteWithADoublingWait(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(podOf(testPod, nil))
	failPodPatches(client, 4)
	l, w := newTestLabeler(t, client)
	var mu sync.Mutex
	var waits []time.Duration
	l.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return time.After(time.Millisecond)
	}
	l.lead() // before the start: the first write is the label, not its removal
	l.start(t.Context())
	defer stopLabeler(t, l)

	select {
	case <-l.settled():
	case <-time.After(5 * time.Second):
		t.Fatal("a failed first write did not settle the start")
	}
	testutil.Eventually(t, "the pod was not labeled after the failures", func() bool { return isLeaderPod(t, client, testPod) })
	want := []string{labelWriteFailed, labelWriteFailed, labelWriteFailed, labelWriteFailed, labelWriteLabeled}
	testutil.Eventually(t, "the writes were not reported", func() bool { return slices.Equal(w.list(), want) })
	mu.Lock()
	defer mu.Unlock()
	if wantWaits := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond}; !slices.Equal(waits, wantWaits) {
		t.Errorf("waits %v; want %v: doubling from the base, held at the ceiling", waits, wantWaits)
	}
}

// Leadership that comes while a failed write waits to be tried again is written at once: the wait is for the API server, and the label
// is what lets the chat Service reach the new leader.
func TestPodLabelerWritesAtOnceWhenLeadershipComesDuringAWait(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(podOf(testPod, nil))
	failPodPatches(client, 1)
	l, _ := newTestLabeler(t, client)
	l.base, l.ceiling = time.Hour, time.Hour // a wait the test would never see the end of
	l.start(t.Context())
	defer stopLabeler(t, l)
	<-l.settled()

	l.lead()
	testutil.Eventually(t, "leadership did not end the wait", func() bool { return isLeaderPod(t, client, testPod) })
}

// At shutdown the leader takes its label off, once the loop has stopped, so the chat Service no longer selects a pod that is going away.
func TestPodLabelerStopTakesTheLabelOff(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(podOf(testPod, nil))
	l, w := newTestLabeler(t, client)
	l.start(t.Context())
	<-l.settled()
	l.lead()
	testutil.Eventually(t, "the pod was not labeled", func() bool { return isLeaderPod(t, client, testPod) })

	stopLabeler(t, l)
	if isLeaderPod(t, client, testPod) {
		t.Error("the label is still on after the stop")
	}
	if got := w.list(); !slices.Equal(got, []string{labelWriteUnlabeled, labelWriteLabeled, labelWriteUnlabeled}) {
		t.Errorf("writes %v; want the removal at start, the label, and its removal at the stop", got)
	}
}

// A replica that never led, and whose removal at start succeeded, writes nothing at shutdown: its pod is known to carry no label. One
// whose removal never succeeded tries once more.
func TestPodLabelerStopWritesOnlyWhenTheLabelMayBeOn(t *testing.T) {
	t.Parallel()
	t.Run("known off", func(t *testing.T) {
		t.Parallel()
		client := fake.NewClientset(podOf(testPod, nil))
		l, _ := newTestLabeler(t, client)
		l.start(t.Context())
		<-l.settled()
		before := patches(client)
		stopLabeler(t, l)
		if after := patches(client); after != before {
			t.Errorf("the stop patched the pod %d times; want none", after-before)
		}
	})
	t.Run("never written", func(t *testing.T) {
		t.Parallel()
		client := fake.NewClientset(podOf(testPod, map[string]string{LeaderLabel: LeaderLabelValue}))
		failPodPatches(client, 1)
		l, _ := newTestLabeler(t, client)
		l.base, l.ceiling = time.Hour, time.Hour // the failed removal is not tried again before the stop
		l.start(t.Context())
		<-l.settled()
		stopLabeler(t, l)
		if isLeaderPod(t, client, testPod) {
			t.Error("the stop left the label on a pod whose removal had failed")
		}
	})
}

// The stop is safe without a start (a Run that failed before the labeler started) and when called twice.
func TestPodLabelerStopWithoutAStart(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(podOf(testPod, map[string]string{LeaderLabel: LeaderLabelValue}))
	l, _ := newTestLabeler(t, client)
	stopLabeler(t, l)
	stopLabeler(t, l)
	if isLeaderPod(t, client, testPod) {
		t.Error("a stop without a start left the label on")
	}
}
