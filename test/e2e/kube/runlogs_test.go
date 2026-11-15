package kube

import (
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// endedRun is the end of the container run named run.
func endedRun(run string) *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{ExitCode: 5, Reason: "Error", ContainerID: "containerd://" + run, FinishedAt: metav1.NewTime(time.Unix(5, 0))}
}

// restartedPod is the pod agent-kourier-a whose container has restarted since the run before ended: before is its last state
// (nil for none), and the container now runs the run now.
func restartedPod(before, now string) *corev1.Pod {
	var last *corev1.ContainerStateTerminated
	if before != "" {
		last = endedRun(before)
	}
	pod := agentKourierPodNamed("uid-a", "agent-kourier-a", 1, last, nil)
	for i := range pod.Status.ContainerStatuses {
		if cs := &pod.Status.ContainerStatuses[i]; cs.Name == "agent-kourier" {
			cs.ContainerID, cs.State = "containerd://"+now, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		}
	}
	return pod
}

// logsByPrevious makes the fake answer a log request with "previous log" or "current log", by the request's Previous.
func logsByPrevious(cs *fake.Clientset) {
	cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "log" {
			return false, nil, nil
		}
		if opts, _ := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions); opts != nil && opts.Previous {
			return true, &runtime.Unknown{Raw: []byte("previous log")}, nil
		}
		return true, &runtime.Unknown{Raw: []byte("current log")}, nil
	})
}

func TestRunLogsReadsTheLogOfTheRunWhereverTheKubeletShowsIt(t *testing.T) {
	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		want string
	}{
		"ended, not yet restarted: the current log": {agentKourierPodNamed("uid-a", "agent-kourier-a", 0, nil, endedRun("aaaa")), "current log"},
		"restarted: the previous log":               {restartedPod("aaaa", "bbbb"), "previous log"},
		"still running: the current log":            {restartedPod("", "aaaa"), "current log"},
	} {
		t.Run(name, func(t *testing.T) {
			p, cs, _ := fakePods(tc.pod)
			logsByPrevious(cs)
			got, err := p.RunLogs(soon(t), "agent-kourier", "agent-kourier-a", "agent-kourier", "aaaa", 10)
			if err != nil || got != tc.want {
				t.Errorf("RunLogs = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestRunLogsReadsAgainWhenTheContainerRestartsMeanwhile: the kubelet restarts the container between the read of its state and the
// read of its log, so the current log read is the new run's. RunLogs sees the run moved and reads the previous log, the run's.
func TestRunLogsReadsAgainWhenTheContainerRestartsMeanwhile(t *testing.T) {
	ended := agentKourierPodNamed("uid-a", "agent-kourier-a", 0, nil, endedRun("aaaa"))
	p, cs, _ := fakePods(ended)
	logsByPrevious(cs)
	var mu sync.Mutex
	gets := 0
	cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "" {
			return false, nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if gets++; gets == 1 {
			return true, ended, nil
		}
		return true, restartedPod("aaaa", "bbbb"), nil // restarted after the first look
	})
	got, err := p.RunLogs(soon(t), "agent-kourier", "agent-kourier-a", "agent-kourier", "aaaa", 10)
	if err != nil || got != "previous log" {
		t.Errorf("RunLogs = %q, %v; want the previous log: the run had moved to the last state when its log was read", got, err)
	}
}

func TestRunLogsFailsWhenTheRunsLogIsGone(t *testing.T) {
	p, cs, _ := fakePods(restartedPod("bbbb", "cccc")) // two restarts since aaaa ended: the kubelet keeps one previous log
	logsByPrevious(cs)
	if got, err := p.RunLogs(soon(t), "agent-kourier", "agent-kourier-a", "agent-kourier", "aaaa", 10); err == nil {
		t.Errorf("RunLogs = %q; want an error: neither log is run aaaa's", got)
	}
}
