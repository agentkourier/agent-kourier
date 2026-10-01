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
	seen []string // the container runs whose terminations the pod's container already showed, by ID
}

// Baseline reads the named container of the pod matching selector: its pod and the terminations it
// already shows, which are not crashes to wait for. A scenario that arms a fault by a rollout takes it after
// that rollout, from the pod the rollout left, so that the exit of the pod the rollout stopped is not in
// question; a crash of any pod that replaces this one counts, and so does one of this pod's later runs. It
// fails if no pod matches, or none has the container.
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
					b.seen = append(b.seen, terminationID(t))
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
// not the baseline's, which is a rollout's new one, every termination is. A clean exit is never a crash, and
// neither is anything a pod that is being deleted does: a rollout stops its container, which may exit 137.
// It polls, and never sleeps for a set time; ctx bounds it, and the error then says what it last saw. A
// failed list ends the wait at once.
func (p *Pods) WaitCrash(ctx context.Context, namespace, selector, container string, since Baseline) (Termination, error) {
	var got Termination
	err := p.wait(ctx, namespace, selector, func(pods []corev1.Pod) string {
		seen := fmt.Sprintf("no pod of %q has the container %q", selector, container)
		for i := range pods {
			cs := containerStatus(&pods[i], container)
			if cs == nil || pods[i].DeletionTimestamp != nil {
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
		if known := samePod && slices.Contains(since.seen, terminationID(t)); !known {
			return t
		}
	}
	return nil
}

// terminationID names the run of the container that ended, by its container ID, which a restart changes.
// Its end time cannot: Kubernetes keeps whole seconds, and a container that crashes again within the second
// of a crash that was already seen has still crashed again. A termination with no ID, which the kubelet does
// not give, is named by when it ended.
func terminationID(t *corev1.ContainerStateTerminated) string {
	if t.ContainerID != "" {
		return t.ContainerID
	}
	return "finished:" + t.FinishedAt.UTC().Format(time.RFC3339)
}

func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}
