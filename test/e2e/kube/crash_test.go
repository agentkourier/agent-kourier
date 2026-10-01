package kube

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var podsGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

const courierSel = "app.kubernetes.io/name=courier"

// courierPod is a courier pod whose container "courier" has restarted restarts times, with last as its
// last termination and now as its state, either of which may be nil.
func courierPod(restarts int32, last, now *corev1.ContainerStateTerminated) *corev1.Pod {
	status := corev1.ContainerStatus{Name: "courier", RestartCount: restarts}
	if last != nil {
		status.LastTerminationState = corev1.ContainerState{Terminated: last}
	}
	if now != nil {
		status.State = corev1.ContainerState{Terminated: now}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "courier", Name: "courier-abc", Labels: map[string]string{"app.kubernetes.io/name": "courier"}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "sidecar", RestartCount: 9}, status}},
	}
}

func killed() *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error", FinishedAt: metav1.NewTime(time.Unix(1_700_000_000, 0))}
}

// Restarts is the baseline a scenario takes before it arms a fault; WaitCrash then waits for a
// termination after it, and an earlier one never answers.
func TestWaitCrash_AnswersOnlyForACrashAfterTheBaseline(t *testing.T) {
	cs := fake.NewClientset(courierPod(2, &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}, nil))
	p := newPods(cs)
	p.poll = time.Millisecond
	baseline, err := p.Restarts(t.Context(), "courier", courierSel, "courier")
	if err != nil || baseline != 2 {
		t.Fatalf("Restarts = %d, %v; want 2 (the sidecar's 9 is another container)", baseline, err)
	}
	// The container crashes after a few polls: until then the old termination, at the same count, is not an answer.
	var lists atomic.Int32
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if lists.Add(1) == 4 {
			_ = cs.Tracker().Update(podsGVR, courierPod(3, killed(), nil), "courier")
		}
		return false, nil, nil
	})
	got, err := p.WaitCrash(t.Context(), "courier", courierSel, "courier", baseline)
	if err != nil || got.ExitCode != 137 || got.Reason != "Error" || got.Restarts != 3 {
		t.Fatalf("WaitCrash = %+v, %v; want exit 137 at restart 3", got, err)
	}
	if lists.Load() < 4 {
		t.Fatalf("WaitCrash answered after %d lists, on the termination that was there before the baseline", lists.Load())
	}
}

// A container that has crashed and not yet been restarted shows the crash in its state, not in its
// last state, and is an answer too; a clean exit is not a crash.
func TestWaitCrash_SeesACrashThatIsStillTheState(t *testing.T) {
	cs := fake.NewClientset(courierPod(0, nil, killed()))
	p := newPods(cs)
	p.poll = time.Millisecond
	got, err := p.WaitCrash(t.Context(), "courier", courierSel, "courier", 0)
	if err != nil || got.ExitCode != 137 {
		t.Fatalf("WaitCrash = %+v, %v; want exit 137", got, err)
	}
	clean := fake.NewClientset(courierPod(1, &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}, nil))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := newPodsPolling(clean).WaitCrash(ctx, "courier", courierSel, "courier", 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a clean exit: %v; want it not to count as a crash", err)
	}
}

// With no crash the wait ends with ctx, and the error says what it last saw. A failed list ends it
// at once: a missing permission must not look like a container that has not crashed yet.
func TestWaitCrash_ReportsWhatItSawAndStopsOnAListError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := newPodsPolling(fake.NewClientset(courierPod(0, nil, nil))).WaitCrash(ctx, "courier", courierSel, "courier", 0)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "has not crashed") {
		t.Errorf("WaitCrash = %v; want a deadline error saying the container has not crashed", err)
	}
	_, err = newPodsPolling(fake.NewClientset()).WaitCrash(ctx, "courier", "app=other", "nosuch", 0)
	if err == nil || !strings.Contains(err.Error(), "no pod") {
		t.Errorf("no pod: %v; want an error naming it", err)
	}
	forbidden := fake.NewClientset()
	forbidden.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pods is forbidden")
	})
	started := time.Now()
	if _, err := newPodsPolling(forbidden).WaitCrash(t.Context(), "courier", courierSel, "courier", 0); err == nil || !strings.Contains(err.Error(), "forbidden") || time.Since(started) > 5*time.Second {
		t.Errorf("a forbidden list: %v after %v; want the error at once", err, time.Since(started))
	}
}

func newPodsPolling(cs *fake.Clientset) *Pods {
	p := newPods(cs)
	p.poll = time.Millisecond
	return p
}
