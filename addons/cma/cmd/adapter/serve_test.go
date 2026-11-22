package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/cma/cluster"
	"github.com/agentkourier/agent-kourier/addons/cma/config"
	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/cma/store/pgtest"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// fakeCMA answers every call with an empty page once it is released: until then, a start-up's listing of the sessions
// waits.
type fakeCMA struct {
	released chan struct{}
	once     sync.Once
}

func (f *fakeCMA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case <-f.released:
	case <-r.Context().Done():
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"data":[]}`)
}

func (f *fakeCMA) release() { f.once.Do(func() { close(f.released) }) }

// freeAddr is a loopback address no listener holds now.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// prober asks like the kubelet does: a connection of its own for each request, so that none is left open to hold back
// the server's shutdown.
type prober struct{ c *http.Client }

func newProber() prober {
	return prober{&http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 2 * time.Second}}
}

// status is the code a GET of url answers, or 0 when it cannot be reached.
func (p prober) status(url string) int {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	resp, err := p.c.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// waitFor polls url until it answers want, failing the test after a few seconds.
func (p prober) waitFor(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.status(url) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not answer %d within 5s", url, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// On SQLite, the one replica is alive as soon as it listens, and ready only once its start-up work (here, the listing of
// the sessions in CMA) is done: the chart's startup probe asks /readyz, so it bounds that work.
func TestSQLiteReplicaIsReadyOnlyAfterItsStartUpWork(t *testing.T) {
	readyOnlyAfterStartUpWork(t, map[string]string{"SQLITE_PATH": filepath.Join(t.TempDir(), "cma.db")})
}

// On Postgres, a replica that takes the free lease at start is the leader from its first answer, and so is ready only
// once its start-up work is done, as on SQLite.
func TestPostgresReplicaThatLeadsAtStartIsReadyOnlyAfterItsStartUpWork(t *testing.T) {
	readyOnlyAfterStartUpWork(t, map[string]string{"STORE": "postgres", "POSTGRES_DSN": pgtest.DSN(t),
		"ADVERTISE_URL": "http://127.0.0.1:1", "POD_NAME": "cma-0"})
}

// readyOnlyAfterStartUpWork runs the adapter with the settings in more, against a CMA that holds the listing of the
// sessions, and checks that it answers /healthz 200 and /readyz 503 from its first answer until the listing returns,
// and /readyz 200 after.
func readyOnlyAfterStartUpWork(t *testing.T, more map[string]string) {
	t.Helper()
	cma := &fakeCMA{released: make(chan struct{})}
	api := httptest.NewServer(cma)
	t.Cleanup(api.Close)
	t.Cleanup(cma.release)
	t.Setenv("ANTHROPIC_BASE_URL", api.URL)
	t.Setenv("ANTHROPIC_API_KEY", "fake-api-key-for-tests")

	addr := freeAddr(t)
	env := map[string]string{"CMA_AGENT_ID": "agent_test", "CMA_ENVIRONMENT_ID": "env_test", "AUTH_DISABLED": "true",
		"LISTEN_ADDR": addr}
	maps.Copy(env, more)
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, cfg, telemetry.Config{}, slog.New(slog.DiscardHandler), serveOptions{}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not return after its context ended")
		}
	})

	base, p := "http://"+addr, newProber()
	// The first answer /readyz gives, as early as the replica can give one, is 503.
	deadline := time.Now().Add(5 * time.Second)
	got := p.status(base + "/readyz")
	for got == 0 {
		if time.Now().After(deadline) {
			t.Fatal("/readyz did not answer within 5s")
		}
		got = p.status(base + "/readyz")
	}
	if got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz first answered %d while the start-up work runs, want 503", got)
	}
	if got := p.status(base + "/healthz"); got != http.StatusOK {
		t.Fatalf("/healthz = %d while the start-up work runs, want 200", got)
	}
	cma.release()
	p.waitFor(t, base+"/readyz", http.StatusOK)
}

// A replica that took the lease before it listens, and then cannot listen, hands the lease over as it stops, so a
// standby need not wait for it to expire.
func TestPostgresReplicaThatCannotListenReleasesTheLease(t *testing.T) {
	dsn := pgtest.DSN(t)
	taken, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	t.Setenv("ANTHROPIC_API_KEY", "fake-api-key-for-tests")
	env := map[string]string{"CMA_AGENT_ID": "agent_test", "CMA_ENVIRONMENT_ID": "env_test", "AUTH_DISABLED": "true",
		"LISTEN_ADDR": taken.Addr().String(), "STORE": "postgres", "POSTGRES_DSN": dsn, "ADVERTISE_URL": "http://127.0.0.1:1",
		"POD_NAME": "cma-0"}
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if err := serve(t.Context(), cfg, telemetry.Config{}, slog.New(slog.DiscardHandler), serveOptions{}); err == nil {
		t.Fatal("serve listened on an address that was taken")
	}
	st, err := store.OpenPostgres(t.Context(), dsn, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lease, err := st.CurrentLeader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if lease.Holder != "cma-0" || lease.Live {
		t.Fatalf("lease = %+v; want cma-0's, released", lease)
	}
}

// Once the adapter starts to stop, /readyz answers 503. net/http answers no request it reads after its shutdown has
// begun, so what this covers is a request read before it whose handler runs after: here a route of the test's that
// asks /readyz once the listener has closed.
func TestReadyzAnswers503OnceTheShutdownStarts(t *testing.T) {
	cma := &fakeCMA{released: make(chan struct{})}
	cma.release()
	api := httptest.NewServer(cma)
	t.Cleanup(api.Close)
	t.Setenv("ANTHROPIC_BASE_URL", api.URL)
	t.Setenv("ANTHROPIC_API_KEY", "fake-api-key-for-tests")
	addr := freeAddr(t)
	env := map[string]string{"CMA_AGENT_ID": "agent_test", "CMA_ENVIRONMENT_ID": "env_test", "AUTH_DISABLED": "true",
		"LISTEN_ADDR": addr, "SQLITE_PATH": filepath.Join(t.TempDir(), "cma.db")}
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	asked, ask := make(chan struct{}), make(chan struct{})
	answerNow := sync.OnceFunc(func() { close(ask) })
	routes := func(m *http.ServeMux) {
		m.HandleFunc("/test/readyz-later", func(w http.ResponseWriter, r *http.Request) {
			close(asked)
			<-ask
			rec := httptest.NewRecorder()
			m.ServeHTTP(rec, httptest.NewRequestWithContext(r.Context(), http.MethodGet, "/readyz", nil))
			_, _ = fmt.Fprint(w, rec.Code)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	served, stopped := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(stopped)
		served <- serve(ctx, cfg, telemetry.Config{}, slog.New(slog.DiscardHandler), serveOptions{routes: routes})
	}()
	// A failure below cannot leave serve draining for the route's handler, or running after the test.
	t.Cleanup(func() {
		cancel()
		answerNow()
		<-stopped
	})
	base, p := "http://"+addr, newProber()
	p.waitFor(t, base+"/readyz", http.StatusOK)

	answer := make(chan string, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/test/readyz-later", nil)
		if err != nil {
			answer <- err.Error()
			return
		}
		resp, err := p.c.Do(req)
		if err != nil {
			answer <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		answer <- string(body)
	}()
	select {
	case <-asked:
	case got := <-answer:
		t.Fatalf("the request ended before its handler ran: %s", got)
	}
	cancel()
	// The listener closes once the shutdown has started.
	deadline := time.Now().Add(5 * time.Second)
	for p.status(base+"/healthz") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the listener did not close within 5s of the context's end")
		}
		time.Sleep(10 * time.Millisecond)
	}
	answerNow()
	if got := <-answer; got != strconv.Itoa(http.StatusServiceUnavailable) {
		t.Fatalf("/readyz answered %s while the adapter stops, want 503", got)
	}
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// serve stops watching the token file before it returns, and so before the store closes, also when it stops on a
// failure while its context is still live.
func TestServeStopsTheTokenWatchBeforeItReturns(t *testing.T) {
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"refused by the test"}}`)
	}))
	t.Cleanup(refused.Close)
	t.Setenv("ANTHROPIC_BASE_URL", refused.URL)
	t.Setenv("ANTHROPIC_API_KEY", "fake-api-key-for-tests")
	tokens := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokens, []byte("fake-token-for-tests\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CMA_AGENT_ID": "agent_test", "CMA_ENVIRONMENT_ID": "env_test", "AUTH_TOKEN_FILE": tokens,
		"LISTEN_ADDR": freeAddr(t), "SQLITE_PATH": filepath.Join(t.TempDir(), "cma.db")}
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	// The takeover's listing of the sessions is refused, which stops serve while t's context is live.
	if err := serve(t.Context(), cfg, telemetry.Config{}, slog.New(slog.DiscardHandler), serveOptions{}); err == nil {
		t.Fatal("serve returned nil after its takeover failed")
	}
	stacks := make([]byte, 1<<20)
	stacks = stacks[:runtime.Stack(stacks, true)]
	if bytes.Contains(stacks, []byte("auth.(*Tokens).Watch")) {
		t.Fatal("serve returned while it still watched the token file")
	}
}

// slowUpkeep is a leader's work whose upkeep ends a little after its context does, as a write in flight would.
type slowUpkeep struct {
	started chan struct{}
	stopped atomic.Bool
}

func (*slowUpkeep) Restore(context.Context) error { return nil }
func (*slowUpkeep) Settle(context.Context) error  { return nil }
func (u *slowUpkeep) Run(ctx context.Context) {
	close(u.started)
	<-ctx.Done()
	time.Sleep(100 * time.Millisecond)
	u.stopped.Store(true)
}

// takeOver returns only once the upkeep has stopped, so that the release of the lease and the close of the store that
// follow do not run under an upkeep still writing.
func TestTakeOverReturnsOnceTheUpkeepHasStopped(t *testing.T) {
	work := &slowUpkeep{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- takeOver(ctx, work, &cluster.Status{}, slog.New(slog.DiscardHandler)) }()
	<-work.started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("takeOver = %v, want nil", err)
	}
	if !work.stopped.Load() {
		t.Fatal("takeOver returned while the upkeep still ran")
	}
}
