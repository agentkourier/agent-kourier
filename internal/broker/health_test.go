package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// stubs are the three things readiness reads, each flipped by the test.
type stubs struct{ loaded, synced, open atomic.Bool }

func (s *stubs) Status() config.Status {
	if s.loaded.Load() {
		return config.Status{LastSuccess: time.Unix(1, 0)}
	}
	return config.Status{}
}

func (s *stubs) Keys(context.Context, string, string) ([]string, error) {
	if s.synced.Load() {
		return nil, config.ErrSecretNotFound // a cache that has synced and lacks the name
	}
	return nil, k8ssecrets.ErrNotSynced
}

func (s *stubs) GetEvent(context.Context, string) (store.Event, error) {
	if s.open.Load() {
		return store.Event{}, store.ErrNotFound
	}
	return store.Event{}, errors.New("sql: database is closed")
}

func (s *stubs) readiness() *Readiness {
	var r Readiness
	r.Set(ConfigLoaded(s), SecretsSynced(s, []string{"sre"}), StoreOpen(s))
	return &r
}

// /healthz says the process is alive, whatever else is true. /readyz is true when, and only
// when, the first config has loaded, the Secret caches have synced and the store answers; it
// names each condition that does not hold. Slack is not one of them.
func TestReadyzFlipsExactlyWhenItsThreeConditionsHold(t *testing.T) {
	var s stubs
	h := NewHandler(mustMetrics(t, nil), s.readiness())
	if code, _ := get(t, h, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d before anything is ready; want 200", code)
	}
	for mask := range 8 {
		s.loaded.Store(mask&1 != 0)
		s.synced.Store(mask&2 != 0)
		s.open.Store(mask&4 != 0)
		code, body := get(t, h, "/readyz")
		wantCode := http.StatusServiceUnavailable
		if mask == 7 {
			wantCode = http.StatusOK
		}
		if code != wantCode {
			t.Errorf("loaded=%t synced=%t open=%t: /readyz = %d, %q; want %d", mask&1 != 0, mask&2 != 0, mask&4 != 0, code, body, wantCode)
		}
		for i, name := range []string{"config", "secrets", "store"} {
			if got := strings.Contains(body, name); got != (mask>>i&1 == 0) {
				t.Errorf("mask %03b: body %q names %s: %t; want it named exactly when it does not hold", mask, body, name, got)
			}
		}
	}
}

// The secrets check reads a real resolver: unstarted and stopped are not ready, started is.
func TestSecretsSyncedFollowsTheResolver(t *testing.T) {
	r := k8ssecrets.New(cluster(), []string{"sre"})
	check := SecretsSynced(r, []string{"sre"})
	if err := check.Fn(t.Context()); err == nil {
		t.Error("an unstarted resolver is ready; want not")
	}
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := check.Fn(t.Context()); err != nil {
		t.Errorf("a synced resolver is not ready: %v", err)
	}
	r.Stop()
	if err := check.Fn(t.Context()); err == nil {
		t.Error("a stopped resolver is ready; want not")
	}
}

// A bootstrapped Config is ready, and says so through the checks it provides.
func TestBootstrappedConfigIsReady(t *testing.T) {
	c, err := Bootstrap(t.Context(), cluster(), options(configDir(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	for _, check := range c.Checks() {
		if err := check.Fn(t.Context()); err != nil {
			t.Errorf("%s: %v", check.Name, err)
		}
	}
}

func TestMetricsEndpointServesTheRegistry(t *testing.T) {
	m := mustMetrics(t, func() bool { return true })
	m.CredentialRejected("sre/sre-alerts")
	code, body := get(t, NewHandler(m, &Readiness{}), "/metrics")
	if code != http.StatusOK || !strings.Contains(body, `agentkourier_credential_rejections_total{binding="sre/sre-alerts"} 1`) || !strings.Contains(body, "agentkourier_config_stale 1") {
		t.Errorf("/metrics = %d\n%s", code, body)
	}
}

func TestServerShutsDownGracefullyAndHasTimeouts(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Errorf("timeouts %v %v %v %v; want all set", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var s stubs
	s.loaded.Store(true)
	s.synced.Store(true)
	s.open.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, NewHandler(mustMetrics(t, nil), s.readiness())) }()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	if code := status(t, client, ln, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d; want 200", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v; want nil after a graceful shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its context ended")
	}
	if req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/healthz", nil); req != nil {
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			t.Error("the listener still answers after shutdown")
		}
	}
}

func status(t *testing.T, c *http.Client, ln net.Listener, path string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// Before anything is set, and with an empty set, /readyz is not ready, while /healthz and /metrics
// are served: the process can say it is alive during the Secret sync without saying it is ready.
func TestReadyzFailsClosedUntilChecksAreSet(t *testing.T) {
	var r Readiness
	h := NewHandler(mustMetrics(t, nil), &r)
	for _, tc := range []struct {
		name  string
		setup func()
	}{{"before Set", func() {}}, {"an empty set", func() { r.Set() }}} {
		name := tc.name
		tc.setup()
		if code, body := get(t, h, "/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "starting") {
			t.Errorf("/readyz %s = %d %q; want 503 starting", name, code, body)
		}
		if code, _ := get(t, h, "/healthz"); code != http.StatusOK {
			t.Errorf("/healthz %s = %d; want 200", name, code)
		}
	}
	var s stubs
	s.loaded.Store(true)
	s.synced.Store(true)
	s.open.Store(true)
	r.Set(ConfigLoaded(&s), SecretsSynced(&s, []string{"sre"}), StoreOpen(&s))
	if code, _ := get(t, h, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz after Set = %d; want 200", code)
	}
}

// A request that outlasts the shutdown wait is cut, not left running: Serve closes the server.
func TestServeCutsARequestThatOutlastsTheShutdownWait(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inFlight, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) }) // so a server that never cuts the request cannot hang the test
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(inFlight); <-release })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, h, 50*time.Millisecond) }()
	got := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String(), nil)
		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		got <- err
	}()
	<-inFlight
	cancel()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("serve = %v; want the shutdown's deadline", err)
	}
	select {
	case err := <-got:
		if err == nil {
			t.Error("the request in flight completed; want its connection cut")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request in flight was not cut")
	}
}

// Set keeps its own copy of the checks, so a caller that reuses its slice cannot change what /readyz runs.
func TestReadinessSetKeepsItsOwnCopy(t *testing.T) {
	var r Readiness
	checks := []Check{{Name: "a", Fn: func(context.Context) error { return nil }}}
	r.Set(checks...)
	checks[0].Fn = func(context.Context) error { return errors.New("changed after Set") }

	if code, body := get(t, NewHandler(mustMetrics(t, nil), &r), "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d %q; the check Set was given must be the one that runs", code, body)
	}
}

// A handler built with no Readiness is starting, not a panic and not ready.
func TestNilReadinessIsStarting(t *testing.T) {
	code, body := get(t, NewHandler(mustMetrics(t, nil), nil), "/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "starting") {
		t.Errorf("/readyz with no Readiness = %d %q, want 503 starting", code, body)
	}
}

// The handler serves health, readiness and metrics, and nothing else: the Alertmanager receiver is not
// on this mux (WP-47 serves it separately), so a request for a webhook path is a 404 and the receiver
// cannot be reached through the Service that exposes this port.
func TestHandlerHasNoReceiver(t *testing.T) {
	h := NewHandler(mustMetrics(t, nil), &Readiness{})
	for _, path := range []string{"/hooks/alertmanager/sre/sre-alerts", "/hooks/", "/hooks", "/", "/api/v2/alerts"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader("{}")))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d; want 404: the receiver is not served here", method, path, rec.Code)
			}
		}
	}
}

// net/http reports on the standard logger, which writes plain text to stderr unless it is given another: the
// health server's goes through slog's default handler, as a warning.
func TestHTTPServerErrorLogGoesThroughTheDefaultLogger(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var out bytes.Buffer
	slog.SetDefault(slog.New(telemetry.NewJSONHandler(&out, slog.LevelDebug)))

	newHTTPServer(http.NotFoundHandler()).ErrorLog.Printf("http: a failure")

	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil || line["level"] != "WARN" || line["msg"] != "http: a failure" {
		t.Errorf("log = %q (%v); want one JSON warning with net/http's text as the message", out.String(), err)
	}
}
