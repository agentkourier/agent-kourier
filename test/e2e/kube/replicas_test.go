package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const containerA = "containerd://aaaa"

// replicaPod is an Agent Kourier pod with the given name and IP whose container "agent-kourier" runs the container run, which
// has restarted restarts times.
func replicaPod(name, ip, run string, restarts int32) *corev1.Pod {
	p := agentKourierPodNamed("uid-"+name, name, restarts, nil, nil)
	p.Status.PodIP = ip
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == "agent-kourier" {
			p.Status.ContainerStatuses[i].ContainerID = run
			p.Status.ContainerStatuses[i].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		}
	}
	return p
}

func TestReplicasListsTheRunningProcessOfEachPod(t *testing.T) {
	gone := replicaPod("agent-kourier-c", "10.0.0.3", "containerd://cccc", 0)
	now := metav1.NewTime(time.Unix(1, 0))
	gone.DeletionTimestamp = &now // a pod that is shutting down is not a replica
	other := replicaPod("other", "10.0.0.4", "containerd://dddd", 0)
	other.Labels = map[string]string{"app": "sink"}
	p, _, _ := fakePods(replicaPod("agent-kourier-a", "10.0.0.1", containerA, 0), replicaPod("agent-kourier-b", "10.0.0.2", "containerd://bbbb", 2), gone, other)

	got, err := p.Replicas(t.Context(), "agent-kourier", agentKourierSel, "agent-kourier")
	if err != nil {
		t.Fatal(err)
	}
	want := []Replica{
		{Name: "agent-kourier-a", UID: types.UID("uid-agent-kourier-a"), IP: "10.0.0.1", ContainerID: "aaaa", Restarts: 0},
		{Name: "agent-kourier-b", UID: types.UID("uid-agent-kourier-b"), IP: "10.0.0.2", ContainerID: "bbbb", Restarts: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("Replicas = %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("replica %d = %+v; want %+v", i, got[i], want[i])
		}
	}
	if got[0].ID() == got[1].ID() || got[0].ID() != "agent-kourier-a/aaaa" {
		t.Errorf("IDs %q and %q; want the pod and the container run, which a restart in the same pod changes", got[0].ID(), got[1].ID())
	}
}

func TestReplicasLeavesOutAPodWithNoRunningContainerOrNoIP(t *testing.T) {
	notStarted := replicaPod("agent-kourier-a", "", "", 0) // scheduled, not yet running
	notStarted.Status.ContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	waiting := replicaPod("agent-kourier-b", "10.0.0.2", containerA, 1) // between two runs: the restart is waiting out its back-off
	waiting.Status.ContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	p, _, _ := fakePods(notStarted, waiting)

	got, err := p.Replicas(t.Context(), "agent-kourier", agentKourierSel, "agent-kourier")
	if err != nil || len(got) != 0 {
		t.Fatalf("Replicas = %+v, %v; want none: neither pod has a process to ask", got, err)
	}
}

func TestLogsOfThePreviousRunAskForThePreviousContainer(t *testing.T) {
	p, _, _ := fakePods(replicaPod("agent-kourier-a", "10.0.0.1", containerA, 1))
	got, err := p.Logs(t.Context(), "agent-kourier", "agent-kourier-a", "agent-kourier", true, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "fake logs") {
		t.Errorf("Logs = %q; want what the API server returned", got)
	}
}

func TestWaitExitFindsTheEndOfAGivenRun(t *testing.T) {
	ended := func(run string, code int32) *corev1.ContainerStateTerminated {
		return &corev1.ContainerStateTerminated{ExitCode: code, Reason: "Error", ContainerID: "containerd://" + run, FinishedAt: metav1.NewTime(time.Unix(5, 0))}
	}
	pod := agentKourierPodNamed("uid-a", "agent-kourier-a", 1, ended("aaaa", 5), nil)
	p, _, _ := fakePods(pod)

	// The run that ended, whichever of the two places the kubelet shows it in, and another run's end is not its.
	got, err := p.WaitExit(soon(t), "agent-kourier", "agent-kourier-a", "agent-kourier", "aaaa")
	if err != nil || got.ExitCode != 5 || !got.Previous {
		t.Fatalf("WaitExit = %+v, %v; want the exit code 5 of run aaaa, from the last state: the container has been restarted", got, err)
	}
	// Before the restart the same end is the container's state, and its log is the current one's.
	waiting := agentKourierPodNamed("uid-b", "agent-kourier-b", 0, nil, ended("cccc", 5))
	pb, _, _ := fakePods(waiting)
	if got, err := pb.WaitExit(soon(t), "agent-kourier", "agent-kourier-b", "agent-kourier", "cccc"); err != nil || got.ExitCode != 5 || got.Previous {
		t.Errorf("WaitExit = %+v, %v; want the exit code 5 of run cccc, from the state: not yet restarted", got, err)
	}
	short, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if got, err := p.WaitExit(short, "agent-kourier", "agent-kourier-a", "agent-kourier", "bbbb"); err == nil {
		t.Errorf("WaitExit for a run that has not ended = %+v; want it to wait, and fail when its context ends", got)
	}
}
