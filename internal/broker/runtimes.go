package broker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
)

// connRuntime is what one served ChatConnection has of its own (spec D51): its chat platform, the run of its Socket Mode connection
// (RunPlatform) with the intake it feeds, and the watch of its posts for a rejected credential (WatchAuth). Everything else of the leader
// half is shared by the connections and finds the platform here, by connection (runtimeTable).
type connRuntime struct {
	name string // "namespace/name"
	ref  v1alpha1.ObjectRef
	// id is whose bot it is, guarded by the table's lock (runtimeTable.identify): the own connection's is learned after it runs.
	id       appIdentity
	platform builtPlatform
	// report is WatchAuth's: a post, a lookup or a reaction that finds the tokens rejected ends ctx with ErrSlackAuth as the cause.
	report func(code string)
	// ctx is RunPlatform's, and ends when halt is called or report is.
	ctx  context.Context
	stop context.CancelFunc
	// halted says the runtime is being stopped on purpose, by halt, so its end is no failure and its platform stays for the final drain.
	halted atomic.Bool
	// done is closed when RunPlatform has returned, and err is its result, set before. Both are set by run: a runtime that never ran has neither.
	done chan struct{}
	err  error
	// closeOnce makes closing the transport once, whoever asks.
	closeOnce sync.Once
}

// halt ends the run of the runtime on purpose and does not wait for it (wait does).
func (rt *connRuntime) halt() {
	rt.halted.Store(true)
	rt.stop()
}

// wait waits for RunPlatform to return, or for ctx. A runtime that never ran has nothing to wait for.
func (rt *connRuntime) wait(ctx context.Context) {
	if rt.done != nil {
		waitFor(ctx, rt.done)
	}
}

// runtimeTable is the connections that run: the Resolver of the parts every connection shares, which look the platform up by
// the connection of their row, turn or press. A connection that is not in it is down (chat.ErrConnectionDown), whether it is not served,
// has not started yet, was taken away or stopped. It is safe for concurrent use.
type runtimeTable struct {
	mu  sync.RWMutex
	rts map[string]*connRuntime
}

func newRuntimeTable() *runtimeTable { return &runtimeTable{rts: map[string]*connRuntime{}} }

var _ chat.Resolver[chat.Platform] = (*runtimeTable)(nil)

// Platform implements chat.Resolver: the platform of the connection conn as the broker uses it (the fenced one, with an election).
func (t *runtimeTable) Platform(conn string) (chat.Platform, error) {
	t.mu.RLock()
	rt, ok := t.rts[conn]
	t.mu.RUnlock()
	if !ok {
		return nil, downError(conn)
	}
	return rt.platform.Platform, nil
}

// downError is the error of a connection that nothing runs.
func downError(conn string) error { return fmt.Errorf("%w: %q", chat.ErrConnectionDown, conn) }

// outboxPlatforms is the table as the outbox sees it: the platform of each connection with its posts watched for a rejected credential
// (WatchAuth), so that a token revoked while the connection is up ends that connection's run.
type outboxPlatforms struct{ t *runtimeTable }

// Platform implements chat.Resolver.
func (o outboxPlatforms) Platform(conn string) (outbox.Platform, error) {
	o.t.mu.RLock()
	rt, ok := o.t.rts[conn]
	o.t.mu.RUnlock()
	if !ok {
		return nil, downError(conn)
	}
	return WatchAuth(rt.platform, rt.report), nil
}

// outbox is the table as the outbox's Resolver.
func (t *runtimeTable) outbox() chat.Resolver[outbox.Platform] { return outboxPlatforms{t} }

// add registers rt as the runtime of its connection.
func (t *runtimeTable) add(rt *connRuntime) {
	t.mu.Lock()
	t.rts[rt.name] = rt
	t.mu.Unlock()
}

// remove takes rt out of the table, if it is the connection's runtime still: a runtime that was replaced is not in it to take.
func (t *runtimeTable) remove(rt *connRuntime) {
	t.mu.Lock()
	if t.rts[rt.name] == rt {
		delete(t.rts, rt.name)
	}
	t.mu.Unlock()
}

// identify records whose bot the runtime of conn is, which is learned after it was built for the own connection.
func (t *runtimeTable) identify(conn string, id appIdentity) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rt, ok := t.rts[conn]; ok {
		rt.id = id
	}
}

// botOwner implements botOwners over the connections that run.
func (t *runtimeTable) botOwner(botID string) (string, bool) {
	if botID == "" {
		return "", false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for name, rt := range t.rts {
		if rt.id.Bot == botID {
			return name, true
		}
	}
	return "", false
}

// all returns the runtimes in the table, in no order.
func (t *runtimeTable) all() []*connRuntime {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*connRuntime, 0, len(t.rts))
	for _, rt := range t.rts {
		out = append(out, rt)
	}
	return out
}

// runtimeHost builds and runs the runtimes of the leader half, the own connection's and the supervisor's.
type runtimeHost struct {
	cfg     RunConfig
	log     *slog.Logger
	metrics *Metrics
	table   *runtimeTable
	// open builds the platform of a connection (leaderHalf.openPlatform).
	open func(conn Connection, onConnection func(connected bool)) (builtPlatform, error)
	// parent is the context the runs hang from: the broker's own, which only the shutdown ends.
	parent context.Context
}

// build makes the runtime of conn, whose bot is id, and puts it in the table: from here the shared parts reach its platform, and the other
// connections tell its bot. agentkourier_chat_connected says 0 for it from here, and 1 while its connection is up. It runs nothing (run does),
// so that the own connection's platform exists before the parts it feeds are built.
func (h *runtimeHost) build(conn Connection, id appIdentity) (*connRuntime, error) {
	name := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	h.metrics.ChatConnected(name, false)
	p, err := h.open(conn, func(up bool) { h.metrics.ChatConnected(name, up) })
	if err != nil {
		return nil, err
	}
	ctx, report, stop := WithAuthFailure(h.parent)
	rt := &connRuntime{name: name, ref: conn.Ref, id: id, platform: p, report: report, ctx: ctx, stop: stop}
	h.table.add(rt)
	return rt, nil
}

// run starts the run of rt on sink, on a goroutine of its own. onEnd, if set, is called with what RunPlatform returned once it has. When the
// runtime was not halted the run ended by itself, which for RunPlatform is a rejected credential: the runtime leaves the table then, so the
// connection is down. A halted one stays until its owner takes it out, because the final drain of a shutdown still posts through it.
func (h *runtimeHost) run(rt *connRuntime, sink chat.Sink, onEnd func(rt *connRuntime, err error)) {
	rt.done = make(chan struct{})
	go func() {
		err := RunPlatform(rt.ctx, rt.platform, sink, RunPolicy{Logger: h.log})
		rt.err = err
		if !rt.halted.Load() {
			h.table.remove(rt)
			h.metrics.ChatConnected(rt.name, false)
		}
		close(rt.done)
		if onEnd != nil {
			onEnd(rt, err)
		}
	}()
}

// closeTransport closes the idle connections of rt's Slack transport, once, and tells the seam. It is for after the last call through the
// platform: one that a later call opens closes when idle (the transport's own timeout), and nothing closes it sooner.
func (h *runtimeHost) closeTransport(rt *connRuntime) {
	rt.closeOnce.Do(func() {
		rt.platform.closeIdle()
		h.metrics.ChatConnected(rt.name, false)
		if h.cfg.transportClosed != nil {
			h.cfg.transportClosed(rt.name)
		}
	})
}

// haltAll ends the run of every runtime in the table and waits for them to return, or for ctx. The runtimes stay in the table.
func (h *runtimeHost) haltAll(ctx context.Context) {
	all := h.table.all()
	for _, rt := range all {
		rt.halt()
	}
	for _, rt := range all {
		rt.wait(ctx)
	}
}

// closeAll closes the transport of every runtime in the table.
func (h *runtimeHost) closeAll() {
	for _, rt := range h.table.all() {
		h.closeTransport(rt)
	}
}

// runtimeStopTimeout bounds the wait for the run of a team connection that the supervisor takes away or restarts to return. Past it the run
// is left to end on its own (it has been told to stop) and the supervisor goes on.
const runtimeStopTimeout = 5 * time.Second

// stopRuntime takes rt out of the table, so that the connection is down at once, ends its run, waits for it for runtimeStopTimeout, and closes
// its transport. It is for a team connection that goes or restarts while the broker runs.
func (h *runtimeHost) stopRuntime(ctx context.Context, rt *connRuntime) {
	h.table.remove(rt)
	rt.halt()
	wctx, cancel := context.WithTimeout(ctx, runtimeStopTimeout)
	defer cancel()
	rt.wait(wctx)
	if rt.done != nil && wctx.Err() != nil && !isClosed(rt.done) {
		h.log.WarnContext(ctx, "chat connection: its run did not stop in time; it was told to stop and is left to end", "connection", rt.name, "waited", runtimeStopTimeout)
	}
	h.closeTransport(rt)
}

// isClosed says whether done is closed.
func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
