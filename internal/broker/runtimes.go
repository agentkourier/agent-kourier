package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
)

// connRuntime is what one served ChatConnection has of its own (spec D52): its chat platform, the run of its Socket Mode connection
// (RunPlatform) with the intake it feeds, and the watch of its calls for a rejected credential (WatchAuth). Everything else of the leader
// half is shared by the connections and finds the platform here, by connection (runtimeTable).
type connRuntime struct {
	name string // "namespace/name"
	// conn is the connection as it was started: its credentials are the ones this runtime runs on.
	conn     Connection
	platform builtPlatform
	// watched is the platform as every shared part gets it (runtimeTable.Platform): watched for a rejected credential (WatchAuth), so that a
	// call of any of them that finds the tokens rejected ends the run.
	watched chat.Platform
	// report is WatchAuth's: a call that finds the tokens rejected ends ctx with an authRejected, which is ErrSlackAuth, as the cause.
	report func(code string)
	// gauge is what agentkourier_chat_connected of the connection is set through.
	gauge *connGauge
	// ctx is RunPlatform's, and ends when halt is called or report is.
	ctx  context.Context
	stop context.CancelFunc
	// halted says the runtime is being stopped on purpose, by halt, so its end is no failure and its platform stays for the final drain.
	halted atomic.Bool
	// done is closed when RunPlatform has returned. It is made by build, so that whoever halts the runtime sees it whenever it ran; ran says
	// run was called, since a runtime that never ran has nothing to wait for.
	done chan struct{}
	ran  atomic.Bool
	// closeOnce makes closing the transport once, whoever asks.
	closeOnce sync.Once
	// life is the span of the connection being served that the runtime belongs to, set by the supervisor for a team connection. Once the span is
	// retired the platform of the runtime fails fast (stopped). A runtime that is replaced by a restart is in the same span: its turns go on with
	// the platform they hold, which is the connection's still. Nil for the own connection, which is always served.
	life atomic.Pointer[connLife]
}

// halt ends the run of the runtime on purpose and does not wait for it (wait does).
func (rt *connRuntime) halt() {
	rt.halted.Store(true)
	rt.stop()
}

// rejection says whether the platform rejected the credentials of the runtime, and the code it did so with: a call or the run saw it, and ended ctx
// with it. A runtime that was halted on purpose has not been rejected.
func (rt *connRuntime) rejection() (code string, rejected bool) {
	var ar *authRejected
	if errors.As(context.Cause(rt.ctx), &ar) {
		return ar.code, true
	}
	return "", false
}

// connLife is a span of a connection being served, shared by the runtimes it has in it. It is retired when the connection stops being served.
type connLife struct{ retired atomic.Bool }

// stopped is the error a call through the runtime's platform fails with once it can no longer be used, and nil while it can: the platform
// rejected the credentials (a *connectionStopped, from the call that found out), or the connection is not served any more (retired: down). A
// runtime that was only replaced by a restart, or halted by the shutdown, is neither: the turns that hold its platform finish with it, and the
// final drain posts through it.
func (rt *connRuntime) stopped() error {
	if code, rejected := rt.rejection(); rejected {
		return &connectionStopped{conn: rt.name, rejected: true, code: code}
	}
	if l := rt.life.Load(); l != nil && l.retired.Load() {
		return downError(rt.name)
	}
	return nil
}

// connGauge sets agentkourier_chat_connected of one runtime, and stops doing so once the runtime is retired: a late report of the adapter's
// connection (it calls back from its own goroutines) must not bring back the series of a connection that was stopped, replaced or removed.
type connGauge struct {
	mu      sync.Mutex
	m       *Metrics
	name    string
	retired bool
}

// set is the adapter's OnConnection.
func (g *connGauge) set(up bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.retired {
		g.m.ChatConnected(g.name, up)
	}
}

// retire says the connection is down for good, and ignores every report after it.
func (g *connGauge) retire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.retired = true
	g.m.ChatConnected(g.name, false)
}

// wait waits for RunPlatform to return, or for ctx. A runtime that never ran has nothing to wait for.
func (rt *connRuntime) wait(ctx context.Context) {
	if rt.ran.Load() {
		waitFor(ctx, rt.done)
	}
}

// runtimeTable is the connections that run: the Resolver of the parts every connection shares, which look the platform up by
// the connection of their row, turn or press. A connection that is not in it is down (chat.ErrConnectionDown), whether it is not served,
// has not started yet, was taken away or stopped. It is safe for concurrent use.
type runtimeTable struct {
	mu  sync.RWMutex
	rts map[string]*connRuntime
	// down says why a connection that is not in rts cannot run until something changes: the platform rejected its credentials, or its credentials
	// Secret cannot be used. A connection with no entry is down without a reason this table knows (it has not started yet, or is not served).
	down map[string]*connectionStopped
	// bots is whose bot each served connection has (appIdentity), apart from rts: a connection that restarts has no runtime while its new tokens
	// are being asked about, and the turns that hold its old platform still post as its old bot (until they finish, even if the new tokens turn
	// out to be another app's, whose bot then replaces the old). The supervisor writes it (identify, forgetBot); the bot guard of every
	// connection reads it (botOwner, botUserOwner).
	//
	// The invariant, after every look of the supervisor: a team connection has its bot here while it is served with an identity that is known
	// (the entry's id), whether or not it runs; the own connection has its own once that identity is known (a look returns before then).
	// A connection that is refused, or that is not in the config, has none. One whose new tokens fail the lookup keeps the bot of its last
	// known identity, whether Slack rejected them, did not answer, or answered for good (a missing scope, a token that belongs to no bot): the
	// old runtime and the turns that hold its platform may still post as that bot, and a post of the broker's own bot that the guard did not
	// know would be the loop it exists to stop. A refusal that lifts writes the bot back, since the lookup that learned it is not asked again;
	// a restart that carries its id on writes nothing, because a connection that is held was never forgotten.
	bots map[string]appIdentity
	// sealed is set by the shutdown when it halts the runtimes: a runtime added after it is halted as it is added, so that none runs on
	// beyond the step that stops them.
	sealed bool
}

func newRuntimeTable() *runtimeTable {
	return &runtimeTable{rts: map[string]*connRuntime{}, down: map[string]*connectionStopped{}, bots: map[string]appIdentity{}}
}

var _ chat.Resolver[chat.Platform] = (*runtimeTable)(nil)

// Platform implements chat.Resolver: the platform of the connection conn as the broker uses it (the fenced one, with an election), watched for a
// rejected credential (WatchAuth). A connection that does not run has none, and the error wraps chat.ErrConnectionDown; when the table knows why
// (the platform rejected its credentials, or its Secret cannot be used) the error is a *connectionStopped that says so. A runtime whose credentials
// were rejected is down from the call that found out, before its run has ended and left the table.
func (t *runtimeTable) Platform(conn string) (chat.Platform, error) {
	t.mu.RLock()
	rt, ok := t.rts[conn]
	why := t.down[conn]
	t.mu.RUnlock()
	switch {
	case !ok && why != nil:
		return nil, why
	case !ok:
		return nil, downError(conn)
	}
	if code, rejected := rt.rejection(); rejected {
		return nil, &connectionStopped{conn: conn, rejected: true, code: code}
	}
	return rt.watched, nil
}

// runs says whether the connection conn runs: it is in the table, and its credentials have not been found rejected.
func (t *runtimeTable) runs(conn string) bool {
	_, err := t.Platform(conn)
	return err == nil
}

// downError is the error of a connection that nothing runs.
func downError(conn string) error { return fmt.Errorf("%w: %q", chat.ErrConnectionDown, conn) }

// outbox is the table as the outbox's Resolver. The platform it gives is the one every part gets: watched.
func (t *runtimeTable) outbox() chat.Resolver[outbox.Platform] {
	return chat.Narrow[outbox.Platform](t)
}

// fail takes rt out of the table because its run ended by itself with err, and records why if err is a credential the platform rejected: the
// connection is down from here, with that reason, until the supervisor clears it (clearDown) or a runtime of the connection is added. A runtime
// that was replaced is not the connection's to take out.
func (t *runtimeTable) fail(rt *connRuntime, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rts[rt.name] != rt {
		return
	}
	delete(t.rts, rt.name)
	if code, ok := rejectionCode(err); ok {
		t.down[rt.name] = &connectionStopped{conn: rt.name, rejected: true, code: code}
	}
}

// setDown records why the connection conn does not run (see down).
func (t *runtimeTable) setDown(conn string, why *connectionStopped) {
	t.mu.Lock()
	t.down[conn] = why
	t.mu.Unlock()
}

// clearDown forgets why conn does not run.
func (t *runtimeTable) clearDown(conn string) {
	t.mu.Lock()
	delete(t.down, conn)
	t.mu.Unlock()
}

// clearUnusable forgets that the Secret of conn cannot be used, and leaves a rejection of its credentials, which only a change of them mends.
func (t *runtimeTable) clearUnusable(conn string) {
	t.mu.Lock()
	if d := t.down[conn]; d != nil && !d.rejected {
		delete(t.down, conn)
	}
	t.mu.Unlock()
}

// reasons says why each connection that does not run is down, as short words (the code of a rejection, or "unusable"), for a caller that wants to
// know whether anything changed.
func (t *runtimeTable) reasons() map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]string, len(t.down))
	for conn, d := range t.down {
		out[conn] = d.reason()
	}
	return out
}

// add registers rt as the runtime of its connection, and says whether it did: a sealed table (the broker is stopping) halts rt and refuses it.
func (t *runtimeTable) add(rt *connRuntime) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed {
		rt.halt()
		return false
	}
	t.rts[rt.name] = rt
	delete(t.down, rt.name) // it runs: whatever kept the connection down is over
	return true
}

// seal refuses every runtime from now on and returns those that are in the table.
func (t *runtimeTable) seal() []*connRuntime {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sealed = true
	out := make([]*connRuntime, 0, len(t.rts))
	for _, rt := range t.rts {
		out = append(out, rt)
	}
	return out
}

// remove takes rt out of the table, if it is the connection's runtime still: a runtime that was replaced is not in it to take.
func (t *runtimeTable) remove(rt *connRuntime) {
	t.mu.Lock()
	if t.rts[rt.name] == rt {
		delete(t.rts, rt.name)
	}
	t.mu.Unlock()
}

// identify records whose bot the connection conn has, in place of the one it had: a connection that is served keeps its bot until a new one is
// learned (a restart on other tokens) or forgetBot is called, whether or not it runs now.
func (t *runtimeTable) identify(conn string, id appIdentity) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bots[conn] = id
}

// forgetBot forgets the bot of conn: it is not served any more (it left the Snapshot, or is refused as a second connection of a Slack app).
func (t *runtimeTable) forgetBot(conn string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.bots, conn)
}

// botOwner implements botOwners over the connections that are served.
func (t *runtimeTable) botOwner(botID string) (string, bool) {
	if botID == "" {
		return "", false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for name, id := range t.bots {
		if id.Bot == botID {
			return name, true
		}
	}
	return "", false
}

// botUserOwner implements botOwners for the user a bot posts as.
func (t *runtimeTable) botUserOwner(user string) (string, bool) {
	if user == "" {
		return "", false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for name, id := range t.bots {
		if id.User == user {
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

// build makes the runtime of conn and puts it in the table: from here the shared parts reach its platform. agentkourier_chat_connected says 0 for
// it from here, and 1 while its connection is up. It runs nothing (run does), so that the own connection's platform exists before the parts it feeds are built.
func (h *runtimeHost) build(conn Connection) (*connRuntime, error) {
	name := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	h.metrics.ChatConnected(name, false)
	gauge := &connGauge{m: h.metrics, name: name}
	p, err := h.open(conn, gauge.set)
	if err != nil {
		return nil, err
	}
	ctx, report, stop := WithAuthFailure(h.parent)
	rt := &connRuntime{name: name, conn: conn, platform: p, report: report, gauge: gauge, ctx: ctx, stop: stop, done: make(chan struct{})}
	rt.watched = watchRuntime(p.Platform, report, rt.stopped)
	if !h.table.add(rt) {
		h.closeTransport(rt)
		return nil, errStopping
	}
	return rt, nil
}

// run starts the run of rt on sink, on a goroutine of its own. onEnd, if set, is called with what RunPlatform returned once it has. When the
// runtime was not halted the run ended by itself, which for RunPlatform is a rejected credential, whether the run or a call through the platform
// found it out: the runtime leaves the table then, so the connection is down, with the rejection as the reason (runtimeTable.fail), and its
// transport closes. A halted one stays until its owner takes it out, because the final drain of a shutdown still posts through it.
func (h *runtimeHost) run(rt *connRuntime, sink chat.Sink, onEnd func(rt *connRuntime, err error)) {
	rt.ran.Store(true)
	go func() {
		err := RunPlatform(rt.ctx, rt.platform, sink, RunPolicy{Logger: h.log})
		if !rt.halted.Load() {
			h.table.fail(rt, err)
			h.closeTransport(rt) // nothing posts through it from here: the connection is down
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
		rt.gauge.retire()
		if h.cfg.transportClosed != nil {
			h.cfg.transportClosed(rt.name)
		}
	})
}

// haltAll ends the run of every runtime in the table and waits for them to return, or for ctx. The runtimes stay in the table, which takes no
// more: one that is added after this is halted as it is.
func (h *runtimeHost) haltAll(ctx context.Context) {
	all := h.table.seal()
	for _, rt := range all {
		rt.halt()
	}
	for _, rt := range all {
		rt.wait(ctx)
	}
}

// closeAll closes the transport of every runtime in the table (closeTransport is once for each; a run that ended by itself closed its own).
func (h *runtimeHost) closeAll() {
	for _, rt := range h.table.all() {
		h.closeTransport(rt)
	}
}

// connectionNames are the names ("namespace/name") of conns.
func connectionNames(conns []Connection) []string {
	out := make([]string, 0, len(conns))
	for _, c := range conns {
		out = append(out, v1alpha1.NamespacedName(c.Ref.Namespace, c.Ref.Name))
	}
	return out
}

// errStopping is the error of building a runtime while the broker is stopping.
var errStopping = errors.New("the broker is stopping")

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
	if rt.ran.Load() && wctx.Err() != nil && !isClosed(rt.done) {
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
