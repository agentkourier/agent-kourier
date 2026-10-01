package broker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// Check is one readiness condition: Fn returns nil when it holds, and says why when it does not.
type Check struct {
	Name string
	Fn   func(context.Context) error
}

// ConfigLoaded holds once the first config has loaded: s is the Reloader.
func ConfigLoaded(s interface{ Status() config.Status }) Check {
	return Check{"config", func(context.Context) error {
		if s.Status().LastSuccess.IsZero() {
			return errors.New("the first config has not loaded")
		}
		return nil
	}}
}

// SecretsSynced holds while the resolver's Secret caches for namespaces are synced and it has not
// been stopped. It asks for a Secret that does not exist: a synced cache answers not found, and
// any other answer says why it cannot.
func SecretsSynced(r interface {
	Keys(ctx context.Context, namespace, name string) ([]string, error)
}, namespaces []string) Check {
	return Check{"secrets", func(ctx context.Context) error {
		for _, ns := range namespaces {
			_, err := r.Keys(ctx, ns, "readyz-probe")
			if errors.Is(err, k8ssecrets.ErrNotSynced) || errors.Is(err, k8ssecrets.ErrStopped) || errors.Is(err, k8ssecrets.ErrNamespaceNotWatched) {
				return err
			}
		}
		return nil
	}}
}

// StoreOpen holds while the store answers a read: s is the store, and an event nobody wrote
// is not found.
func StoreOpen(s interface {
	GetEvent(ctx context.Context, id string) (store.Event, error)
}) Check {
	return Check{"store", func(ctx context.Context) error {
		if _, err := s.GetEvent(ctx, "readyz-probe"); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	}}
}

// Readiness holds the checks /readyz runs. Until Set is called it is not ready ("starting"), so the
// endpoints can be served while the Secret caches sync, and an empty set is never ready either.
type Readiness struct{ checks atomic.Pointer[[]Check] }

// Set replaces the checks. It is safe to call while /readyz is served.
func (r *Readiness) Set(checks ...Check) { r.checks.Store(&checks) }

// NewHandler serves /healthz (the process is alive), /readyz (every check of r holds, else 503
// naming those that do not; Slack is not one of them) and /metrics.
func NewHandler(m *Metrics, r *Readiness) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, req *http.Request) {
		checks := r.checks.Load()
		if checks == nil || len(*checks) == 0 {
			http.Error(w, "not ready: starting", http.StatusServiceUnavailable)
			return
		}
		var failing []string
		for _, c := range *checks {
			if err := c.Fn(req.Context()); err != nil {
				failing = append(failing, c.Name+": "+err.Error())
			}
		}
		if len(failing) > 0 {
			http.Error(w, "not ready: "+strings.Join(failing, "; "), http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /metrics", m.Handler())
	return mux
}

// shutdownTimeout bounds the wait for requests in flight when Serve stops.
const shutdownTimeout = 10 * time.Second

func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
}

// Serve serves h on ln until ctx ends, then shuts down gracefully. It returns nil after a
// shutdown that finished, and the error of a listener that failed before it.
func Serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	return serve(ctx, ln, h, shutdownTimeout)
}

// serve is Serve with the time it waits for requests in flight, which a test shortens.
func serve(ctx context.Context, ln net.Listener, h http.Handler, wait time.Duration) error {
	srv := newHTTPServer(h)
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
	defer cancel()
	err := srv.Shutdown(sctx)
	if err != nil {
		_ = srv.Close() // the requests in flight did not finish in time: cut them
	}
	<-failed
	return err
}
