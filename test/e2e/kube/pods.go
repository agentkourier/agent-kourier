package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// Pods controls pods through the Kubernetes API. Build one with [NewInCluster].
type Pods struct {
	client kubernetes.Interface
	poll   time.Duration // how often the wait helpers re-list
}

// NewPods wraps a client with no in-cluster guard, for tests with a fake; the runner uses [NewInCluster].
func NewPods(client kubernetes.Interface) *Pods {
	return &Pods{client: client, poll: 500 * time.Millisecond}
}

// List returns the pods in namespace matching the label selector.
func (p *Pods) List(ctx context.Context, namespace, selector string) ([]corev1.Pod, error) {
	list, err := p.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("kube: list pods %s in %s: %w", selector, namespace, err)
	}
	return list.Items, nil
}

// Kill deletes a pod with a zero grace period, which is a crash: no SIGTERM handling, no drain.
func (p *Pods) Kill(ctx context.Context, namespace, name string) error {
	grace := int64(0)
	err := p.client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
	if err != nil {
		return fmt.Errorf("kube: kill pod %s/%s: %w", namespace, name, err)
	}
	return nil
}

// Restart deletes the one pod matching selector gracefully, then waits for a pod with a different
// UID to be Ready and returns it. The old pod never satisfies the wait, even Ready and still
// shutting down. It fails if the selector matches zero or several pods.
func (p *Pods) Restart(ctx context.Context, namespace, selector string) (*corev1.Pod, error) {
	pods, err := p.List(ctx, namespace, selector)
	if err != nil {
		return nil, err
	}
	if len(pods) != 1 {
		return nil, fmt.Errorf("kube: restart %s in %s: %d pods match, want exactly 1", selector, namespace, len(pods))
	}
	old := pods[0]
	err = p.client.CoreV1().Pods(namespace).Delete(ctx, old.Name, metav1.DeleteOptions{})
	if err != nil {
		return nil, fmt.Errorf("kube: restart pod %s/%s: %w", namespace, old.Name, err)
	}
	var replacement *corev1.Pod
	err = p.wait(ctx, namespace, selector, func(pods []corev1.Pod) string {
		for i := range pods {
			if pods[i].UID != old.UID && ready(&pods[i]) {
				replacement = &pods[i]
				return ""
			}
		}
		return fmt.Sprintf("no Ready replacement for %s (UID %s)", old.Name, old.UID)
	})
	if err != nil {
		return nil, fmt.Errorf("kube: restart %s in %s: %w", selector, namespace, err)
	}
	return replacement, nil
}

// WaitReady blocks until at least one pod matches selector and every matching pod is Ready (a terminating
// pod is not), or
// until ctx ends. The error then says what it last saw.
func (p *Pods) WaitReady(ctx context.Context, namespace, selector string) error {
	err := p.wait(ctx, namespace, selector, func(pods []corev1.Pod) string {
		var notReady []string
		for i := range pods {
			if !ready(&pods[i]) {
				notReady = append(notReady, pods[i].Name)
			}
		}
		switch {
		case len(pods) == 0:
			return "no pods match"
		case len(notReady) > 0:
			return "not Ready: " + strings.Join(notReady, ", ")
		}
		return ""
	})
	if err != nil {
		return fmt.Errorf("kube: wait ready %s in %s: %w", selector, namespace, err)
	}
	return nil
}

// wait re-lists pods every poll interval until check returns "". Otherwise check's string is
// the reason, reported if ctx ends first. A failed list ends the wait at once: an RBAC error
// would otherwise look like a slow pod.
func (p *Pods) wait(ctx context.Context, namespace, selector string, check func([]corev1.Pod) string) error {
	reason := "not yet listed"
	err := wait.PollUntilContextCancel(ctx, p.poll, true, func(ctx context.Context) (bool, error) {
		pods, err := p.List(ctx, namespace, selector)
		if err != nil {
			return false, err
		}
		reason = check(pods)
		return reason == "", nil
	})
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", reason, err)
	}
	return err
}

// ready reports whether the pod is not terminating and has condition Ready=True.
func ready(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue && pod.DeletionTimestamp == nil
		}
	}
	return false
}
