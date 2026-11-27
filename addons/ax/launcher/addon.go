package launcher

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

const timedOut telemetry.TurnOutcome = "timed_out"

const legacyQuestion = "The AX add-on was upgraded. Send a new message to start an AX Task."

// Addon supplies the AX launcher to the shared A2A runtime.
func Addon(version string) runtime.Addon[*store.Store] {
	return runtime.Addon[*store.Store]{
		Name: "ax", Noun: "agent", Version: version, Settings: vars(),
		Card: runtime.Card{Name: "Agent Kourier AX", Description: "Runs agent commands in AX Tasks."},
		Telemetry: telemetry.Spec{
			Calls: "ax", TurnsHelp: "AX launcher turns by outcome.",
			AuthRefusedHelp: "Refused AX A2A requests by reason.", ErrorTurns: []telemetry.TurnOutcome{timedOut},
		},
		Open: func(ctx context.Context, cfg settings.Config, opts runtime.StoreOptions) (*store.Store, error) {
			options := store.Options{Log: opts.Log, OnDeposed: opts.OnDeposed}
			if cfg.Store == settings.StorePostgres {
				return store.OpenPostgres(ctx, cfg.PostgresDSN, options)
			}
			return store.OpenSQLite(ctx, cfg.SQLitePath, options)
		},
		Build: build,
	}
}

type launcher struct {
	cfg      config
	st       *store.Store
	client   ax.AXClient
	conn     *grpc.ClientConn
	template *ax.Task
	log      *slog.Logger
	token    [32]byte
	now      func() time.Time

	life context.Context
	stop context.CancelFunc

	admitted atomic.Int64
	active   sync.Map // task ID -> struct{} while an AX launch call can still run
	waiters  sync.Map // task ID -> a wakeup channel while following the Task
	launchMu sync.Mutex
	launches sync.WaitGroup

	rpc        *prometheus.HistogramVec
	callbacks  *prometheus.CounterVec
	resultTime prometheus.Histogram
	trimmed    prometheus.Counter
	cleanup    prometheus.Gauge
}

var (
	_ runtime.MessageValidator = (*launcher)(nil)
	_ runtime.MessageAdmission = (*launcher)(nil)
	_ runtime.CancelAdmission  = (*launcher)(nil)
	_ runtime.LeaderRoutes     = (*launcher)(nil)
	_ runtime.LeaderWorker     = (*launcher)(nil)
	_ runtime.Restorer         = (*launcher)(nil)
	_ runtime.TurnClassifier   = (*launcher)(nil)
)

func build(ctx context.Context, env runtime.Env[*store.Store]) (runtime.Backend, error) {
	cfg := readConfig(env.Settings)
	token, err := os.ReadFile(cfg.tokenFile)
	if err != nil || len(token) == 0 || len(token) > 4096 ||
		strings.ContainsAny(string(token), " \t\r\n,") || string(token) == "ax-placeholder" {
		return nil, errors.New("AX_RESULT_TOKEN_FILE must contain one dedicated bearer without whitespace")
	}
	if env.Config.AuthTokenFile != "" {
		a2aTokens, err := auth.LoadTokens(env.Config.AuthTokenFile, env.Log)
		if err != nil {
			return nil, errors.New("cannot check A2A token file")
		}
		if a2aTokens.Accepts(string(token)) {
			return nil, errors.New("AX callback and A2A credentials must differ")
		}
	}
	conn, err := grpc.NewClient(cfg.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("open AX control plane client: %w", err)
	}
	e := &launcher{
		cfg: cfg, client: ax.NewAXClient(conn), conn: conn, st: env.Store, log: env.Log,
		now: time.Now, token: sha256.Sum256(token),
		rpc: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "agentkourier_ax_rpc_duration_seconds", Help: "AX RPC duration by method and gRPC status.",
		}, []string{"method", "code"}),
		callbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agentkourier_ax_callback_total", Help: "AX callback requests by route and response status.",
		}, []string{"route", "status"}),
		resultTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "agentkourier_ax_result_latency_seconds", Help: "Time from turn start to the first committed AX result.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 900, 1800},
		}),
		trimmed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "agentkourier_ax_history_trimmed_total", Help: "Earlier turns dropped from AX prompts.",
		}),
		cleanup: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "agentkourier_ax_cleanup_pending", Help: "AX Task launches with pending cleanup.",
		}),
	}
	e.template, err = readTemplate(ctx, cfg, e.client)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	for _, collector := range []prometheus.Collector{e.rpc, e.callbacks, e.resultTime, e.trimmed, e.cleanup} {
		if err := env.Telemetry.Register(collector); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	// Run closes the backend after draining requests and turns. Keep launch
	// work alive during that drain, then cancel and join it before lease release.
	e.life, e.stop = context.WithCancel(context.WithoutCancel(ctx))
	return e, nil
}

func (e *launcher) Close() error {
	e.launchMu.Lock()
	e.stop()
	e.launchMu.Unlock()
	err := e.conn.Close()
	e.launches.Wait()
	return err
}

func (e *launcher) LeaderRoutes(register func(string, http.Handler)) {
	handler := result.Handler{Token: func() [32]byte { return e.token }, Receipts: e, Observe: func(start bool, status int) {
		route := "result"
		if start {
			route = "start"
		}
		e.callbacks.WithLabelValues(route, strconv.Itoa(status)).Inc()
	}}
	register(result.Path, handler)
	register(result.StartPath, handler)
}

func (e *launcher) Claim(ctx context.Context, claim result.Claim) (int, error) {
	return e.st.Claim(ctx, claim)
}

func (e *launcher) Report(ctx context.Context, report result.Report, hash [32]byte) (int, error) {
	status, launch, err := e.st.AcceptResult(ctx, report, hash)
	if launch != nil {
		started := time.Unix(0, launch.Deadline).Add(-e.cfg.timeout)
		e.resultTime.Observe(max(0, e.now().Sub(started).Seconds()))
		e.wake(launch.TaskID)
	}
	return status, err
}

func (e *launcher) wake(id string) {
	if ch, ok := e.waiters.Load(id); ok {
		select {
		case ch.(chan struct{}) <- struct{}{}:
		default:
		}
	}
}

func (e *launcher) AdmitMessage(_ context.Context, msg *a2a.Message) (func(), error) {
	if msg.TaskID != "" {
		return nil, nil // the SDK refuses a new message for a settled task
	}
	for {
		count := e.admitted.Load()
		if count >= int64(e.cfg.running) {
			return nil, runtime.ErrBusy
		}
		if e.admitted.CompareAndSwap(count, count+1) {
			return func() { e.admitted.Add(-1) }, nil
		}
	}
}

func (e *launcher) AdmitCancel(ctx context.Context, task *a2a.Task) error {
	accepted, err := e.st.RequestCancel(ctx, string(task.ID))
	if err != nil {
		return errors.New("could not record AX cancellation")
	}
	if !accepted {
		return a2a.ErrTaskNotCancelable
	}
	return nil
}

func (e *launcher) Restore(ctx context.Context) error {
	if err := e.st.Recover(ctx, e.cfg.keep); err != nil {
		return err
	}
	_, err := e.st.SettlePaused(ctx, legacyQuestion)
	return err
}

func (e *launcher) FailedOutcome(s a2a.TaskStatus) telemetry.TurnOutcome {
	if s.Message != nil && s.Message.Metadata["code"] == "DEADLINE_EXCEEDED" {
		return timedOut
	}
	return ""
}
