package broker

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"k8s.io/klog/v2"
)

// secretBody stands for a Secret list that client-go would log at verbosity 8 and above.
const secretBody = "canary-secret-list-body"

// logBuffer is a log destination that goroutines and the test share.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// routed sends klog to a logger that keeps every level, so only klog's own verbosity decides what
// arrives, and restores klog at cleanup.
func routed(t *testing.T, verbosity int) *logBuffer {
	t.Helper()
	logs := &logBuffer{}
	if err := RouteKlog(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.Level(-20)})), verbosity); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		klog.ClearLogger()
		var v klog.Level
		_ = v.Set("0")
	})
	return logs
}

func TestKlogGoesToSlogAndStaysBelowVerbosityEight(t *testing.T) {
	logs := routed(t, 10) // asking for more than the cap
	klog.Info("hello from klog")
	klog.V(7).Info("level seven")
	klog.V(8).Infof("response body: %s", secretBody)
	klog.V(10).Infof("request body: %s", secretBody)

	got := logs.String()
	if !strings.Contains(got, "hello from klog") || !strings.Contains(got, "level seven") {
		t.Errorf("klog output %q should reach slog up to level 7", got)
	}
	if strings.Contains(got, secretBody) {
		t.Errorf("a verbosity 8 or 10 message reached the log: %q", got)
	}
}

// client-go logs bodies at verbosity 8 and above, so whatever verbosity is asked for, nothing at
// those levels may be emitted.
func TestNoSecretBodyReachesTheLogAtAnyRequestedVerbosity(t *testing.T) {
	for _, asked := range []int{0, 5, 7, 8, 9, 10, 100, 1 << 20} {
		logs := routed(t, asked)
		for level := 8; level <= 12; level++ {
			klog.V(klog.Level(level)).Infof("body: %s", secretBody)
		}
		if got := logs.String(); strings.Contains(got, secretBody) {
			t.Errorf("verbosity %d asked: a Secret body reached the log: %q", asked, got)
		}
	}
}

func TestKlogVerbosityBelowTheCapIsKept(t *testing.T) {
	logs := routed(t, 2)
	klog.V(2).Info("level two")
	klog.V(3).Info("level three")
	if got := logs.String(); !strings.Contains(got, "level two") || strings.Contains(got, "level three") {
		t.Errorf("with verbosity 2 the log is %q, want level two only", got)
	}
}

func TestKlogNegativeVerbosityIsZero(t *testing.T) {
	logs := routed(t, -5)
	klog.V(1).Info("level one")
	if got := logs.String(); strings.Contains(got, "level one") {
		t.Errorf("verbosity -5 let level one through: %q", got)
	}
}
