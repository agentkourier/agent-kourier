package main

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/cma/config"
	"github.com/agentkourier/agent-kourier/addons/cma/store"
	"github.com/agentkourier/agent-kourier/addons/cma/store/pgtest"
	"github.com/agentkourier/agent-kourier/addons/cma/telemetry"
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
	go func() { served <- serve(ctx, cfg, telemetry.Config{}, slog.New(slog.DiscardHandler)) }()
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
	if err := serve(t.Context(), cfg, telemetry.Config{}, slog.New(slog.DiscardHandler)); err == nil {
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
