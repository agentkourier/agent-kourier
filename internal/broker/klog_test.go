package broker

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

// client-go asks klog.FromContext(ctx).V(8).Enabled() before it logs a response body. With an slog
// logger set, that question goes to the slog handler as level -8 and klog's -v is not consulted,
// so the cap must also be in the handler klog is given. The handler here accepts every level.
func TestClientGoCannotLogASecretBodyWhateverTheHandlerAllows(t *testing.T) {
	logs := routed(t, 100)
	klog.Background().V(7).Info("bg seven") // what is under the cap still gets through
	klog.Background().V(8).Info("bg " + secretBody)
	klog.FromContext(t.Context()).V(8).Info("ctx " + secretBody)
	klog.FromContext(t.Context()).V(9).Info("ctx " + secretBody)
	klog.Background().WithValues("k", "v").WithName("n").V(8).Info("derived " + secretBody) // a derived logger is capped too

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"SecretList","apiVersion":"v1","metadata":{},"items":[{"metadata":{"name":"s"},` +
			`"data":{"token":"Y2FuYXJ5LXRva2Vu"},"stringData":{"x":"clientgo-` + secretBody + `"}}]}`))
	}))
	defer srv.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Secrets("ns").List(t.Context(), metav1.ListOptions{}); err != nil {
		t.Fatal(err)
	}

	got := logs.String()
	if !strings.Contains(got, "bg seven") {
		t.Errorf("a level 7 message never arrived: %q", got)
	}
	for _, leak := range []string{secretBody, "Y2FuYXJ5LXRva2Vu"} {
		if strings.Contains(got, leak) {
			t.Errorf("the log holds %q, from a verbosity 8 or 9 message or from client-go's body logging: %q", leak, got)
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
