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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var podsGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

const courierSel = "app.kubernetes.io/name=courier"

// courierPod is a courier pod with the given UID whose container "courier" has restarted restarts times,
// with last as its last termination and now as its state, either of which may be nil.
func courierPod(uid string, restarts int32, last, now *corev1.ContainerStateTerminated) *corev1.Pod {
	status := corev1.ContainerStatus{Name: "courier", RestartCount: restarts}
	if last != nil {
		status.LastTerminationState = corev1.ContainerState{Terminated: last}
	}
	if now != nil {
		status.State = corev1.ContainerState{Terminated: now}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "courier", Name: "courier-abc", UID: types.UID(uid), Labels: map[string]string{"app.kubernetes.io/name": "courier"}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "sidecar", RestartCount: 9}, status}},
	}
}

// killed is a crash that finished at second sec.
func killed(sec int64) *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error", FinishedAt: metav1.NewTime(time.Unix(sec, 0))}
}

// expect runs WaitCrash against cs, which `change` alters after the third list, and returns what it says.
func expect(t *testing.T, cs *fake.Clientset, since Baseline, change func()) (Termination, error) {
	t.Helper()
	var lists atomic.Int32
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if lists.Add(1) == 4 && change != nil {
			change()
		}
		return false, nil, nil
	})
	got, err := newPodsPolling(cs).WaitCrash(t.Context(), "courier", courierSel, "courier", since)
	if change != nil && lists.Load() < 4 {
		t.Errorf("WaitCrash answered after %d lists, before the change that makes a crash", lists.Load())
	}
	return got, err
}

func replace(cs *fake.Clientset, pod *corev1.Pod) func() {
	return func() {
		_ = cs.Tracker().Delete(podsGVR, "courier", "courier-abc")
		_ = cs.Tracker().Add(pod)
	}
}

// Baseline is what a scenario takes before the action that causes a crash; WaitCrash then waits for a
// termination after it, and the one that was there before never answers: not the old pod's last, at
// the same restart count, nor a state that was already terminated.
func TestWaitCrash_IgnoresWhatTheBaselineShowed(t *testing.T) {
	for name, old := range map[string]*corev1.Pod{
		"a last termination":   courierPod("u1", 2, killed(100), nil),
		"a terminated state":   courierPod("u1", 2, nil, killed(100)),
		"both, and a restart":  courierPod("u1", 3, killed(100), killed(100)),
		"an earlier exit, too": courierPod("u1", 4, &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: metav1.NewTime(time.Unix(50, 0))}, killed(100)),
	} {
		t.Run(name, func(t *testing.T) {
			cs := fake.NewClientset(old)
			since, err := newPodsPolling(cs).Baseline(t.Context(), "courier", courierSel, "courier")
			if err != nil {
				t.Fatal(err)
			}
			// Nothing new: the wait must not answer on what was there, so it ends with ctx.
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			if _, err := newPodsPolling(cs).WaitCrash(ctx, "courier", courierSel, "courier", since); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("on the baseline's own termination: %v; want it not to answer", err)
			}
			cancel()
			// A new crash of the same pod finishes later, and answers.
			got, err := expect(t, cs, since, replace(cs, courierPod("u1", 5, killed(200), nil)))
			if err != nil || got.ExitCode != 137 || got.FinishedAt.Unix() != 200 || got.Restarts != 5 {
				t.Errorf("WaitCrash = %+v, %v; want the crash that finished at 200", got, err)
			}
		})
	}
}

// Arming a fault is a rollout: the pod that crashes is a new one, whose restart count starts at 0, so a
// baseline taken from the old pod (restarted 3 times) is not a count to pass. Any crash of the new pod
// counts, and the old pod's terminations do not.
func TestWaitCrash_AnswersForTheCrashOfARolloutsNewPod(t *testing.T) {
	cs := fake.NewClientset(courierPod("old", 3, killed(100), nil))
	since, err := newPodsPolling(cs).Baseline(t.Context(), "courier", courierSel, "courier")
	if err != nil {
		t.Fatal(err)
	}
	// The new pod is running, then crashes once, after the old one's last termination was at 100.
	cs2 := fake.NewClientset(courierPod("new", 0, nil, nil))
	got, err := expect(t, cs2, since, replace(cs2, courierPod("new", 1, killed(60), nil)))
	if err != nil || got.ExitCode != 137 || got.Restarts != 1 {
		t.Fatalf("WaitCrash = %+v, %v; want the new pod's first crash, though its count is below the old pod's", got, err)
	}
}

// A container that has crashed and not yet been restarted shows the crash in its state, not in its
// last state, and is an answer too; a clean exit is not a crash.
func TestWaitCrash_SeesACrashThatIsStillTheState(t *testing.T) {
	cs := fake.NewClientset(courierPod("u1", 0, nil, killed(100)))
	got, err := newPodsPolling(cs).WaitCrash(t.Context(), "courier", courierSel, "courier", Baseline{})
	if err != nil || got.ExitCode != 137 {
		t.Fatalf("WaitCrash = %+v, %v; want exit 137", got, err)
	}
	exited := &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}
	for name, pod := range map[string]*corev1.Pod{"in its last state": courierPod("u1", 1, exited, nil), "as its state": courierPod("u1", 0, nil, exited)} {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		if _, err := newPodsPolling(fake.NewClientset(pod)).WaitCrash(ctx, "courier", courierSel, "courier", Baseline{}); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a clean exit %s: %v; want it not to count as a crash", name, err)
		}
		cancel()
	}
}

// With no crash the wait ends with ctx, and the error says what it last saw. A failed list ends it
// at once: a missing permission must not look like a container that has not crashed yet.
func TestWaitCrash_ReportsWhatItSawAndStopsOnAListError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := newPodsPolling(fake.NewClientset(courierPod("u1", 0, nil, nil))).WaitCrash(ctx, "courier", courierSel, "courier", Baseline{})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "has not crashed") {
		t.Errorf("WaitCrash = %v; want a deadline error saying the container has not crashed", err)
	}
	_, err = newPodsPolling(fake.NewClientset()).WaitCrash(ctx, "courier", "app=other", "nosuch", Baseline{})
	if err == nil || !strings.Contains(err.Error(), "no pod") {
		t.Errorf("no pod: %v; want an error naming it", err)
	}
	forbidden := fake.NewClientset()
	forbidden.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pods is forbidden")
	})
	started := time.Now()
	if _, err := newPodsPolling(forbidden).WaitCrash(t.Context(), "courier", courierSel, "courier", Baseline{}); err == nil || !strings.Contains(err.Error(), "forbidden") || time.Since(started) > 5*time.Second {
		t.Errorf("a forbidden list: %v after %v; want the error at once", err, time.Since(started))
	}
}

func newPodsPolling(cs *fake.Clientset) *Pods {
	p := newPods(cs)
	p.poll = time.Millisecond
	return p
}

// Only the named container counts: another container of the pod that crashed is not courier's crash.
func TestWaitCrash_WatchesOnlyTheNamedContainer(t *testing.T) {
	pod := courierPod("u1", 0, nil, nil)
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: killed(100)} // the sidecar
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := newPodsPolling(fake.NewClientset(pod)).WaitCrash(ctx, "courier", courierSel, "courier", Baseline{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a crashed sidecar: %v; want it not to answer for courier", err)
	}
}
