package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/cluster"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// Options are what Run takes beside the add-on and its settings.
type Options struct {
	// Log receives the add-on's log. Nil is slog.Default().
	Log *slog.Logger
	// Telemetry is the add-on's metrics, which /metrics serves, and its tracer, built from TelemetrySpec. Nil is one of
	// Run's own that records no spans.
	Telemetry *telemetry.Telemetry
	// Routes, if set, adds routes to the server's mux before it serves (a test's).
	Routes func(*http.ServeMux)
	// Listening, if set, is told the address the add-on listens on, once it does: before its takeover, if it leads, is
	// done.
	Listening func(net.Addr)
	// Exit ends the process with cluster.ExitLeadershipLost when this replica stops leading while it runs. Nil is
	// os.Exit. If it returns (a test's), Run goes on until the election finds the lease lost, then stops as on a
	// failure and returns cluster.ErrLeadershipLost.
	Exit func(code int)
	// UpkeepInterval is how often the leader sweeps the store. Zero is UpkeepInterval (a test's is shorter).
	UpkeepInterval time.Duration
	// Now is the upkeep's clock. Nil is time.Now.
	Now func() time.Time
}

// TelemetrySpec is the add-on's telemetry spec: Telemetry, named Name.
func (a Addon[S]) TelemetrySpec() telemetry.Spec {
	spec := a.Telemetry
	spec.Name = a.Name
	return spec
}

// check reports what makes a unusable: a missing name, noun, Open or Build, or a telemetry spec named otherwise.
func (a Addon[S]) check() error {
	switch {
	case a.Name == "" || a.Noun == "":
		return errors.New("runtime: the add-on's Name and Noun are required")
	case a.Open == nil || a.Build == nil:
		return errors.New("runtime: the add-on's Open and Build are required")
	case a.Telemetry.Name != "" && a.Telemetry.Name != a.Name:
		return fmt.Errorf("runtime: the add-on's telemetry is named %q, not %q: leave its Name empty", a.Telemetry.Name, a.Name)
	}
	return nil
}

// InterruptedMessage is the agent message of a task that a restart or a takeover failed: "Interrupted by an agent
// restart." for the noun "agent".
func InterruptedMessage(noun string) string {
	article := "a"
	if noun != "" && strings.ContainsAny(noun[:1], "aeiouAEIOU") {
		article = "an"
	}
	return "Interrupted by " + article + " " + noun + " restart."
}

// Run serves the add-on that a and its settings describe until ctx ends (it then returns nil, after a graceful stop
// and, if it leads, the release of the lease) or a dependency fails. own are the add-on's own settings, as Parse read
// them.
func Run[S Store](ctx context.Context, a Addon[S], cfg settings.Config, own settings.Values, opts Options) error {
	if err := a.check(); err != nil {
		return err
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	tel := opts.Telemetry
	if tel == nil {
		var err error
		if tel, err = telemetry.New(a.TelemetrySpec(), nil); err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
	}
	exit := opts.Exit
	if exit == nil {
		exit = os.Exit
	}
	check, watch, err := callerCheck(a, cfg, log, tel)
	if err != nil {
		return err
	}
	lost := &cluster.FailStop{Log: log, Exit: exit}
	st, err := a.Open(ctx, cfg, StoreOptions{Log: log, OnDeposed: lost.Deposed})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	backend, err := a.Build(ctx, Env[S]{Config: cfg, Settings: own, Store: st, Telemetry: tel, Log: log})
	if err != nil {
		return err
	}
	if undeclared := own.Undeclared(); len(undeclared) > 0 {
		return fmt.Errorf("the %s read settings it does not declare: %s", a.Noun, strings.Join(undeclared, ", "))
	}

	// The turns run on life, which ends once the requests have drained, before the lease is released and the store
	// closed; from then a2a-go's writes of a task are refused (tasks), so that a turn cut short is left for the next
	// leader to fail as interrupted.
	life, stopTurns := context.WithCancel(context.WithoutCancel(ctx))
	defer stopTurns()
	running := newTurns(life, backend, tel)
	tasks := &guardedTasks{Store: st}
	local := endpoint(a, backend, st, tasks, running, check, cfg.PublicURL, tel)

	status := &cluster.Status{}
	router := &cluster.Router{Local: telemetry.Extract(local), Status: status, Addon: a.Name, Noun: a.Noun,
		RetryAfter: cfg.Lease.RetryPeriod, Telemetry: tel, Log: log}
	var stopping atomic.Bool
	var readyz http.Handler
	if r, ok := backend.(Readiness); ok {
		readyz = r.Readyz()
	}
	mux := newMux(router, readyz, func() bool { return !stopping.Load() && status.Ready() })
	mux.Handle("/metrics", tel.Handler())
	if r, ok := backend.(Routes); ok {
		r.Routes(mux)
	}
	if opts.Routes != nil {
		opts.Routes(mux)
	}

	var elector *cluster.Elector
	if cfg.Store == settings.StorePostgres {
		elector = &cluster.Elector{Lease: st, Holder: cfg.PodName, Address: cfg.AdvertiseURL, LeaseDuration: cfg.Lease.Duration,
			RenewDeadline: cfg.Lease.RenewDeadline, RetryPeriod: cfg.Lease.RetryPeriod, Status: status, Telemetry: tel, Log: log}
		router.Leaders = elector
		// One try at the lease before listening: a replica that takes it leads from its first answer, so /readyz cannot
		// answer 200 before its takeover is done; one that does not is a standby, ready at once, and campaigns in Run.
		elector.TryLeadOnce(ctx)
	} else {
		// The only replica leads before it listens, so /readyz cannot answer 200 before its takeover is done.
		status.Lead()
		tel.SetLeader(true)
	}
	release := func() {
		if elector == nil {
			return
		}
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ReleaseTimeout)
		defer cancel()
		if err := elector.Release(releaseCtx); err != nil {
			log.Warn("hand the lease over; a standby takes it once it expires", "error", err)
		}
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		release()
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: ReadHeaderTimeout, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	served := make(chan error, 1)
	go func() { served <- httpSrv.Serve(ln) }()
	// The background work ends when Run returns, whether or not ctx has ended, and before the store closes.
	var background sync.WaitGroup
	defer background.Wait()
	backgroundCtx, stopBackground := context.WithCancel(ctx)
	defer stopBackground()
	if watch != nil {
		background.Go(func() { watch(backgroundCtx) })
	}
	if opts.Listening != nil {
		opts.Listening(ln.Addr())
	}
	attrs := []any{"version", a.Version, "addr", ln.Addr().String(), "card", cfg.PublicURL + CardPath}
	if d, ok := backend.(Describer); ok {
		attrs = append(attrs, d.LogAttrs()...)
	}
	log.Info(a.Noun+" listening", append(attrs, "store", cfg.Store, "pod", cfg.PodName, "auth", authMode(cfg),
		"auth_header", cfg.AuthHeader, "task_retention", cfg.TaskRetention, "session_idle_ttl", cfg.SessionIdleTTL)...)

	up := &upkeep{store: st, retention: cfg.TaskRetention, idle: cfg.SessionIdleTTL, every: opts.UpkeepInterval, now: opts.Now, log: log}
	up.releaser, _ = backend.(ContextReleaser)
	up.reporter, _ = backend.(IdleReporter)
	if up.every <= 0 {
		up.every = UpkeepInterval
	}
	if up.now == nil {
		up.now = time.Now
	}
	restorer, _ := backend.(Restorer)
	interrupter, _ := backend.(Interrupter)
	interrupted := InterruptedMessage(a.Noun)
	// takeOver is the leader's work, once it holds the lease: restore what the backend keeps, fail the turns a restart or
	// a takeover cut off, start the upkeep, then serve. On Postgres no other replica serves turns from the store by then:
	// the lease's epoch fences every write of the one that held it. It returns when ctx ends, once the upkeep has
	// stopped, or with the error that stopped the takeover.
	takeOver := func(ctx context.Context) error {
		if restorer != nil {
			if err := restorer.Restore(ctx); err != nil {
				return err
			}
		}
		settled, err := st.SettleInterrupted(ctx, interrupted)
		tel.TasksInterrupted(settled.Interrupted()) // each in a write of its own: those failed before an error stay failed
		if err != nil {
			return err
		}
		if interrupter != nil {
			interrupter.Interrupted(ctx, settled)
		}
		if settled.Interrupted() > 0 || settled.Skipped > 0 {
			log.Info("failed the turns a restart or a takeover cut off", "tasks", settled.Interrupted(),
				"unreadable", len(settled.Unreadable), "skipped", settled.Skipped)
		}
		var swept sync.WaitGroup
		swept.Go(func() { up.run(ctx) })
		status.Serve()
		log.Info("serving A2A requests as the leader")
		<-ctx.Done()
		swept.Wait()
		return nil
	}
	// The election outlives ctx until the server has stopped and the turns have, so that the lease is renewed while
	// they end and a standby does not take over while this replica still writes.
	electionCtx, stopElection := context.WithCancel(context.WithoutCancel(ctx))
	defer stopElection()
	led := make(chan error, 1)
	go func() {
		if elector == nil {
			led <- takeOver(electionCtx)
			return
		}
		led <- elector.Run(electionCtx, takeOver)
	}()

	var failure error
	ledDone := false
	select {
	case <-ctx.Done():
	case err := <-served:
		failure = fmt.Errorf("serve: %w", err)
	case err := <-led:
		ledDone = true
		if errors.Is(err, cluster.ErrLeadershipLost) {
			lost.LeaseLost()
		}
		failure = err
	}
	stopping.Store(true)
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(drain); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("stop serving", "error", err)
	}
	// The turns still running are cut short: their tasks stay as they are, for the next leader to fail.
	tasks.stop()
	stopTurns()
	if !running.wait(TurnStopTimeout) {
		log.Warn("stopped waiting for the turns still running", "turn_stop_timeout", TurnStopTimeout)
	}
	stopElection()
	if !ledDone {
		// The election's context has ended, so Run, or the takeover, returns once the leader's work (the upkeep) has
		// stopped, before the lease is released and the store closed; a takeover it cut short says so.
		if err := <-led; err != nil {
			log.Info("stopped before the takeover was done", "error", err)
		}
	}
	release()
	return failure
}

// Handler is the A2A endpoint at A2APath and its card under it, behind check, as Run serves them on the leader, and
// nothing else of Run: no election, takeover, upkeep or stop, and each turn runs until it ends. publicURL is the
// endpoint the card names; tel may be nil. It serves a backend in a test or a smoke run.
func (a Addon[S]) Handler(backend Backend, st Store, check *auth.Check, publicURL string, tel *telemetry.Telemetry) http.Handler {
	return endpoint(a, backend, st, st, newTurns(context.Background(), backend, tel), check, publicURL, tel)
}

// endpoint is the A2A endpoint at A2APath and its card under it, behind check. The turns run through running, and
// a2a-go writes the tasks through tasks.
func endpoint[S Store](a Addon[S], backend Backend, st Store, tasks taskstore.Store, running *turns, check *auth.Check, publicURL string,
	tel *telemetry.Telemetry) http.Handler {
	validator, _ := backend.(MessageValidator)
	handler := &a2asrv.InterceptedHandler{
		Handler: &replayHandler{RequestHandler: a2asrv.NewHandler(running, a2asrv.WithTaskStore(tasks)), store: st, tel: tel,
			validator: validator, life: running.life},
		Interceptors: []a2asrv.CallInterceptor{auth.Interceptor{}},
	}
	mux := http.NewServeMux()
	mux.Handle(A2APath+CardPath, check.Wrap(a2asrv.NewStaticAgentCardHandler(card(a, publicURL))))
	mux.Handle(A2APath, check.Wrap(a2asrv.NewJSONRPCHandler(handler)))
	return mux
}

// card is the add-on's agent card: its endpoint, publicURL, and its version.
func card[S Store](a Addon[S], publicURL string) *a2a.AgentCard {
	return &a2a.AgentCard{Name: a.Card.Name, Description: a.Card.Description, Version: a.Version,
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(publicURL, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		Capabilities: a2a.AgentCapabilities{Streaming: true, Extensions: a.Card.Extensions}}
}

// newMux serves a2a, the A2A endpoint and its card, with /healthz and /readyz. /readyz is readyz when it is set, and
// otherwise answers 200 while ready says so and 503 otherwise.
func newMux(a2a, readyz http.Handler, ready func() bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(A2APath, a2a)
	mux.Handle(A2APath+CardPath, a2a)
	// Alive once it listens: it stays cheap, and a takeover that takes long does not fail it.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	if readyz != nil {
		mux.Handle("/readyz", readyz)
		return mux
	}
	// Ready on a standby, which forwards, and on a leader once its takeover is done, until the replica starts to stop.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// callerCheck builds the check the settings name, which counts each refusal in tel, and for the token file the watch
// that keeps its tokens current.
func callerCheck[S Store](a Addon[S], cfg settings.Config, log *slog.Logger, tel *telemetry.Telemetry) (*auth.Check, func(context.Context), error) {
	refused := func(reason auth.Reason) { tel.AuthRefused(string(reason)) }
	switch {
	case cfg.AuthDisabled:
		log.Warn("the caller check is off (AUTH_DISABLED=true): anything that reaches the loopback address can run the agent", "addr", cfg.ListenAddr)
		return &auth.Check{Verifier: auth.Open{}, Log: log, Refused: refused}, nil, nil
	case cfg.OIDC():
		if a.OIDC == nil {
			return nil, nil, errors.New("OIDC: the add-on has no OIDC check")
		}
		v, err := a.OIDC(cfg.OIDCIssuer, cfg.OIDCAudience, cfg.OIDCSubject)
		if err != nil {
			return nil, nil, fmt.Errorf("OIDC: %w", err)
		}
		return &auth.Check{Verifier: v, Bearer: true, Log: log, Refused: refused}, nil, nil
	default:
		tokens, err := auth.LoadTokens(cfg.AuthTokenFile, log)
		if err != nil {
			return nil, nil, fmt.Errorf("AUTH_TOKEN_FILE: %w", err)
		}
		check := &auth.Check{Verifier: auth.TokenVerifier{Header: cfg.AuthHeader, Tokens: tokens},
			Bearer: strings.EqualFold(cfg.AuthHeader, "Authorization"), Log: log, Refused: refused}
		return check, func(ctx context.Context) { tokens.Watch(ctx, auth.ReloadInterval) }, nil
	}
}

func authMode(cfg settings.Config) string {
	switch {
	case cfg.AuthDisabled:
		return "disabled"
	case cfg.OIDC():
		return "oidc"
	}
	return "token-file"
}
