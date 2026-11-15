package kube

import (
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Replica is one running process of Agent Kourier as a failover scenario needs it: where to ask it (its pod's IP), which
// container run it is (the ID the signaler names it by) and how often its container has restarted. A container that
// restarts in the same pod is a new process in the same pod, so the pod's name alone does not say which process led.
type Replica struct {
	Name        string
	UID         types.UID
	IP          string
	ContainerID string // the run's ID without the runtime's scheme: "containerd://abc" is "abc"
	Restarts    int32
}

// ID names the process: its pod and its container run. A restart in the same pod changes it.
func (r Replica) ID() string { return r.Name + "/" + r.ContainerID }

// Replicas returns the pods matching selector in namespace whose named container is running, as [Replica]s, in the
// order the API lists them. A pod that is shutting down, one with no IP yet, and one whose container is not running
// (starting, or waiting out the back-off of a restart) have no process to ask and are left out.
func (p *Pods) Replicas(ctx context.Context, namespace, selector, container string) ([]Replica, error) {
	pods, err := p.List(ctx, namespace, selector)
	if err != nil {
		return nil, err
	}
	var out []Replica
	for i := range pods {
		pod := &pods[i]
		cs := containerStatus(pod, container)
		if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" || cs == nil || cs.State.Running == nil || cs.ContainerID == "" {
			continue
		}
		_, id, _ := strings.Cut(cs.ContainerID, "://")
		out = append(out, Replica{Name: pod.Name, UID: pod.UID, IP: pod.Status.PodIP, ContainerID: id, Restarts: cs.RestartCount})
	}
	return out, nil
}

// Logs returns the last tail lines of the named container's log in the pod: of the run before the current one if
// previous is set, which is how a container that exited is read after the kubelet has restarted it. The runner's Role
// needs get on pods/log in Agent Kourier's namespace (hack/kind/e2e/runner.yaml). Agent Kourier logs no token and no Secret value,
// and a caller that prints a line of it prints only the ones it looks for.
func (p *Pods) Logs(ctx context.Context, namespace, pod, container string, previous bool, tail int64) (string, error) {
	stream, err := p.client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: previous, TailLines: &tail}).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("logs of %s in %s/%s: %w", container, namespace, pod, err)
	}
	defer func() { _ = stream.Close() }()
	b, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
	if err != nil {
		return "", fmt.Errorf("read the logs of %s in %s/%s: %w", container, namespace, pod, err)
	}
	return string(b), nil
}

// maxLogBytes is the most of a log Logs reads.
const maxLogBytes = 1 << 20

// runLogTries is how many times RunLogs reads a log that the kubelet moved between the read of the container's state and the read
// of the log, before it gives up: each move is a restart, so a second one means the run's log is gone already.
const runLogTries = 3

// RunLogs returns the last tail lines of the log of the run of the named container whose ID is run (as [Replica.ContainerID]), as
// [Pods.Logs] does, wherever the kubelet shows the run when the log is read: the current log while the run is the container's
// state, running or ended, and the previous one once the kubelet has restarted the container. A Termination's Previous, from
// [Pods.WaitExit], says where the run was when it was seen to end, and a restart after that would make it name the new run's log.
// RunLogs reads the container's state before the log and again after, and reads once more if the run moved in between. It is an
// error when the container shows the run in neither place: it has restarted twice since, and the kubelet keeps one previous log.
func (p *Pods) RunLogs(ctx context.Context, namespace, pod, container, run string, tail int64) (string, error) {
	previous, err := p.runLog(ctx, namespace, pod, container, run)
	if err != nil {
		return "", err
	}
	for range runLogTries {
		logs, err := p.Logs(ctx, namespace, pod, container, previous, tail)
		if err != nil {
			return "", err
		}
		after, err := p.runLog(ctx, namespace, pod, container, run)
		if err != nil {
			return "", err
		}
		if after == previous {
			return logs, nil
		}
		previous = after
	}
	return "", fmt.Errorf("the log of run %s of %s in %s/%s: the container kept restarting while it was read", run, container, namespace, pod)
}

// runLog says which log of the container holds the run's, as [RunLogs] reads it: the previous one when the run is the container's
// last state, and the current one when it is its state.
func (p *Pods) runLog(ctx context.Context, namespace, pod, container, run string) (previous bool, err error) {
	got, err := p.client.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("read pod %s/%s: %w", namespace, pod, err)
	}
	cs := containerStatus(got, container)
	if cs == nil {
		return false, fmt.Errorf("pod %s/%s has no container %q", namespace, pod, container)
	}
	isRun := func(id string) bool { return strings.HasSuffix(id, "://"+run) }
	switch {
	case cs.State.Terminated != nil && isRun(cs.State.Terminated.ContainerID), cs.State.Running != nil && isRun(cs.ContainerID):
		return false, nil
	case cs.LastTerminationState.Terminated != nil && isRun(cs.LastTerminationState.Terminated.ContainerID):
		return true, nil
	}
	return false, fmt.Errorf("pod %s/%s shows run %s of %s in neither its state nor its last state: its log is gone", namespace, pod, run, container)
}

// WaitExit blocks until the run of the named container in the named pod whose ID is run (as [Replica.ContainerID]) has
// ended, and returns how: the kubelet shows it as the container's state while it waits to restart it, and as its last
// state after (Termination.Previous says which, for its log). Another run's end is not an answer. It polls, and ctx bounds it; the error then says what it last saw.
func (p *Pods) WaitExit(ctx context.Context, namespace, pod, container, run string) (Termination, error) {
	var got Termination
	err := p.wait(ctx, namespace, "", func(pods []corev1.Pod) string {
		seen := "no pod " + pod
		for i := range pods {
			if pods[i].Name != pod {
				continue
			}
			cs := containerStatus(&pods[i], container)
			if cs == nil {
				return fmt.Sprintf("pod %s has no container %q", pod, container)
			}
			for i, t := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
				if t != nil && strings.HasSuffix(t.ContainerID, "://"+run) {
					got = Termination{ExitCode: t.ExitCode, Reason: t.Reason, FinishedAt: t.FinishedAt.Time, Restarts: cs.RestartCount, Previous: i == 1}
					return ""
				}
			}
			seen = fmt.Sprintf("run %s of %s has not ended (restart count %d)", run, container, cs.RestartCount)
		}
		return seen
	})
	if err != nil {
		return Termination{}, fmt.Errorf("wait for run %s of %s in %s/%s to end: %w", run, container, namespace, pod, err)
	}
	return got, nil
}
