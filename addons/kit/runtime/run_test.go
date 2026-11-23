package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
	"github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

const fakeToken = "fake-runtime-token"

// running is one Run of an add-on, at base.
type running struct {
	base   string
	tel    *telemetry.Telemetry
	done   chan error
	cancel context.CancelFunc
}

// tokenFile is a token file with fakeToken.
func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte(fakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// startRun runs a on a free loopback port with the token file and the settings in env over SQLite's, and returns once it
// listens.
func startRun[S Store](t *testing.T, a Addon[S], env map[string]string, opts Options) *running {
	t.Helper()
	all := map[string]string{"LISTEN_ADDR": "127.0.0.1:0", "PUBLIC_URL": "http://kit.test/a2a", "AUTH_TOKEN_FILE": tokenFile(t),
		"SQLITE_PATH": "/unused/by/the/fake.db"}
	maps.Copy(all, env)
	cfg, own, err := Parse(a, func(k string) string { return all[k] })
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := &running{tel: newTel(t), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	listening := make(chan net.Addr, 1)
	opts.Listening = func(addr net.Addr) { listening <- addr }
	opts.Telemetry = r.tel
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	go func() { r.done <- Run(ctx, a, cfg, own, opts) }()
	t.Cleanup(func() { cancel(); _ = <-r.wait(t) })
	select {
	case addr := <-listening:
		r.base = "http://" + addr.String()
	case err := <-r.done:
		t.Fatalf("Run = %v before it listened", err)
	}
	return r
}

// wait returns a channel that gets what Run returned, failing the test if it takes over 30 s.
func (r *running) wait(t *testing.T) <-chan error {
	t.Helper()
	out := make(chan error, 1)
	select {
	case err := <-r.done:
		r.done <- err
		out <- err
	case <-time.After(30 * time.Second):
		t.Error("Run did not return")
		out <- nil
	}
	return out
}

// stop ends Run as SIGTERM does and returns what it returned.
func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	return <-r.wait(t)
}

var probeClient = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}

// status is what a GET of url answers, or 0 when it cannot be reached.
func status(url string) int {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within 10s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunRefusesAnAddonItCannotRun(t *testing.T) {
	good := testAddon(newFakeStore(), &scripted{})
	for name, change := range map[string]func(*Addon[*fakeStore]){
		"no name":                func(a *Addon[*fakeStore]) { a.Name = "" },
		"no noun":                func(a *Addon[*fakeStore]) { a.Noun = "" },
		"no open":                func(a *Addon[*fakeStore]) { a.Open = nil },
		"no build":               func(a *Addon[*fakeStore]) { a.Build = nil },
		"a telemetry of another": func(a *Addon[*fakeStore]) { a.Telemetry.Name = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			a := good
			change(&a)
			if err := Run(t.Context(), a, settings.Config{}, settings.Values{}, Options{}); err == nil {
				t.Fatal("Run accepted the add-on")
			}
			var stderr bytes.Buffer
			if code := Command(a, nil, io.Discard, &stderr, func(string) string { return "" }); code != ExitFailure {
				t.Fatalf("Command = %d, want %d", code, ExitFailure)
			}
			if !strings.Contains(stderr.String(), `"msg":"add-on"`) {
				t.Fatalf("Command's log = %s", stderr.String())
			}
		})
	}
}

// A backend that reads a setting its add-on did not declare stops Run before it serves.
func TestRunRefusesABackendThatReadsAnUndeclaredSetting(t *testing.T) {
	undeclared := settings.Func("KIT_UNDECLARED", "x", func(v string, _ settings.Values) (string, error) { return v, nil })
	a := testAddon(newFakeStore(), &scripted{})
	a.Build = func(_ context.Context, env Env[*fakeStore]) (Backend, error) {
		_ = undeclared.From(env.Settings)
		return &scripted{}, nil
	}
	env := map[string]string{"LISTEN_ADDR": "127.0.0.1:0", "AUTH_TOKEN_FILE": tokenFile(t), "SQLITE_PATH": "/unused.db"}
	cfg, own, err := Parse(a, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), a, cfg, own, Options{Log: slog.New(slog.DiscardHandler)})
	if err == nil || !strings.Contains(err.Error(), "the agent read settings it does not declare: KIT_UNDECLARED") {
		t.Fatalf("Run = %v", err)
	}
}

func TestInterruptedMessage(t *testing.T) {
	for noun, want := range map[string]string{
		"agent":   "Interrupted by an agent restart.",
		"adapter": "Interrupted by an adapter restart.",
		"bridge":  "Interrupted by a bridge restart.",
		"":        "Interrupted by a  restart.",
	} {
		if got := InterruptedMessage(noun); got != want {
			t.Errorf("InterruptedMessage(%q) = %q, want %q", noun, got, want)
		}
	}
}

// /readyz answers 503 once the stop has started, while requests drain.
func TestReadyzAnswers503OnceTheStopStarts(t *testing.T) {
	asked, answer := make(chan struct{}), make(chan struct{})
	answerNow := sync.OnceFunc(func() { close(answer) })
	t.Cleanup(answerNow)
	routes := func(m *http.ServeMux) {
		m.HandleFunc("/test/readyz-later", func(w http.ResponseWriter, r *http.Request) {
			close(asked)
			<-answer
			rec := httptest.NewRecorder()
			m.ServeHTTP(rec, httptest.NewRequestWithContext(r.Context(), http.MethodGet, "/readyz", nil))
			_, _ = fmt.Fprint(w, rec.Code)
		})
	}
	r := startRun(t, testAddon(newFakeStore(), &scripted{}), nil, Options{Routes: routes})
	waitUntil(t, "ready", func() bool { return status(r.base+"/readyz") == http.StatusOK })
	got := make(chan string, 1)
	go func() {
		resp, err := probeClient.Get(r.base + "/test/readyz-later") //nolint:noctx // the test's own probe
		if err != nil {
			got <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		got <- string(body)
	}()
	<-asked
	r.cancel()
	waitUntil(t, "the listener to close", func() bool { return status(r.base+"/healthz") == 0 })
	answerNow()
	if code := <-got; code != "503" {
		t.Fatalf("/readyz while the add-on stops = %s, want 503", code)
	}
	if err := <-r.wait(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
}

// Run stops watching the token file before it returns, and so before the store closes, also when it stops on a
// failure while its context is live.
func TestRunStopsTheTokenWatchBeforeItReturns(t *testing.T) {
	st := newFakeStore()
	st.settle = func(context.Context) error { return errors.New("the settlement fails") }
	a := testAddon(st, &scripted{})
	env := map[string]string{"LISTEN_ADDR": "127.0.0.1:0", "AUTH_TOKEN_FILE": tokenFile(t), "SQLITE_PATH": "/unused.db"}
	cfg, own, err := Parse(a, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), a, cfg, own, Options{Log: slog.New(slog.DiscardHandler)}); err == nil {
		t.Fatal("Run returned nil after its takeover failed")
	}
	stacks := make([]byte, 1<<20)
	stacks = stacks[:goruntime.Stack(stacks, true)]
	if bytes.Contains(stacks, []byte("auth.(*Tokens).Watch")) {
		t.Fatal("Run returned while it still watched the token file")
	}
	if got := st.events(); !slices.Equal(got, []string{"close"}) {
		t.Fatalf("the store's events = %q", got)
	}
}

// slowRelease is a backend whose release of an idle context ends a little after the upkeep's context does, as a call
// in flight would.
type slowRelease struct {
	*scripted
	st      *fakeStore
	started chan struct{}
	once    sync.Once
}

func (s *slowRelease) ReleaseContext(ctx context.Context, _ store.Context) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	time.Sleep(100 * time.Millisecond)
	s.st.note("upkeep stopped")
	return ctx.Err()
}

// The upkeep has stopped before the store closes.
func TestTheUpkeepStopsBeforeTheStoreCloses(t *testing.T) {
	st := newFakeStore()
	st.idle = []store.Context{{Owner: auth.Caller, ContextID: "c1", Ref: "session-1"}}
	b := &slowRelease{scripted: &scripted{}, st: st, started: make(chan struct{})}
	r := startRun(t, testAddon(st, b), nil, Options{UpkeepInterval: 10 * time.Millisecond})
	<-b.started
	if err := r.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := st.events(); !slices.Equal(got, []string{"upkeep stopped", "close"}) {
		t.Fatalf("the store's events = %q, want the upkeep stopped before the close", got)
	}
}

// noting is scripted, noting each Cleanup in the store's events.
type noting struct {
	*scripted
	st *fakeStore
}

func (n noting) Cleanup(ctx context.Context, ec *a2asrv.ExecutorContext, result a2a.SendMessageResult, err error) {
	n.scripted.Cleanup(ctx, ec, result, err)
	n.st.note("cleanup " + string(ec.TaskID))
}

// The stop cancels the turns still running once the requests have drained, and waits for them before the store
// closes. Their tasks stay as they were, for the next leader to fail as interrupted, and they are not counted: that
// leader counts them.
func TestAStopCutsTheRunningTurnsShortBeforeTheStoreCloses(t *testing.T) {
	st := newFakeStore()
	st.detached = true // a2a-go's write of the failed turn would land: the guard refuses it
	b := noting{scripted: &scripted{started: make(chan string, 1)}, st: st}
	r := startRun(t, testAddon(st, b), nil, Options{})
	c := clientOf(t, r.base, fakeToken)
	ctx, hangUp := context.WithCancel(t.Context())
	ids := make(chan a2a.TaskID, 1)
	go func() {
		for ev, err := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message("m1", "hold", "c1", "")}) {
			if err == nil && taskOf(ev) != "" {
				select {
				case ids <- taskOf(ev):
				default:
				}
			}
		}
	}()
	id := <-ids
	<-b.started
	hangUp() // the turn runs on: a2a-go runs it detached from its request
	if err := r.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := st.events(); !slices.Equal(got, []string{"cleanup " + string(id), "close"}) {
		t.Fatalf("the store's events = %q, want the turn cleaned up before the close", got)
	}
	task, err := st.Get(asCaller(t.Context(), auth.Caller), id)
	if err != nil || task.Task.Status.State != a2a.TaskStateWorking {
		t.Fatalf("the cut task = %+v, %v; want it left working", task, err)
	}
	for _, outcome := range []string{"canceled", "failed", "interrupted"} {
		if got := metricOf(t, r.tel, `agentkourier_kit_turns_total{outcome="`+outcome+`"}`); got != "" {
			t.Errorf("turns %s = %q, want none: the cut turn is the next leader's to count", outcome, got)
		}
	}
}

// stuck is a backend whose "hold" turn ignores its context's end until released.
type stuck struct {
	*scripted
	release chan struct{}
}

func (s stuck) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		s.started <- "stuck"
		<-s.release
		yield(nil, ctx.Err())
	}
}

// A turn that does not end when the stop cancels it holds the stop back for TurnStopTimeout at most.
func TestTheStopWaitsForATurnThatIgnoresItsCancelForTurnStopTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits TurnStopTimeout")
	}
	st := newFakeStore()
	b := stuck{scripted: &scripted{started: make(chan string, 1)}, release: make(chan struct{})}
	t.Cleanup(func() { close(b.release) })
	var logs syncBuffer
	r := startRun(t, testAddon(st, b), nil, Options{Log: slog.New(slog.NewJSONHandler(&logs, nil))})
	c := clientOf(t, r.base, fakeToken)
	ctx, hangUp := context.WithCancel(t.Context())
	go func() {
		for _, err := range c.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: message("m1", "x", "c1", "")}) {
			if err != nil {
				return
			}
		}
	}()
	<-b.started
	hangUp()
	began := time.Now()
	if err := r.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if took := time.Since(began); took < TurnStopTimeout || took > TurnStopTimeout+ShutdownTimeout {
		t.Fatalf("the stop took %s, want about TurnStopTimeout (%s)", took, TurnStopTimeout)
	}
	if !strings.Contains(logs.String(), `"msg":"stopped waiting for the turns still running","turn_stop_timeout":5000000000`) {
		t.Fatalf("the log = %s", logs.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A standby forwards a request through the runtime's own mux and middleware with the request and the answer in full
// duplex: the leader answers before it has read the body, and the standby goes on sending the body after.
func TestAStandbyForwardsInFullDuplexThroughItsMux(t *testing.T) {
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	}))
	t.Cleanup(leader.Close)
	st := newFakeStore()
	st.other = &store.Lease{Holder: "kit-a", Address: leader.URL, Live: true}
	r := startRun(t, testAddon(st, &scripted{}), map[string]string{"STORE": "postgres", "POSTGRES_DSN": "postgres://u:fake-password@db/kit",
		"ADVERTISE_URL": "http://127.0.0.1:1", "POD_NAME": "kit-b", "SQLITE_PATH": ""}, Options{})
	waitUntil(t, "the standby to be ready", func() bool { return status(r.base+"/readyz") == http.StatusOK })

	body, more := io.Pipe()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.base+A2APath, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fakeToken)
	// A connection kept alive: with Connection: close, net/http's server would not read what is left of the body as the
	// answer starts, and the test would pass without full duplex.
	keepAlive := &http.Transport{}
	t.Cleanup(keepAlive.CloseIdleConnections)
	answered := make(chan *http.Response, 1)
	failed := make(chan error, 1)
	go func() {
		resp, err := (&http.Client{Transport: keepAlive}).Do(req) //nolint:bodyclose // the test closes it once it has the answer
		if err != nil {
			failed <- err
			return
		}
		answered <- resp
	}()
	if _, err := more.Write([]byte("first,")); err != nil {
		t.Fatal(err)
	}
	var resp *http.Response
	select {
	case resp = <-answered:
	case err := <-failed:
		t.Fatalf("the forwarded request: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the leader's answer did not come back before the body ended")
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := more.Write([]byte("then the rest")); err != nil {
		t.Fatalf("the body after the answer began: %v", err)
	}
	_ = more.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != "first,then the rest" || resp.StatusCode != http.StatusOK {
		t.Fatalf("the answer = %d %q, %v", resp.StatusCode, got, err)
	}
	if got := metricOf(t, r.tel, `agentkourier_kit_forwarded_requests_total{outcome="forwarded"}`); got != "1" {
		t.Fatalf("forwarded = %q", got)
	}
}

// hooked is scripted with the hooks that shape the server: its own routes and readiness, and the listening line's
// attributes.
type hooked struct{ *scripted }

func (hooked) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/backend", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
}

func (hooked) Readyz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
}

func (hooked) LogAttrs() []any { return []any{"model", "fake-model"} }

func TestTheBackendsRoutesReadinessAndLogAttributesAreServed(t *testing.T) {
	var logs syncBuffer
	r := startRun(t, testAddon(newFakeStore(), hooked{&scripted{}}), nil, Options{Log: slog.New(slog.NewJSONHandler(&logs, nil))})
	for path, want := range map[string]int{"/backend": http.StatusTeapot, "/readyz": http.StatusAccepted, "/healthz": http.StatusOK,
		"/metrics": http.StatusOK, A2APath + CardPath: http.StatusUnauthorized} {
		if got := status(r.base + path); got != want {
			t.Errorf("%s = %d, want %d", path, got, want)
		}
	}
	line := logs.String()
	if !strings.Contains(line, `"msg":"agent listening","version":"1.2.3-test"`) || !strings.Contains(line, `"card":"http://kit.test/a2a/.well-known/agent-card.json","model":"fake-model","store":"sqlite"`) {
		t.Fatalf("the listening line = %s", line)
	}
}

// A takeover settles the turns a restart cut off before the leader serves, and tells the backend's Interrupter what it
// settled.
func TestATakeoverTellsTheInterrupterWhatItSettled(t *testing.T) {
	st := newFakeStore()
	st.settled = store.Settlement{Failed: []*a2a.Task{{ID: "t-cut", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}}}
	b := &hooks{scripted: &scripted{}, st: st, told: make(chan store.Settlement, 1)}
	r := startRun(t, testAddon(st, b), nil, Options{})
	waitUntil(t, "ready", func() bool { return status(r.base+"/readyz") == http.StatusOK })
	select {
	case got := <-b.told:
		if len(got.Failed) != 1 || got.Failed[0].ID != "t-cut" {
			t.Fatalf("the Interrupter was told %+v, want the settled task t-cut", got)
		}
	default:
		t.Fatal("the leader served before it told the backend what it settled")
	}
	wantMetric(t, r.tel, "agentkourier_kit_tasks_interrupted_total", "1")
}

// A takeover restores what the backend keeps before it settles the turns a restart cut off.
func TestATakeoverRestoresBeforeItSettles(t *testing.T) {
	st := newFakeStore()
	st.settle = func(context.Context) error { st.note("settle"); return nil }
	b := &hooks{scripted: &scripted{}, st: st, told: make(chan store.Settlement, 1)}
	r := startRun(t, testAddon(st, b), nil, Options{})
	waitUntil(t, "ready", func() bool { return status(r.base+"/readyz") == http.StatusOK })
	if got := st.events(); !slices.Equal(got, []string{"restore", "settle"}) {
		t.Fatalf("the takeover's events = %q, want the restore before the settlement", got)
	}
}

// A restore that fails stops the takeover: the replica never serves as the leader, settles nothing, and Run returns
// the restore's error.
func TestARestoreThatFailsStopsTheTakeover(t *testing.T) {
	st := newFakeStore()
	st.settle = func(context.Context) error { st.note("settle"); return nil }
	errRestore := errors.New("the backend's sessions cannot be read")
	b := &hooks{scripted: &scripted{}, st: st, told: make(chan store.Settlement, 1), proceed: make(chan struct{}), restoreErr: errRestore}
	r := startRun(t, testAddon(st, b), nil, Options{})
	if got := status(r.base + "/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz during the restore = %d, want 503", got)
	}
	close(b.proceed)
	var ready atomic.Bool
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		for range 50 {
			if status(r.base+"/readyz") == http.StatusOK {
				ready.Store(true)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	select {
	case err := <-r.done:
		if !errors.Is(err, errRestore) {
			t.Fatalf("Run = %v, want the restore's error", err)
		}
		r.done <- err
	case <-time.After(5 * time.Second):
		t.Fatal("Run went on after the restore failed")
	}
	<-polled
	if ready.Load() {
		t.Fatal("/readyz answered 200 after the restore failed")
	}
	if got := st.events(); slices.Contains(got, "settle") {
		t.Fatalf("the store's events = %q: the takeover settled after the restore failed", got)
	}
}

// hooks is scripted with a Restorer, which notes its restore in the store's events, waits for proceed when it is set,
// and returns restoreErr, and an Interrupter, which passes on what it is told.
type hooks struct {
	*scripted
	st         *fakeStore
	told       chan store.Settlement
	proceed    chan struct{}
	restoreErr error
}

func (h *hooks) Restore(context.Context) error {
	h.st.note("restore")
	if h.proceed != nil {
		<-h.proceed
	}
	return h.restoreErr
}

func (h *hooks) Interrupted(_ context.Context, settled store.Settlement) { h.told <- settled }
