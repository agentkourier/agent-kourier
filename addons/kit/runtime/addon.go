// Package runtime runs an add-on: an A2A agent in front of a backend, which the add-on supplies as an
// a2asrv.AgentExecutor with a few optional hooks. The kit owns the rest of the process: the settings (package
// settings), the telemetry, the caller check (package auth), the store (package store), the request handler with its
// replay of resent messages, the leader election and the standby's forwarding (package cluster), the leader's takeover
// (the settlement of the turns a restart or a takeover cut off, then the upkeep), and the order of the shutdown.
//
// With SQLite the add-on is the only replica and leads at once. With Postgres its replicas elect a leader through the
// store. The leader restores what the backend keeps (Restorer), fails the tasks a restart or a takeover cut off
// (Interrupter is told which), starts the store's upkeep, and only then serves A2A requests; a standby forwards them,
// and the card, to it, and settles and deletes nothing. A leader whose lease another replica takes, or that cannot
// renew it within LEASE_RENEW_DEADLINE, ends the process at once with cluster.ExitLeadershipLost; one that stops on its
// context hands the lease over once its requests have drained and its turns have stopped.
//
// /healthz, /readyz and /metrics are open; the A2A endpoint and its card are behind the caller check. /readyz answers
// 200 on a standby, which forwards, and on a leader once its takeover is done, until the add-on starts to stop.
package runtime

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
	"github.com/agentkourier/agent-kourier/addons/kit/cluster"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
	"github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// A2APath is the JSON-RPC endpoint's path; CardPath, the card's under it.
const (
	A2APath  = "/a2a"
	CardPath = "/.well-known/agent-card.json"
)

// ShutdownTimeout bounds the wait for the requests in flight at a graceful stop, and the flush of the spans after it;
// TurnStopTimeout the wait for the turns still running after it, which the stop cancels; ReleaseTimeout the hand-over of
// the lease after them. Together they stay well inside Kubernetes's default grace period of 30 s.
const (
	ShutdownTimeout = 5 * time.Second
	TurnStopTimeout = 5 * time.Second
	ReleaseTimeout  = 3 * time.Second
)

// ReadHeaderTimeout bounds how long a caller may take to send a request's headers before the server closes the
// connection.
const ReadHeaderTimeout = 10 * time.Second

// UpkeepInterval is how often the leader sweeps the store. A context can outlive SESSION_IDLE_TTL, and a task
// TASK_RETENTION, by up to this much.
const UpkeepInterval = time.Minute

// Addon is one add-on: its names, its settings, its store and the backend it builds. S is the add-on's store.
type Addon[S Store] struct {
	// Name is the add-on's, such as "ax": its metrics are agentkourier_<Name>_*, its spans agentkourier.<Name>.*, its
	// service agent-kourier-<Name>, its SQLite file <Name>-addon.db by default, and its standby's hop header
	// X-Agent-Kourier-<Name>-Forwarded. Required.
	Name string
	// Noun is what the add-on's messages call its process: "agent" makes "agent listening", "the agent is taking over as
	// the leader" and "Interrupted by an agent restart.". Required.
	Noun string
	// Version is the add-on's, which the card names and the log reports.
	Version string
	// Telemetry is the help of the metrics whose words are the add-on's, and the outcomes of its own turns. Its Name is
	// left empty: Run sets it to Name.
	Telemetry telemetry.Spec
	// Settings are the add-on's own variables, read before the shared ones (settings.Spec.Vars).
	Settings []settings.Var
	// OIDC, if set, makes OIDC_ISSUER, OIDC_AUDIENCE and OIDC_SUBJECT a caller check of their own: it builds the
	// verifier (package auth/oidc), which the kit does not link unless the add-on does.
	OIDC func(issuer, audience, subject string) (auth.Verifier, error)
	// Card is what the agent card says of the add-on.
	Card Card
	// Open opens the add-on's store on the configuration's SQLite file or Postgres database, applying its migrations.
	// Required.
	Open func(ctx context.Context, cfg settings.Config, opts StoreOptions) (S, error)
	// Build builds the backend once the store is open. Required.
	Build func(ctx context.Context, env Env[S]) (Backend, error)
}

// Card is what the agent card says of the add-on, beside the endpoint (PUBLIC_URL) and the version: text in and out,
// streaming.
type Card struct {
	Name, Description string
	// Extensions are the A2A extensions the add-on speaks, such as kagent's human-in-the-loop one.
	Extensions []a2a.AgentExtension
}

// StoreOptions are what the kit gives Open.
type StoreOptions struct {
	// Log receives what the store finds worth a warning.
	Log *slog.Logger
	// OnDeposed, on Postgres, runs when a write finds that another replica has taken the lease since this store took
	// it, before the write fails: the kit ends the process in it (cluster.FailStop).
	OnDeposed func()
}

// Store is what the kit needs of the add-on's store: the kit's task state, which an add-on's store embeds
// (store.Store), with the lease, and its close.
type Store interface {
	taskstore.Store
	cluster.Lease
	// MessageTask returns the call's caller's task that a message ID started or joined, or nil when none is recorded.
	MessageTask(ctx context.Context, messageID string) (*a2a.Task, error)
	// TouchContext records now as the last activity of the call's caller's context.
	TouchContext(ctx context.Context, contextID string) error
	// SettleInterrupted fails the tasks a restart or a takeover left submitted or working.
	SettleInterrupted(ctx context.Context, text string) (store.Settlement, error)
	// DeleteSettledBefore deletes the settled tasks last updated before cutoff.
	DeleteSettledBefore(ctx context.Context, cutoff time.Time) (int64, error)
	// DeleteIdleContexts deletes the contexts idle since before cutoff, with what the store keeps of them.
	DeleteIdleContexts(ctx context.Context, cutoff time.Time) (int, error)
	// IdleContexts lists the contexts idle since before cutoff, with no turn running.
	IdleContexts(ctx context.Context, cutoff time.Time) ([]store.Context, error)
	// ForgetContext removes an owner's context whose ref the add-on let go of, with its tasks from before cutoff.
	ForgetContext(ctx context.Context, owner, contextID, ref string, cutoff time.Time) error
	// Close closes the store.
	Close() error
}

var _ Store = (*store.Store)(nil)

// Env is what Build gets: the shared settings, the add-on's own (each Var reads its value with From), the open store,
// the telemetry, in which the backend registers its own metrics, and the log.
type Env[S Store] struct {
	Config    settings.Config
	Settings  settings.Values
	Store     S
	Telemetry *telemetry.Telemetry
	Log       *slog.Logger
}

// Backend runs the turns. The kit wraps it: each Execute is one turn, which it counts and traces, and ends when the
// add-on stops; a message whose ID the store has recorded joins its task and never reaches the backend. It may
// implement any of the hooks below, which the kit finds by type assertion: a backend that wraps another must expose
// the inner one's hooks itself, and a backend should assert each hook it means to implement
// (var _ runtime.Restorer = (*Backend)(nil)), so that a changed signature fails the build rather than drop the hook.
type Backend interface {
	a2asrv.AgentExecutor
}

// Restorer is a backend that reconciles what it keeps outside the store when this replica takes over as the leader,
// before the settlement of the interrupted turns (cma: its sessions in CMA). Its error stops the takeover.
type Restorer interface {
	Restore(ctx context.Context) error
}

// Interrupter is a backend that is told which tasks the settlement of a takeover failed, once it has (cma: it sends
// their sessions an interrupt). It runs before the leader serves.
type Interrupter interface {
	Interrupted(ctx context.Context, settled store.Settlement)
}

// ResumeValidator is a backend that checks a message for a paused task (input-required) before the turn starts. Its
// error refuses the request with a2a.ErrInvalidParams and leaves the task paused: an executor's error would fail it.
type ResumeValidator interface {
	ValidateResume(ctx context.Context, paused *a2a.Task, msg *a2a.Message) error
}

// ContextReleaser is a backend that keeps a context outside the store (cma: a session in CMA). For each context idle
// past SESSION_IDLE_TTL with no turn running, the upkeep calls ReleaseContext, then forgets the context and its tasks
// (store.Store.ForgetContext). An error leaves the context for the next sweep, and is logged at WARN unless it wraps
// ErrContextBusy. Without it, the upkeep deletes idle contexts in the store's own transaction
// (store.Store.DeleteIdleContexts, with store.TaskOptions.ReleaseContext).
type ContextReleaser interface {
	ReleaseContext(ctx context.Context, c store.Context) error
}

// ErrContextBusy is what a ContextReleaser's error wraps when a turn of the context has begun since the store listed
// it as idle: the store shows a turn only once a2a-go has stored its task, and a message that arrives during a sweep
// can start one in between. The upkeep leaves the context, which the message made active, and logs it at DEBUG.
var ErrContextBusy = errors.New("a turn of the context has begun")

// IdleReporter is a backend that is told, after each sweep without a ContextReleaser, how many idle contexts the store
// deleted and the error that stopped the sweep, if any (ax: it counts the ADK sessions deleted).
type IdleReporter interface {
	IdleContextsDeleted(n int, err error)
}

// TurnClassifier is a backend whose failed turns have outcomes of their own (cma: timeout). FailedOutcome returns the
// outcome of a turn that ended failed with status, or "" for telemetry.TurnFailed.
type TurnClassifier interface {
	FailedOutcome(status a2a.TaskStatus) telemetry.TurnOutcome
}

// Routes is a backend that serves routes of its own beside the kit's (a test control).
type Routes interface {
	Routes(mux *http.ServeMux)
}

// Readiness is a backend whose readiness is another server's (ax beside the AX task runner): a non-nil Readyz serves
// /readyz instead of the kit's.
type Readiness interface {
	Readyz() http.Handler
}

// Describer is a backend whose settings the "listening" log line reports, as key and value pairs.
type Describer interface {
	LogAttrs() []any
}
