package broker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"

	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
)

// startConfig is a RunConfig that would run: the example config in a temp dir, a fake cluster that
// holds its Secrets, and a free port for the health server.
func startConfig(t *testing.T) RunConfig {
	t.Helper()
	c := valid(t)
	c.ConfigDir = configDir(t)
	c.DBPath = filepath.Join(t.TempDir(), "courier.db")
	c.Client = cluster()
	c.SyncTimeout = 30 * time.Second
	return c
}

// running is a Run in progress.
type running struct {
	t      *testing.T
	cfg    RunConfig
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, c RunConfig) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &running{t: t, cfg: c, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- Run(ctx, c) }()
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

// wait returns Run's error, or fails the test if Run has not returned in 10 s.
func (r *running) wait() error {
	r.t.Helper()
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup, which waits too
		return err
	case <-time.After(10 * time.Second):
		r.t.Fatal("Run did not return")
		return nil
	}
}

// get asks the health server for path.
func (r *running) get(path string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodGet, "http://"+r.cfg.Listener.Addr().String()+path, http.NoBody)
	if err != nil {
		r.t.Fatal(err)
	}
	// A listener nobody serves on still accepts connections, so a request needs a deadline of its own.
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// until polls get(path) until it answers want, or fails after 10 s.
func (r *running) until(path string, want int) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body := r.get(path)
		if code == want {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%s = %d %q, want %d", path, code, body, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Liveness and readiness are different questions: the process is alive as soon as it serves, and
// ready only when the Secret caches have synced and the store answers.
func TestRunAnswersHealthzWhileReadyzIsStillStarting(t *testing.T) {
	gate := make(chan struct{})
	var open sync.Once
	c := startConfig(t)
	c.Client.(*fake.Clientset).PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		<-gate // the Secret caches cannot sync, so Bootstrap cannot finish
		return false, nil, nil
	})
	r := start(t, c)
	t.Cleanup(func() { open.Do(func() { close(gate) }) }) // after start's own, so it runs first: Run cannot return while Bootstrap waits

	r.until("/healthz", http.StatusOK)
	if code, body := r.get("/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "starting") {
		t.Errorf("/readyz during Bootstrap = %d %q, want 503 starting", code, body)
	}

	open.Do(func() { close(gate) })
	r.until("/readyz", http.StatusOK)
	if code, _ := r.get("/healthz"); code != http.StatusOK {
		t.Errorf("/healthz after readiness = %d", code)
	}
}

func TestRunReturnsNilWhenItsContextEndsAndLeavesNothingRunning(t *testing.T) {
	before := goleak.IgnoreCurrent()
	r := start(t, startConfig(t))
	r.until("/readyz", http.StatusOK)

	r.cancel()

	if err := r.wait(); err != nil {
		t.Errorf("Run = %v after its context ended, want nil: a clean shutdown", err)
	}
	if conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", r.cfg.Listener.Addr().String()); err == nil {
		_ = conn.Close()
		t.Error("the health server still accepts connections after Run returned")
	}
	goleak.VerifyNone(t, before)
}

// The connection check comes right after Bootstrap, before the store exists: a config that v1 cannot
// serve leaves no database behind and never becomes ready.
func TestRunRefusesTwoConnectionsBeforeItOpensTheStore(t *testing.T) {
	c := startConfig(t)
	second := `apiVersion: agentcourier.dev/v1alpha1
kind: ChatConnection
metadata: { name: slack-two, namespace: courier-system }
spec: { platform: slack, mode: socket, credentialsSecretRef: { name: slack-two } }
`
	if err := os.WriteFile(filepath.Join(c.ConfigDir, "second.yaml"), []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Client = fake.NewClientset(secretOf("courier-system", "slack-main", "botToken", "appToken"), secretOf("courier-system", "slack-two", "botToken", "appToken"),
		secretOf("sre", "courier-sre-alerts-oidc", "token"), secretOf("sre", "courier-webhook-token", "token"))
	r := start(t, c)

	if err := r.wait(); !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "slack-two") {
		t.Errorf("Run = %v, want ErrWiring naming both connections", err)
	}
	if _, err := os.Stat(c.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the database exists after a wiring error (%v): the connection check must come before the store", err)
	}
}

func TestRunPassesAnRBACDenialThroughAsAccessDenied(t *testing.T) {
	c := startConfig(t)
	c.Client.(*fake.Clientset).PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("RBAC: no list"))
	})
	r := start(t, c)

	err := r.wait()

	if !errors.Is(err, k8ssecrets.ErrAccessDenied) || !apierrors.IsForbidden(err) || errors.Is(err, ErrWiring) {
		t.Errorf("Run = %v, want ErrAccessDenied wrapping the forbidden cause, and not a wiring error", err)
	}
	if _, err := os.Stat(c.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("the database was opened although Bootstrap failed")
	}
}

func TestRunRefusesAnIncompleteConfigBeforeStartingAnything(t *testing.T) {
	c := startConfig(t)
	c.Sink = nil
	r := start(t, c)

	if err := r.wait(); !errors.Is(err, ErrWiring) {
		t.Errorf("Run = %v, want ErrWiring", err)
	}
	if _, err := os.Stat(c.DBPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("the database was opened for an incomplete config")
	}
}

// klog is routed before anything else, so client-go's first log line already goes through the
// cap and to the broker's logger.
func TestRunRoutesKlogToItsLoggerFirst(t *testing.T) {
	t.Cleanup(klog.ClearLogger)
	out := &logBuffer{}
	c := startConfig(t)
	c.Logger = slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.Level(-20)}))
	r := start(t, c)
	r.until("/readyz", http.StatusOK)

	klog.Info("klog probe line")

	if !strings.Contains(out.String(), "klog probe line") {
		t.Errorf("klog output did not reach the broker's logger: %q", out.String())
	}
}

func TestCleanupsRunInReverseOrderAndAllOfThem(t *testing.T) {
	var order []string
	var s cleanups
	for _, n := range []string{"first", "second", "third"} {
		s.add(func() { order = append(order, n) })
	}
	s.run()
	s.run() // a second run finds nothing left
	if got := strings.Join(order, ","); got != "third,second,first" {
		t.Errorf("cleanups ran as %s, want last registered first", got)
	}
}
