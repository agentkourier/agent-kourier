package serve

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/jackc/pgx/v5"

	"github.com/agentkourier/agent-kourier/addons/ax/cluster"
	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
)

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

// prober asks as the kubelet does: a connection of its own for each request, so that none is left open to hold back
// the server's shutdown.
var prober = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}

// probe is the status a request answers, or 0 when it cannot be reached, and its body.
func probe(method, url, token string) (int, string) {
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

// waitFor polls until cond holds, failing the test after ten seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within 10s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// metric is the value of one series that base serves at /metrics, or "" when it serves none.
func metric(base, series string) string {
	_, body := probe(http.MethodGet, base+"/metrics", "")
	for _, line := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			return v
		}
	}
	return ""
}

// replica is one agent process that Run serves at base.
type replica struct {
	base   string
	exits  chan int
	done   chan error
	cancel context.CancelFunc
}

// startReplica runs the agent with the minimal settings and a token file, on addr, with the overrides in more. Its
// Exit records the code instead of ending the test binary.
func startReplica(t *testing.T, addr string, more map[string]string) *replica {
	t.Helper()
	tokens := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokens, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"MODEL_REGION": "us-east-1", "MODEL_NAME": "fake-model", "LISTEN_ADDR": addr, "AUTH_TOKEN_FILE": tokens}
	maps.Copy(env, more)
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{base: "http://" + addr, exits: make(chan int, 4), done: make(chan error, 1), cancel: cancel}
	go func() {
		r.done <- Run(ctx, cfg, echo{}, Options{Version: "9.9.9-test", Log: slog.New(slog.DiscardHandler),
			Exit: func(code int) { r.exits <- code }})
	}()
	t.Cleanup(func() { _ = r.stop(t) })
	waitFor(t, addr+" to listen", func() bool { c, _ := probe(http.MethodGet, r.base+"/healthz", ""); return c == http.StatusOK })
	return r
}

// stop ends the replica as SIGTERM does and returns what Run returned.
func (r *replica) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		r.done <- err // a later stop returns it again
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context ended")
		return nil
	}
}

func (r *replica) ready() bool {
	c, _ := probe(http.MethodGet, r.base+"/readyz", "")
	return c == http.StatusOK
}

// client is an A2A client of the replica, with the token.
func (r *replica) client(t *testing.T) *a2aclient.Client {
	t.Helper()
	card := &a2a.AgentCard{Name: "test", Version: "1", Capabilities: a2a.AgentCapabilities{Streaming: true},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(r.base+"/a2a", a2a.TransportProtocolJSONRPC)}}
	c, err := a2aclient.NewFromCard(t.Context(), card, a2aclient.WithDefaultsDisabled(),
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: bearer{&http.Transport{DisableKeepAlives: true}}}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func turn(t *testing.T, c *a2aclient.Client, text string) *a2a.Task {
	t.Helper()
	res, err := c.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))})
	task, ok := res.(*a2a.Task)
	if err != nil || !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("turn = %+v, %v", res, err)
	}
	return task
}

func caller() context.Context {
	ctx, call := a2asrv.NewCallContext(context.Background(), nil)
	call.User = a2asrv.NewAuthenticatedUser("agent-kourier", nil)
	return ctx
}

// cutOff leaves a working task in the store, as a process that stopped mid-turn would.
func cutOff(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.Create(caller(), &a2a.Task{ID: a2a.TaskID(id), ContextID: "c-" + id, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}); err != nil {
		t.Fatal(err)
	}
}

// /metrics is open, like /healthz and /readyz; the one SQLite replica leads; a refusal of the caller check is counted.
func TestMetricsAreServed(t *testing.T) {
	r := startReplica(t, freeAddr(t), map[string]string{"SQLITE_PATH": filepath.Join(t.TempDir(), "ax.db")})
	waitFor(t, "the replica to be ready", r.ready)
	if c, _ := probe(http.MethodGet, r.base+"/a2a/.well-known/agent-card.json", ""); c != http.StatusUnauthorized {
		t.Fatalf("the card without a token = %d, want 401", c)
	}
	for series, want := range map[string]string{
		"agentkourier_ax_leader":                                   "1",
		"agentkourier_ax_leader_takeovers_total":                   "0",
		`agentkourier_ax_auth_refused_total{reason="missing"}`:     "1",
		`agentkourier_ax_forwarded_requests_total{outcome="loop"}`: "",
	} {
		if got := metric(r.base, series); got != want {
			t.Errorf("%s = %q, want %q", series, got, want)
		}
	}
}

// On SQLite, the replica fails the turns a restart cut off before it serves, and counts them.
func TestSQLiteSettlesTheCutOffTurnsBeforeServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ax.db")
	st, err := store.OpenSQLite(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cutOff(t, st, "t1")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	r := startReplica(t, freeAddr(t), map[string]string{"SQLITE_PATH": path})
	waitFor(t, "the replica to be ready", r.ready)
	got, err := r.client(t).GetTask(t.Context(), &a2a.GetTaskRequest{ID: "t1"})
	if err != nil || got.Status.State != a2a.TaskStateFailed {
		t.Fatalf("the cut-off task = %+v, %v; want failed", got, err)
	}
	for series, want := range map[string]string{"agentkourier_ax_tasks_interrupted_total": "1", `agentkourier_ax_turns_total{outcome="interrupted"}`: "1"} {
		if got := metric(r.base, series); got != want {
			t.Errorf("%s = %q, want %q", series, got, want)
		}
	}
}

// postgresEnv is a replica's settings on dsn, named name, with short lease timings.
func postgresEnv(dsn, name, addr string) map[string]string {
	return map[string]string{"STORE": "postgres", "POSTGRES_DSN": dsn, "POD_NAME": name, "ADVERTISE_URL": "http://" + addr,
		"LEASE_DURATION": "1500ms", "LEASE_RENEW_DEADLINE": "1s", "LEASE_RETRY_PERIOD": "100ms"}
}

// Two replicas on one Postgres: the first leads, settles the turn a restart cut off and serves; the second is a standby,
// ready at once, which settles nothing and forwards the A2A endpoint and the card to the leader, where the caller check
// runs. When the leader stops (SIGTERM) it releases the lease, and the standby takes over at once and serves.
func TestPostgresStandbyForwardsAndTakesOver(t *testing.T) {
	dsn := pgtest.DSN(t)
	seed, err := store.OpenPostgres(t.Context(), dsn, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	if _, ok, err := seed.TryLead(t.Context(), "seed", "http://127.0.0.1:1", time.Hour); err != nil || !ok {
		t.Fatalf("TryLead: %v, %v", ok, err)
	}
	cutOff(t, seed, "t1")
	if err := seed.Release(t.Context()); err != nil {
		t.Fatal(err)
	}

	addrA, addrB := freeAddr(t), freeAddr(t)
	a := startReplica(t, addrA, postgresEnv(dsn, "ax-a", addrA))
	waitFor(t, "a to be ready", a.ready)
	b := startReplica(t, addrB, postgresEnv(dsn, "ax-b", addrB))
	if !b.ready() {
		t.Fatal("a standby is not ready: it forwards, so it should be")
	}
	for series, want := range map[string][2]string{
		"agentkourier_ax_leader":                  {"1", "0"},
		"agentkourier_ax_leader_takeovers_total":  {"1", "0"},
		"agentkourier_ax_tasks_interrupted_total": {"1", "0"},
	} {
		if got := [2]string{metric(a.base, series), metric(b.base, series)}; got != want {
			t.Errorf("%s on a, b = %q, want %q", series, got, want)
		}
	}

	task := turn(t, b.client(t), "through the standby")
	events := 0
	for _, err := range b.client(t).SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("streamed"))}) {
		if err != nil {
			t.Fatalf("a streamed turn through the standby: %v", err)
		}
		events++
	}
	if events < 2 {
		t.Fatalf("a streamed turn through the standby gave %d events, want the leader's stream", events)
	}
	if got, err := a.client(t).GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID}); err != nil || got.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the leader's view of the forwarded turn = %+v, %v", got, err)
	}
	if got, err := b.client(t).GetTask(t.Context(), &a2a.GetTaskRequest{ID: "t1"}); err != nil || got.Status.State != a2a.TaskStateFailed {
		t.Fatalf("the cut-off task = %+v, %v; want failed, once", got, err)
	}
	if c, _ := probe(http.MethodGet, b.base+"/a2a/.well-known/agent-card.json", token); c != http.StatusOK {
		t.Fatalf("the card through the standby = %d, want 200", c)
	}
	if c, _ := probe(http.MethodGet, b.base+"/a2a/.well-known/agent-card.json", ""); c != http.StatusUnauthorized {
		t.Fatalf("the card through the standby without a token = %d, want the leader's 401", c)
	}
	if got := metric(a.base, `agentkourier_ax_auth_refused_total{reason="missing"}`); got != "1" {
		t.Errorf("the leader counted %q refusals, want 1: the caller check runs there", got)
	}
	if got := metric(b.base, `agentkourier_ax_auth_refused_total{reason="missing"}`); got != "" {
		t.Errorf("the standby counted %q refusals, want none: it does not check", got)
	}
	if got := metric(b.base, `agentkourier_ax_forwarded_requests_total{outcome="forwarded"}`); got == "" || got == "0" {
		t.Errorf("the standby forwarded %q requests", got)
	}

	if err := a.stop(t); err != nil {
		t.Fatalf("the leader's Run after SIGTERM: %v", err)
	}
	stopped := time.Now()
	waitFor(t, "b to take over", func() bool { return metric(b.base, "agentkourier_ax_leader") == "1" && b.ready() })
	if since := time.Since(stopped); since > 750*time.Millisecond { // half the lease: a take at expiry would be later
		t.Errorf("b took over %s after a stopped: a released lease is taken on the next try, not when it expires", since)
	}
	turn(t, b.client(t), "on the new leader")
	if got := metric(b.base, "agentkourier_ax_tasks_interrupted_total"); got != "0" {
		t.Errorf("the new leader failed %q tasks; a's turns had ended", got)
	}
	if len(a.exits) != 0 || len(b.exits) != 0 {
		t.Fatal("a replica exited")
	}
}

// A leader serves only once it has failed the turns a restart cut off: while its settlement waits (here, on a lock an
// outside transaction holds on the tasks), /readyz answers 503 and an A2A request 503 with Retry-After; once the
// settlement is done, the cut-off task has failed and a new turn completes.
func TestPostgresLeaderSettlesBeforeItServes(t *testing.T) {
	dsn := pgtest.DSN(t)
	seed, err := store.OpenPostgres(t.Context(), dsn, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	if _, ok, err := seed.TryLead(t.Context(), "seed", "http://127.0.0.1:1", time.Hour); err != nil || !ok {
		t.Fatalf("TryLead: %v, %v", ok, err)
	}
	cutOff(t, seed, "t1")
	if err := seed.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	rollback := pgHold(t, dsn, `LOCK TABLE a2a_tasks IN EXCLUSIVE MODE`)

	addr := freeAddr(t)
	a := startReplica(t, addr, postgresEnv(dsn, "ax-a", addr))
	waitFor(t, "the settlement to wait on the lock", func() bool { return lockWaiters(t, dsn) > 0 })
	if c, _ := probe(http.MethodGet, a.base+"/readyz", ""); c != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while the leader settles = %d, want 503", c)
	}
	if c, retry := post(t, a.base+"/a2a"); c != http.StatusServiceUnavailable || retry == "" {
		t.Fatalf("an A2A request while the leader settles = %d, Retry-After %q; want 503 with Retry-After", c, retry)
	}
	if got := metric(a.base, "agentkourier_ax_leader"); got != "1" {
		t.Fatalf("leader = %q: the replica took the lease before it listened", got)
	}

	rollback()
	waitFor(t, "a to be ready", a.ready)
	if got, err := a.client(t).GetTask(t.Context(), &a2a.GetTaskRequest{ID: "t1"}); err != nil || got.Status.State != a2a.TaskStateFailed {
		t.Fatalf("the cut-off task = %+v, %v; want failed", got, err)
	}
	turn(t, a.client(t), "after the settlement")
	if got := metric(a.base, "agentkourier_ax_tasks_interrupted_total"); got != "1" {
		t.Errorf("tasks interrupted = %q, want 1", got)
	}
}

// A leader whose write finds that another replica has taken the lease stops at once with ExitLeadershipLost, without
// waiting for its next renewal to find the same.
func TestPostgresLeaderWhoseWriteIsDeposedExits(t *testing.T) {
	dsn := pgtest.DSN(t)
	addr := freeAddr(t)
	env := postgresEnv(dsn, "ax-a", addr)
	maps.Copy(env, map[string]string{"LEASE_DURATION": "15s", "LEASE_RENEW_DEADLINE": "10s", "LEASE_RETRY_PERIOD": "5s"})
	started := time.Now()
	a := startReplica(t, addr, env)
	waitFor(t, "a to be ready", a.ready)
	depose(t, dsn)
	go func() {
		// The turn's first write is refused as deposed; what the client gets back does not matter here.
		_, _ = a.client(t).SendMessage(context.Background(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("after the takeover"))})
	}()
	select {
	case code := <-a.exits:
		if code != cluster.ExitLeadershipLost {
			t.Fatalf("exit code %d, want %d", code, cluster.ExitLeadershipLost)
		}
		if since := time.Since(started); since >= 5*time.Second {
			t.Fatalf("exited %s after the start, at or after the first renewal: the write did not stop it", since)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the leader whose write was deposed did not exit before its next renewal")
	}
}

// A leader whose lease another replica takes stops at once with ExitLeadershipLost, without a drain.
func TestPostgresLeaderThatLosesTheLeaseExits(t *testing.T) {
	dsn := pgtest.DSN(t)
	addr := freeAddr(t)
	a := startReplica(t, addr, postgresEnv(dsn, "ax-a", addr))
	waitFor(t, "a to be ready", a.ready)
	depose(t, dsn)
	select {
	case code := <-a.exits:
		if code != cluster.ExitLeadershipLost {
			t.Fatalf("exit code %d, want %d", code, cluster.ExitLeadershipLost)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the deposed leader did not exit")
	}
	select {
	case err := <-a.done:
		a.done <- err
		if !errors.Is(err, cluster.ErrLeadershipLost) {
			t.Fatalf("Run = %v, want ErrLeadershipLost", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the exit")
	}
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
	tokens := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokens, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := postgresEnv(dsn, "ax-0", taken.Addr().String())
	maps.Copy(env, map[string]string{"MODEL_REGION": "r", "MODEL_NAME": "m", "LISTEN_ADDR": taken.Addr().String(), "AUTH_TOKEN_FILE": tokens})
	cfg, err := config.Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	var exits atomic.Int32
	if err := Run(t.Context(), cfg, echo{}, Options{Log: slog.New(slog.DiscardHandler), Exit: func(int) { exits.Add(1) }}); err == nil {
		t.Fatal("Run listened on an address that was taken")
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
	if lease.Holder != "ax-0" || lease.Live || exits.Load() != 0 {
		t.Fatalf("lease = %+v, exits %d; want ax-0's, released, and no exit", lease, exits.Load())
	}
}

// depose has another store take the live lease. It names the intruder as the holder first, keeping the leader's epoch,
// so that its TryLead retakes the lease as a restarted process of the holder's pod would, however often the leader
// renews it meanwhile; the take moves the epoch on.
func depose(t *testing.T, dsn string) {
	t.Helper()
	intruder, err := store.OpenPostgres(t.Context(), dsn, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = intruder.Close() })
	if _, err := pgExec(t, dsn, `UPDATE leadership SET holder = 'intruder'`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := intruder.TryLead(t.Context(), "intruder", "http://127.0.0.1:1", time.Hour); err != nil || !ok {
		t.Fatalf("TryLead: %v, %v", ok, err)
	}
}

// post sends an empty A2A request with the token and returns its status and Retry-After.
func post(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := prober.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, resp.Header.Get("Retry-After")
}

// pgHold runs sql in a transaction of its own on dsn and keeps it open until the returned rollback runs (or the test
// ends).
func pgHold(t *testing.T, dsn, sql string) (rollback func()) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), sql); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	rollback = func() {
		once.Do(func() {
			_ = tx.Rollback(context.Background())
			_ = conn.Close(context.Background())
		})
	}
	t.Cleanup(rollback)
	return rollback
}

// lockWaiters is how many other sessions on dsn's database wait for a lock.
func lockWaiters(t *testing.T, dsn string) int {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var n int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// pgExec runs one statement on dsn, outside any store.
func pgExec(t *testing.T, dsn, sql string) (int64, error) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tag, err := conn.Exec(t.Context(), sql)
	return tag.RowsAffected(), err
}
