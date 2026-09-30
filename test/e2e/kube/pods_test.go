package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const sinkSel = "app=sink"

func sink(name, uid string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: Namespace, Name: name, UID: types.UID(uid), Labels: map[string]string{"app": "sink"}},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

// newPods returns Pods polling every millisecond, and a channel that gets a value per List call.
func newPods(objs ...runtime.Object) (*Pods, *fake.Clientset, <-chan struct{}) {
	cs := fake.NewClientset(objs...)
	listed := make(chan struct{}, 1024)
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		listed <- struct{}{}
		return false, nil, nil
	})
	p := NewPods(cs)
	p.poll = time.Millisecond
	return p, cs, listed
}

func TestList_FiltersByNamespaceAndSelector(t *testing.T) {
	elsewhere := sink("elsewhere", "u3", true)
	elsewhere.Namespace = "courier"
	other := sink("other", "u2", true)
	other.Labels = map[string]string{"app": "stubagent"}
	p, _, _ := newPods(sink("sink-a", "u1", true), other, elsewhere)

	got, err := p.List(t.Context(), Namespace, sinkSel)
	if err != nil || len(got) != 1 || got[0].Name != "sink-a" {
		t.Fatalf("List = (%d pods, %v), want only sink-a", len(got), err)
	}
}

func TestKill_DeletesWithGraceZero(t *testing.T) {
	p, cs, _ := newPods(sink("sink-a", "u1", true))
	var grace *int64
	cs.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		grace = a.(k8stesting.DeleteActionImpl).DeleteOptions.GracePeriodSeconds
		return false, nil, nil
	})
	if err := p.Kill(t.Context(), Namespace, "sink-a"); err != nil {
		t.Fatal(err)
	}
	if grace == nil || *grace != 0 {
		t.Fatalf("grace period = %v, want 0", grace)
	}
}

func TestWaitReady(t *testing.T) {
	ready, _, _ := newPods(sink("sink-a", "u1", true))
	if err := ready.WaitReady(t.Context(), Namespace, sinkSel); err != nil {
		t.Fatal(err)
	}
	stuck, _, _ := newPods(sink("sink-b", "u2", false))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := stuck.WaitReady(ctx, Namespace, sinkSel)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	for _, want := range []string{"e2e", sinkSel, "not Ready: sink-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not mention %q", err, want)
		}
	}
}

// TestRestart_WaitsForANewReadyUID replaces the pod the way a Deployment does: the old pod stays
// Ready (a graceful shutdown's grace window) until a replacement with a new UID exists, and the
// replacement is not Ready at first. Restart must return neither of them early.
func TestRestart_WaitsForANewReadyUID(t *testing.T) {
	p, cs, listed := newPods(sink("sink-old", "uid-old", true))
	deleted := make(chan struct{})
	cs.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if g := a.(k8stesting.DeleteActionImpl).DeleteOptions.GracePeriodSeconds; g != nil {
			t.Errorf("Restart must delete gracefully, got grace %d", *g)
		}
		close(deleted)
		return true, nil, nil // handled: the old pod lingers, still Ready
	})
	var (
		gotPod *corev1.Pod
		gotErr error
		done   = make(chan struct{})
	)
	go func() {
		gotPod, gotErr = p.Restart(t.Context(), Namespace, sinkSel)
		close(done)
	}()
	// pending waits for three fresh polls, without sleeping, and fails if Restart returned.
	pending := func(why string) {
		for len(listed) > 0 {
			<-listed
		}
		for range 3 {
			select {
			case <-listed:
			case <-done:
				t.Fatalf("Restart returned (%v, %v) %s", gotPod, gotErr, why)
			}
		}
	}

	<-deleted
	pending("while only the old pod existed")
	pods := cs.CoreV1().Pods(Namespace)
	if _, err := pods.Create(t.Context(), sink("sink-new", "uid-new", false), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pending("while the replacement was not Ready")
	if _, err := pods.Update(t.Context(), sink("sink-new", "uid-new", true), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if <-done; gotErr != nil || gotPod.UID != "uid-new" {
		t.Fatalf("Restart = (%v, %v), want the pod with UID uid-new", gotPod, gotErr)
	}
}

func TestRestart_RefusesAmbiguousSelection(t *testing.T) {
	p, _, _ := newPods(sink("a", "u1", true), sink("b", "u2", true))
	if _, err := p.Restart(t.Context(), Namespace, sinkSel); err == nil || !strings.Contains(err.Error(), "2 pods match") {
		t.Fatalf("err = %v, want one saying 2 pods match", err)
	}
}
