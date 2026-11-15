package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/agentkourier/agent-kourier/internal/backoff"
)

// LeaderLabel is the label the leader puts on its own pod, with the value LeaderLabelValue, while it leads and runs a chat listener: the
// chart's chat Service selects the pod by it, so that a chat platform's requests reach the one replica that serves them, and a standby,
// which would answer 503, gets none.
const (
	LeaderLabel      = "agentkourier.dev/role"
	LeaderLabelValue = "leader"
)

// The pod label's timings. A write is one merge patch of the pod's labels.
const (
	// podLabelTimeout bounds one write of the label: the first, which Run waits for before the replica says it is ready, and the last, at
	// shutdown, in the health step, which is 2 s, so the health server's own stop keeps at least half of it.
	podLabelTimeout = time.Second
	// podLabelBackoffBase is the wait after a failed write, which doubles with each failure in a row up to podLabelBackoffCap. Until a write
	// succeeds the chat Service selects no leader (or, for a removal, a pod that no longer leads), and the platform's retries are what cover it.
	podLabelBackoffBase = time.Second
	podLabelBackoffCap  = 30 * time.Second
)

// The results of a write of the label, as agentkourier_leader_pod_label_writes_total counts them.
const (
	labelWriteLabeled   = "labeled"
	labelWriteUnlabeled = "unlabeled"
	labelWriteFailed    = "failed"
)

// PodRef names the pod the broker runs in: its namespace and name, which the chart passes from the downward API.
type PodRef struct {
	Namespace, Name string
}

// IsZero says the reference names no pod.
func (p PodRef) IsZero() bool { return p == PodRef{} }

// labelState is what the labeler last wrote of the label: unknown until a write succeeds.
type labelState int

const (
	labelUnknown labelState = iota
	labelOff
	labelOn
)

// podLabeler keeps the leader label on the broker's pod while the broker leads, and off otherwise. One goroutine writes it: it takes the
// label off at start (a container that exited as the leader and was restarted in the same pod kept it), puts it on once lead is called,
// and tries a failed write again after a doubling wait, for as long as it runs; a failed write never stops the broker leading. stop ends
// the goroutine and takes the label off. Leadership is never handed back while the process runs: a leader that loses the lease exits at
// once (failStop), and its pod is not ready, so out of every Service, until the container that replaces it has taken the label off.
type podLabeler struct {
	pods    corev1client.PodInterface
	name    string
	log     *slog.Logger
	onWrite func(result string)
	// base and ceiling are the doubling wait after a failure, and timeout the bound of one write; after is time.After, which a test replaces.
	base, ceiling, timeout time.Duration
	after                  func(time.Duration) <-chan time.Time

	mu      sync.Mutex
	leading bool
	stopped bool
	cancel  context.CancelFunc // ends the goroutine; nil before start
	wake    chan struct{}      // told when leading changes
	done    chan struct{}      // closed when the goroutine has ended
	first   chan struct{}      // closed after the first write, whatever its result
	state   labelState         // the goroutine's own while it runs, stop's after
}

// newPodLabeler returns the labeler of pod, which writes through client and reports each write's result to onWrite.
func newPodLabeler(client kubernetes.Interface, pod PodRef, log *slog.Logger, onWrite func(result string)) *podLabeler {
	return &podLabeler{
		pods: client.CoreV1().Pods(pod.Namespace), name: pod.Name, log: log.With("pod", pod.Namespace+"/"+pod.Name), onWrite: onWrite,
		base: podLabelBackoffBase, ceiling: podLabelBackoffCap, timeout: podLabelTimeout, after: time.After,
		wake: make(chan struct{}, 1), done: make(chan struct{}), first: make(chan struct{}),
	}
}

// start runs the goroutine that writes the label, on a context of its own that stop ends (ctx gives only its values).
func (l *podLabeler) start(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancel != nil || l.stopped {
		return
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l.cancel = cancel
	go func() { defer close(l.done); l.run(runCtx) }()
}

// lead has the label put on: the broker leads from now on.
func (l *podLabeler) lead() {
	l.mu.Lock()
	l.leading = true
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// settled is closed once the first write has been tried, whether or not it succeeded: Run waits for it before the replica says it is
// ready, so that a pod that kept the label from an earlier process does not join the chat Service as a standby.
func (l *podLabeler) settled() <-chan struct{} { return l.first }

// run writes the label until ctx ends: whenever what is wanted differs from what was last written, and again after a failure.
func (l *podLabeler) run(ctx context.Context) {
	failures := 0
	for {
		l.mu.Lock()
		want := l.leading
		l.mu.Unlock()
		var wait <-chan time.Time // nil, and never ready, while the label is as wanted
		if l.state != stateOf(want) {
			if err := l.write(ctx, want); err != nil {
				if ctx.Err() != nil {
					return
				}
				failures++
				delay := backoff.Doubling(l.base, l.ceiling, failures)
				l.log.WarnContext(ctx, "writing the leader label of the pod failed; trying again", "leader", want, "attempt", failures, "retry_in", delay, "error", err)
				wait = l.after(delay)
			} else {
				failures = 0
			}
			l.settle()
		}
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
		case <-wait:
		}
	}
}

// settle closes first, once.
func (l *podLabeler) settle() {
	select {
	case <-l.first:
	default:
		close(l.first)
	}
}

func stateOf(leading bool) labelState {
	if leading {
		return labelOn
	}
	return labelOff
}

// write puts the label on, or takes it off, with a merge patch of the pod's labels that leaves the others alone, within the timeout, and
// records the result. Taking off a label the pod does not carry succeeds.
func (l *podLabeler) write(ctx context.Context, on bool) error {
	var value any // null removes the key
	result := labelWriteUnlabeled
	if on {
		value, result = LeaderLabelValue, labelWriteLabeled
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]any{LeaderLabel: value}}})
	if err != nil {
		return fmt.Errorf("encode the label patch: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	if _, err := l.pods.Patch(ctx, l.name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "agent-kourier"}); err != nil {
		l.onWrite(labelWriteFailed)
		return fmt.Errorf("patch the pod's labels: %w", err)
	}
	l.state = stateOf(on)
	l.onWrite(result)
	if on {
		l.log.InfoContext(ctx, "labeled the pod as the leader, for the chat Service", "label", LeaderLabel+"="+LeaderLabelValue)
	}
	return nil
}

// stop ends the goroutine and, unless the pod is known to carry no label, takes it off: one write, within the timeout and ctx, logged if
// it fails. It returns when ctx ends, whatever is left. A second stop, or one after start was never called, does no more than that write.
func (l *podLabeler) stop(ctx context.Context) {
	l.mu.Lock()
	cancel, started := l.cancel, l.cancel != nil && !l.stopped
	l.stopped = true
	l.mu.Unlock()
	if started {
		cancel()
		waitFor(ctx, l.done)
		select {
		case <-l.done:
		default:
			return // the goroutine still holds the state: its write was cut, and the step's bound has passed
		}
	}
	if l.state == labelOff {
		return
	}
	if err := l.write(ctx, false); err != nil {
		l.log.Warn("taking the leader label off the pod at shutdown failed", "error", err)
	}
}
