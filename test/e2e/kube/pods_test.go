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

// fakePods returns Pods polling every millisecond, and a channel that gets a value per List call.
func fakePods(objs ...runtime.Object) (*Pods, *fake.Clientset, <-chan struct{}) {
	cs := fake.NewClientset(objs...)
	listed := make(chan struct{}, 1024)
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		listed <- struct{}{}
		return false, nil, nil
	})
	p := newPods(cs)
	p.poll = time.Millisecond
	return p, cs, listed
}

func TestList_FiltersByNamespaceAndSelector(t *testing.T) {
	elsewhere := sink("elsewhere", "u3", true)
	elsewhere.Namespace = "courier"
	other := sink("other", "u2", true)
	other.Labels = map[string]string{"app": "stubagent"}
	p, _, _ := fakePods(sink("sink-a", "u1", true), other, elsewhere)

	got, err := p.List(t.Context(), Namespace, sinkSel)
	if err != nil || len(got) != 1 || got[0].Name != "sink-a" {
		t.Fatalf("List = (%d pods, %v), want only sink-a", len(got), err)
	}
}

func TestKill_DeletesWithGraceZero(t *testing.T) {
	p, cs, _ := fakePods(sink("sink-a", "u1", true))
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
	errBoom := errors.New("boom")
	terminating := sink("sink-gone", "u3", true) // Ready=True, but being deleted
	now := metav1.Now()
	terminating.DeletionTimestamp = &now

	tests := []struct {
		name      string
		pods      []runtime.Object
		listErr   error         // the API server's answer to every list
		timeout   time.Duration // the context's deadline
		cancelled bool          // cancel the context before the call
		wantErr   error         // matched with errors.Is; nil means success
		wantMsg   string        // must appear in the error
	}{
		{name: "ready", pods: []runtime.Object{sink("sink-a", "u1", true)}, timeout: time.Minute},
		{name: "not ready", pods: []runtime.Object{sink("sink-b", "u2", false)}, timeout: 30 * time.Millisecond,
			wantErr: context.DeadlineExceeded, wantMsg: "not Ready: sink-b"},
		{name: "zero pods is not ready", timeout: 30 * time.Millisecond,
			wantErr: context.DeadlineExceeded, wantMsg: "no pods match"},
		{name: "terminating is not ready", pods: []runtime.Object{terminating}, timeout: 30 * time.Millisecond,
			wantErr: context.DeadlineExceeded, wantMsg: "not Ready: sink-gone"},
		{name: "list error returns at once", listErr: errBoom, timeout: 2 * time.Second, wantErr: errBoom},
		{name: "success wins over a spent context", pods: []runtime.Object{sink("sink-a", "u1", true)},
			timeout: time.Minute, cancelled: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, cs, _ := fakePods(tc.pods...)
			if tc.listErr != nil {
				cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tc.listErr
				})
			}
			ctx, cancel := context.WithTimeout(t.Context(), tc.timeout)
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			err := p.WaitReady(ctx, Namespace, sinkSel)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want it to wrap %v", err, tc.wantErr)
			}
			for _, want := range []string{"e2e", sinkSel, tc.wantMsg} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestRestart_WaitsForANewReadyUID replaces the pod the way a Deployment does: the old pod stays
// Ready (a graceful shutdown's grace window) until a replacement with a new UID exists, and the
// replacement is not Ready at first. Restart must return neither of them early.
func TestRestart_WaitsForANewReadyUID(t *testing.T) {
	p, cs, listed := fakePods(sink("sink-old", "uid-old", true))
	deleted := make(chan struct{})
	cs.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		opts := a.(k8stesting.DeleteActionImpl).DeleteOptions
		if opts.GracePeriodSeconds != nil {
			t.Errorf("Restart must delete gracefully, got grace %d", *opts.GracePeriodSeconds)
		}
		if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != "uid-old" {
			t.Errorf("Restart must delete only the pod it listed, got preconditions %v", opts.Preconditions)
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
	for want, objs := range map[string][]runtime.Object{
		"0 pods match": nil,
		"2 pods match": {sink("a", "u1", true), sink("b", "u2", true)},
	} {
		t.Run(want, func(t *testing.T) {
			p, _, _ := fakePods(objs...)
			if _, err := p.Restart(t.Context(), Namespace, sinkSel); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want one containing %q", err, want)
			}
		})
	}
}
