package broker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	got := podLabels(t, client, testPod)
	if _, ok := got[LeaderLabel]; ok || got["app.kubernetes.io/name"] != "agent-kourier" {
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

// A write that reported an error may still have been applied (the API server wrote it, and the answer was cut), so after one the label
// may be on whatever was written before: the stop takes it off.
func TestPodLabelerStopTakesOffALabelWhoseWriteReportedAnError(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(podOf(testPod, nil))
	var n atomic.Int32
	apply := k8stesting.ObjectReaction(client.Tracker())
	client.PrependReactor("patch", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if n.Add(1) != 2 { // the removal at start goes through; the label is written, and its answer lost
			return false, nil, nil
		}
		if _, _, err := apply(a); err != nil {
			return true, nil, err
		}
		return true, nil, errors.New("the connection was reset")
	})
	l, w := newTestLabeler(t, client)
	l.base, l.ceiling = time.Hour, time.Hour // no retry before the stop
	l.start(t.Context())
	<-l.settled()
	l.lead()
	testutil.Eventually(t, "the label's write was not tried", func() bool {
		return slices.Equal(w.list(), []string{labelWriteUnlabeled, labelWriteFailed})
	})
	if !isLeaderPod(t, client, testPod) {
		t.Fatal("the reactor did not apply the write it failed")
	}

	stopLabeler(t, l)
	if isLeaderPod(t, client, testPod) {
		t.Error("the stop left on a label whose write reported an error but was applied")
	}
}

// The labeler's readiness check fails until a write has succeeded: a pod whose removal at start failed may still carry the label from an
// earlier process, and must not be ready, so in the chat Service, as a standby. Once a write has succeeded the check holds, whatever
// fails after.
func TestPodLabelerIsNotReadyUntilAWriteHasSucceeded(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(podOf(testPod, map[string]string{LeaderLabel: LeaderLabelValue}))
	failPodPatches(client, 1)
	l, _ := newTestLabeler(t, client)
	release := make(chan time.Time)
	l.after = func(time.Duration) <-chan time.Time { return release }
	check := l.check()
	if check.Name != "leader-label" {
		t.Errorf("check named %q; want leader-label", check.Name)
	}
	if err := check.Fn(t.Context()); err == nil {
		t.Error("the check holds before the labeler has started")
	}
	l.start(t.Context())
	defer stopLabeler(t, l)
	<-l.settled()
	if err := check.Fn(t.Context()); err == nil {
		t.Error("the check holds after the removal failed; want it to fail while the label may be on")
	}
	close(release) // the removal is tried again, and succeeds
	testutil.Eventually(t, "the check did not hold after the removal succeeded", func() bool { return check.Fn(t.Context()) == nil })
	if isLeaderPod(t, client, testPod) {
		t.Error("the check holds with the label still on")
	}
}

// A first write that the API server refuses because the broker may not patch the pod, or because there is no such pod, can never succeed
// as the broker is deployed (no RBAC, or a wrong pod name or namespace): it is a wiring error, which fails the start. Any other failure
// is tried again, and a success is no error.
func TestPodLabelerStartErrorIsAWiringErrorForAMissingPodOrRBAC(t *testing.T) {
	t.Parallel()
	podsResource := corev1.Resource("pods")
	for name, tc := range map[string]struct {
		pod    bool
		refuse error // the first patch's answer; nil applies it
		wiring bool
	}{
		"applied":             {pod: true},
		"forbidden":           {pod: true, refuse: apierrors.NewForbidden(podsResource, testPod.Name, errors.New("no RBAC")), wiring: true},
		"no such pod":         {pod: false, wiring: true},
		"unreachable":         {pod: true, refuse: errors.New("the API server cannot be reached")},
		"refused for a while": {pod: true, refuse: apierrors.NewServiceUnavailable("the API server is starting")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := fake.NewClientset()
			if tc.pod {
				if err := client.Tracker().Add(podOf(testPod, nil)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.refuse != nil {
				var once sync.Once
				client.PrependReactor("patch", "pods", func(k8stesting.Action) (handled bool, _ runtime.Object, err error) {
					once.Do(func() { handled, err = true, tc.refuse })
					return handled, nil, err
				})
			}
			l, _ := newTestLabeler(t, client)
			if err := l.startError(); err != nil {
				t.Errorf("startError = %v before the first write; want nil", err)
			}
			l.start(t.Context())
			defer stopLabeler(t, l)
			<-l.settled()
			err := l.startError()
			switch {
			case tc.wiring && !errors.Is(err, ErrWiring):
				t.Errorf("startError = %v; want an ErrWiring", err)
			case tc.wiring && !strings.Contains(err.Error(), testPod.Namespace+"/"+testPod.Name):
				t.Errorf("startError = %v; want it to name the pod", err)
			case !tc.wiring && err != nil:
				t.Errorf("startError = %v; want nil", err)
			}
		})
	}
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
