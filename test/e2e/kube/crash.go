package kube

import (
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Termination is how a container last ended.
type Termination struct {
	ExitCode   int32
	Reason     string
	FinishedAt time.Time
	// Restarts is the container's restart count when it was read.
	Restarts int32
}

// Baseline is what a container's terminations looked like when a scenario began to watch it, so that
// [Pods.WaitCrash] answers only for one after it. It names the pod by its UID, because arming a fault is a
// rollout: the pod that crashes is a new one, whose restart count starts again at 0, and a count from the
// pod before it would never be passed.
type Baseline struct {
	pod  types.UID
	seen []time.Time // when the terminations the pod's container already showed finished
}

// Baseline reads the named container of the pod matching selector: its pod and the terminations it
// already shows, which are not crashes to wait for. Take it before the action that causes the crash, or
// after the rollout that arms it; either way, a crash of the pod that replaces this one counts. It fails if
// no pod matches, or none has the container.
func (p *Pods) Baseline(ctx context.Context, namespace, selector, container string) (Baseline, error) {
	pods, err := p.List(ctx, namespace, selector)
	if err != nil {
		return Baseline{}, err
	}
	for i := range pods {
		if cs := containerStatus(&pods[i], container); cs != nil {
			b := Baseline{pod: pods[i].UID}
			for _, t := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
				if t != nil {
					b.seen = append(b.seen, t.FinishedAt.Time)
				}
			}
			return b, nil
		}
	}
	return Baseline{}, fmt.Errorf("baseline of %s in %s: no pod of %q has the container %q", container, namespace, selector, container)
}

// WaitCrash blocks until the named container of a pod matching selector has crashed since the baseline,
// and returns how: a non-zero exit, in the container's last termination or still as its state, before the
// kubelet restarts it. A termination that the baseline already showed is not an answer, and in a pod that is
// not the baseline's, which is a rollout's new one, every termination is. A clean exit is never a crash.
// It polls, and never sleeps for a set time; ctx bounds it, and the error then says what it last saw. A
// failed list ends the wait at once.
func (p *Pods) WaitCrash(ctx context.Context, namespace, selector, container string, since Baseline) (Termination, error) {
	var got Termination
	err := p.wait(ctx, namespace, selector, func(pods []corev1.Pod) string {
		seen := fmt.Sprintf("no pod of %q has the container %q", selector, container)
		for i := range pods {
			cs := containerStatus(&pods[i], container)
			if cs == nil {
				continue
			}
			if t := crashed(cs, since, pods[i].UID == since.pod); t != nil {
				got = Termination{ExitCode: t.ExitCode, Reason: t.Reason, FinishedAt: t.FinishedAt.Time, Restarts: cs.RestartCount}
				return ""
			}
			seen = fmt.Sprintf("%s has not crashed since the baseline (restart count %d)", container, cs.RestartCount)
		}
		return seen
	})
	if err != nil {
		return Termination{}, fmt.Errorf("wait for %s in %s to crash: %w", container, namespace, err)
	}
	return got, nil
}

// crashed returns the non-zero termination of cs that the baseline did not show, or nil. samePod says
// cs is the baseline's pod, the only one whose earlier terminations are known.
func crashed(cs *corev1.ContainerStatus, since Baseline, samePod bool) *corev1.ContainerStateTerminated {
	for _, t := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
		if t == nil || t.ExitCode == 0 {
			continue
		}
		if known := samePod && slices.ContainsFunc(since.seen, t.FinishedAt.Time.Equal); !known {
			return t
		}
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
