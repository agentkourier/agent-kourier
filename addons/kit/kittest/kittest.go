// Package kittest holds an add-on to what the kit's runtime promises, end to end: the add-on runs under runtime.Run on
// a loopback port, and an A2A client drives its backend as Agent Kourier does. An add-on runs RunSQLite from a test
// that links its SQLite driver (the kit links none), and RunPostgres, which skips unless AGENTKOURIER_POSTGRES_DSN names
// a database (hack/postgres/run.sh). Each case also checks that the add-on read no setting it did not declare. Only
// tests import it.
package kittest

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// Token is the caller's token, in the token file of every replica Start runs: a fake constant.
const Token = "fake-kittest-token"

// Subject is the add-on under test and the messages that drive its backend.
type Subject[S runtime.Store] struct {
	// Addon is the add-on. Its Build makes a backend that answers the messages below with no real backend behind it (a
	// scripted model, a fake API).
	Addon runtime.Addon[S]
	// Env are the add-on's own settings, which every replica adds to the kit's.
	Env map[string]string
	// Complete is the text of a message whose turn completes; Pause, of one whose turn pauses for an answer (its task
	// input-required); Hold, of one whose turn runs until it is canceled or the add-on stops.
	Complete, Pause, Hold string
	// Answer is the text of an answer to Pause's question, which completes its task.
	Answer string
	// AnswerMetadata is the metadata of Answer's message, for a backend that reads an answer there (a structured tool
	// approval); nil sends the text alone.
	AnswerMetadata map[string]any
	// BadAnswer are the parts of an answer to Pause's question that the backend refuses (its MessageValidator).
	BadAnswer []*a2a.Part
	// BadMessage are the parts of a new message that the backend refuses before its turn (its MessageValidator); nil
	// for a backend that accepts every new message, which skips that case.
	BadMessage []*a2a.Part
}

// RunSQLite runs the cases on a SQLite file of each case's own. The test's binary must link the add-on's driver.
func RunSQLite[S runtime.Store](t *testing.T, s Subject[S]) {
	t.Helper()
	run(t, s, false, func(t *testing.T) map[string]string {
		return map[string]string{"SQLITE_PATH": filepath.Join(t.TempDir(), s.Addon.Name+".db")}
	})
}

// RunPostgres runs the cases on a Postgres schema of each case's own, with the cases of two replicas, or skips when
// AGENTKOURIER_POSTGRES_DSN is unset.
func RunPostgres[S runtime.Store](t *testing.T, s Subject[S]) {
	t.Helper()
	run(t, s, true, func(t *testing.T) map[string]string { return PostgresEnv(pgtest.DSN(t)) })
}

// PostgresEnv are the settings of a replica on the Postgres database dsn, with short lease timings. Start names the
// replica's pod and its address.
func PostgresEnv(dsn string) map[string]string {
	return map[string]string{"STORE": "postgres", "POSTGRES_DSN": dsn,
		"LEASE_DURATION": "1500ms", "LEASE_RENEW_DEADLINE": "1s", "LEASE_RETRY_PERIOD": "100ms"}
}

type kase[S runtime.Store] struct {
	name     string
	postgres bool // two replicas, or a fault only Postgres has
	run      func(t *testing.T, s Subject[S], env map[string]string)
}

func run[S runtime.Store](t *testing.T, s Subject[S], postgres bool, newEnv func(t *testing.T) map[string]string) {
	t.Helper()
	for _, c := range []kase[S]{
		{"EverySettingItReadsIsDeclared", false, everySettingItReadsIsDeclared[S]},
		{"MetricsAreServedAndTheCheckGuardsTheCard", false, metricsAreServed[S]},
		{"AResentMessageJoinsItsTask", false, aResentMessageJoinsItsTask[S]},
		{"ARefusedAnswerKeepsTheTaskPaused", false, aRefusedAnswerKeepsTheTaskPaused[S]},
		{"ARefusedMessageStartsNoTurn", false, aRefusedMessageStartsNoTurn[S]},
		{"ACancelEndsTheTurnCanceled", false, aCancelEndsTheTurnCanceled[S]},
		{"AStartFailsTheTurnsARestartCutOff", false, aStartFailsTheTurnsARestartCutOff[S]},
		{"AStopLeavesARunningTurnForTheNextStartToFail", false, aStopLeavesARunningTurnForTheNextStart[S]},
		{"AnIdleContextIsReleased", false, anIdleContextIsReleased[S]},
		{"AStandbyForwardsAndTakesOverAPausedTask", true, aStandbyForwardsAndTakesOver[S]},
		{"ALeaderSettlesBeforeItServes", true, aLeaderSettlesBeforeItServes[S]},
		{"AStandbyForwardsWithTheHopHeader", true, aStandbyForwardsWithTheHopHeader[S]},
		{"ALeaderWhoseWriteIsDeposedExits", true, aLeaderWhoseWriteIsDeposedExits[S]},
		{"ALeaderThatLosesTheLeaseExits", true, aLeaderThatLosesTheLeaseExits[S]},
		{"AReplicaThatCannotListenReleasesTheLease", true, aReplicaThatCannotListenReleasesTheLease[S]},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.postgres && !postgres {
				t.Skip("Postgres only")
			}
			c.run(t, s, newEnv(t))
		})
	}
}

// Instance is one replica of the add-on, which runtime.Run serves at Base.
type Instance struct {
	// Base is the replica's root URL, http://<its address>.
	Base string
	// Tel is its telemetry.
	Tel *telemetry.Telemetry
	// Exits gets each code the replica's Exit was called with (it does not end the test binary).
	Exits chan int
	own   settings.Values
	done  chan error
	stop  context.CancelFunc
}

// Start runs a replica named pod of the subject's add-on on a free loopback address, with env and the subject's own
// settings over a token file of Token's, and returns once it listens. On Postgres its ADVERTISE_URL is its address.
// The test fails if it read a setting it did not declare.
func Start[S runtime.Store](t *testing.T, s Subject[S], pod string, env map[string]string, opts runtime.Options) *Instance {
	t.Helper()
	addr := FreeAddr(t)
	all := map[string]string{"LISTEN_ADDR": addr, "AUTH_TOKEN_FILE": TokenFile(t), "POD_NAME": pod}
	if env["STORE"] == "postgres" {
		all["ADVERTISE_URL"] = "http://" + addr
	}
	maps.Copy(all, env)
	maps.Copy(all, s.Env)
	cfg, own, err := runtime.Parse(s.Addon, func(k string) string { return all[k] })
	if err != nil {
		t.Fatalf("the settings: %v", err)
	}
	tel, err := telemetry.New(s.Addon.TelemetrySpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	in := &Instance{Base: "http://" + addr, Tel: tel, Exits: make(chan int, 4), own: own, done: make(chan error, 1), stop: cancel}
	if opts.Log == nil {
		opts.Log = discard()
	}
	opts.Telemetry = tel
	opts.Exit = func(code int) { in.Exits <- code }
	listening := make(chan struct{})
	prev := opts.Listening
	opts.Listening = func(a net.Addr) {
		if prev != nil {
			prev(a)
		}
		close(listening)
	}
	go func() { in.done <- runtime.Run(ctx, s.Addon, cfg, own, opts) }()
	t.Cleanup(func() {
		_ = in.Stop(t)
		if undeclared := own.Undeclared(); len(undeclared) > 0 {
			t.Errorf("the add-on read settings it does not declare: %q", undeclared)
		}
	})
	select {
	case <-listening:
	case err := <-in.done:
		in.done <- err
		t.Fatalf("Run returned before it listened: %v", err)
	}
	return in
}

// Stop ends the replica as SIGTERM does and returns what Run returned.
func (in *Instance) Stop(t *testing.T) error {
	t.Helper()
	in.stop()
	return in.Wait(t, 30*time.Second)
}

// Wait waits up to d for Run to return, and returns what it returned.
func (in *Instance) Wait(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-in.done:
		in.done <- err // a later Wait returns it again
		return err
	case <-time.After(d):
		t.Fatalf("Run did not return within %s", d)
		return nil
	}
}

// Ready reports whether the replica's /readyz answers 200.
func (in *Instance) Ready() bool {
	c, _ := Probe(http.MethodGet, in.Base+"/readyz", "")
	return c == http.StatusOK
}

// Client is an A2A client of the replica, with Token.
func (in *Instance) Client(t *testing.T) *a2aclient.Client {
	t.Helper()
	card := &a2a.AgentCard{Name: "kittest", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(in.Base+runtime.A2APath, a2a.TransportProtocolJSONRPC)}}
	c, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithDefaultsDisabled(),
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: Bearer{Base: &http.Transport{DisableKeepAlives: true}}}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Metric is the value of one series of the replica's metrics, or "" when it has none. It reads them as /metrics serves
// them, from Tel, so a stopped replica can be read too.
func (in *Instance) Metric(series string) string {
	rec := httptest.NewRecorder()
	in.Tel.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			return v
		}
	}
	return ""
}

// WantMetric waits up to five seconds for the replica's series to be want: a turn is counted when its execution
// returns, which can be after the caller has read its last event.
func (in *Instance) WantMetric(t *testing.T, series, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	got := in.Metric(series)
	for got != want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		got = in.Metric(series)
	}
	if got != want {
		t.Errorf("%s = %q, want %q", series, got, want)
	}
}

// Bearer sets Token on each request.
type Bearer struct{ Base http.RoundTripper }

// RoundTrip implements http.RoundTripper.
func (b Bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+Token)
	return b.Base.RoundTrip(r)
}

// TokenFile is a token file holding Token.
func TokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte(Token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// FreeAddr is a loopback address no listener holds now.
func FreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// prober asks as the kubelet does: a connection of its own for each request, so that none is left open to hold back
// the server's shutdown.
var prober = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}

// Probe is the status a request answers, or 0 when it cannot be reached, and its body. A token, if set, is sent as
// Bearer.
func Probe(method, url, token string) (int, string) {
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		return 0, ""
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := prober.Do(req)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// WaitFor polls until cond holds, failing the test after ten seconds.
func WaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within 10s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// AsCaller is a context in an A2A call of the caller every request with Token runs as (auth.Caller).
func AsCaller() context.Context {
	ctx, call := a2asrv.NewCallContext(context.Background(), nil)
	call.User = a2asrv.NewAuthenticatedUser(auth.Caller, nil)
	return ctx
}
