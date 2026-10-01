package kube

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Termination is how a container last ended.
type Termination struct {
	ExitCode   int32
	Reason     string
	FinishedAt time.Time
	// Restarts is the container's restart count when it was read.
	Restarts int32
}

// Restarts returns how many times the named container of the pod matching selector has been restarted:
// the baseline a scenario takes before it arms a fault, and gives [Pods.WaitCrash]. It fails if no pod
// matches, or none has the container.
func (p *Pods) Restarts(ctx context.Context, namespace, selector, container string) (int32, error) {
	pods, err := p.List(ctx, namespace, selector)
	if err != nil {
		return 0, err
	}
	for i := range pods {
		if cs := containerStatus(&pods[i], container); cs != nil {
			return cs.RestartCount, nil
		}
	}
	return 0, fmt.Errorf("restarts of %s in %s: no pod of %q has the container %q", container, namespace, selector, container)
}

// WaitCrash blocks until the named container of a pod matching selector has crashed since the
// baseline (its restart count at [Pods.Restarts]), and returns how: a non-zero exit, either in the
// container's last termination, once it has been restarted past the baseline, or still as its state,
// before the kubelet restarts it. A termination that was there at the baseline is not an answer. It
// polls, and never sleeps for a set time; ctx bounds it, and the error then says what it last saw.
// A failed list ends the wait at once.
func (p *Pods) WaitCrash(ctx context.Context, namespace, selector, container string, baseline int32) (Termination, error) {
	var got Termination
	err := p.wait(ctx, namespace, selector, func(pods []corev1.Pod) string {
		seen := fmt.Sprintf("no pod of %q has the container %q", selector, container)
		for i := range pods {
			cs := containerStatus(&pods[i], container)
			if cs == nil {
				continue
			}
			switch t := crashed(cs, baseline); {
			case t != nil:
				got = Termination{ExitCode: t.ExitCode, Reason: t.Reason, FinishedAt: t.FinishedAt.Time, Restarts: cs.RestartCount}
				return ""
			default:
				seen = fmt.Sprintf("%s has not crashed since restart %d (at restart %d)", container, baseline, cs.RestartCount)
			}
		}
		return seen
	})
	if err != nil {
		return Termination{}, fmt.Errorf("wait for %s in %s to crash: %w", container, namespace, err)
	}
	return got, nil
}

// crashed returns the non-zero termination of cs that is after baseline, or nil.
func crashed(cs *corev1.ContainerStatus, baseline int32) *corev1.ContainerStateTerminated {
	if t := cs.State.Terminated; t != nil && t.ExitCode != 0 && cs.RestartCount >= baseline {
		return t
	}
	if t := cs.LastTerminationState.Terminated; t != nil && t.ExitCode != 0 && cs.RestartCount > baseline {
		return t
	}
	return nil
}

func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}
